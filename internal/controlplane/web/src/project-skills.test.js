import assert from "node:assert/strict";
import test from "node:test";
import {
  appliesToKind,
  composeSkills,
  parseSkillsSetting,
  serializeSkillsSetting,
  skillKey,
  splitSkillName,
  unknownSkillNames,
} from "./project-skills.js";

const library = [
  { name: "feature-dev:code-reviewer", active: "abc123" },
  { name: "rust-review", active: "def456" },
];

test("splitSkillName separates an explicit version", () => {
  assert.deepEqual(splitSkillName("rust-review@abc"), { name: "rust-review", version: "abc" });
  assert.deepEqual(splitSkillName("rust-review"), { name: "rust-review", version: "" });
  assert.deepEqual(splitSkillName("  rust-review@  "), { name: "rust-review", version: "" });
});

test("skillKey folds colons and case like the server", () => {
  assert.equal(skillKey("Feature-Dev:Code-Reviewer"), "feature-dev_code-reviewer");
});

test("parseSkillsSetting reads the editor JSON document", () => {
  const entries = parseSkillsSetting(JSON.stringify({ skills: [{ name: "rust-review", applies_to: ["review"], required: true }] }));
  assert.deepEqual(entries, [
    { name: "rust-review", appliesTo: ["review"], required: true, rule: "", enabled: undefined, known: true },
  ]);
});

test("parseSkillsSetting reads the documented YAML subset and keeps rule/enabled", () => {
  const entries = parseSkillsSetting(
    [
      "# a comment",
      "skills:",
      "  - name: feature-dev:code-reviewer",
      "  - name: rust-review",
      "    applies_to: [review]",
      "    rule: Check unsafe blocks.",
      "    required: true",
      "  - name: docs-reviewer",
      "    enabled: false",
    ].join("\n"),
  );
  assert.equal(entries.length, 3);
  assert.equal(entries[0].name, "feature-dev:code-reviewer");
  assert.deepEqual(entries[1].appliesTo, ["review"]);
  assert.equal(entries[1].rule, "Check unsafe blocks.");
  assert.equal(entries[1].required, true);
  assert.equal(entries[2].enabled, false);
});

test("parseSkillsSetting ignores empty and unreadable values", () => {
  assert.deepEqual(parseSkillsSetting(""), []);
  assert.deepEqual(parseSkillsSetting("   "), []);
});

test("serializeSkillsSetting round-trips an entry, preserving rule and enabled", () => {
  const entries = parseSkillsSetting("skills:\n  - name: rust-review\n    applies_to: [build, review]\n    required: true\n    rule: Check.\n");
  const saved = serializeSkillsSetting(entries);
  assert.deepEqual(JSON.parse(saved), {
    skills: [{ name: "rust-review", applies_to: ["build", "review"], required: true, rule: "Check." }],
  });
  const reparsed = parseSkillsSetting(saved);
  assert.equal(reparsed[0].name, "rust-review");
  assert.deepEqual(reparsed[0].appliesTo, ["build", "review"]);
  assert.equal(reparsed[0].required, true);
});

test("serializeSkillsSetting drops nameless entries", () => {
  assert.deepEqual(JSON.parse(serializeSkillsSetting([{ name: "  " }, { name: "rust-review" }])), {
    skills: [{ name: "rust-review", applies_to: [], required: false }],
  });
});

test("unknownSkillNames flags names absent from the library", () => {
  const entries = parseSkillsSetting(JSON.stringify({ skills: [{ name: "rust-review" }, { name: "not-installed" }, { name: "feature-dev:code-reviewer" }] }));
  assert.deepEqual(unknownSkillNames(entries, library), ["not-installed"]);
});

test("appliesToKind matches an empty filter and known kinds", () => {
  assert.equal(appliesToKind({ appliesTo: ["review"] }, ""), true);
  assert.equal(appliesToKind({ appliesTo: ["review"] }, "review"), true);
  assert.equal(appliesToKind({ appliesTo: ["review"] }, "build"), false);
  assert.equal(appliesToKind({ appliesTo: [] }, "build"), true);
});

test("composeSkills resolves project then global with versions", () => {
  const project = parseSkillsSetting(JSON.stringify({ skills: [{ name: "rust-review", applies_to: ["review"], required: true }] }));
  const global = parseSkillsSetting(JSON.stringify({ skills: [{ name: "feature-dev:code-reviewer" }, { name: "missing-skill" }] }));
  const { skills, warnings } = composeSkills({ projectEntries: project, globalEntries: global, kind: "review", skills: library });
  assert.deepEqual(
    skills.map((entry) => [entry.name, entry.version, entry.scope, entry.status]),
    [
      ["rust-review", "def456", "project", "installed"],
      ["feature-dev:code-reviewer", "abc123", "global", "installed"],
      ["missing-skill", "", "global", "missing"],
    ],
  );
  assert.deepEqual(warnings, ['skill "missing-skill" is not installed; skipping']);
});

test("composeSkills honours a pinned version, kind filter, disable and de-dupe", () => {
  const project = parseSkillsSetting(JSON.stringify({ skills: [{ name: "rust-review@aaa" }] }));
  const global = parseSkillsSetting(JSON.stringify({ skills: [{ name: "rust-review" }, { name: "feature-dev:code-reviewer", applies_to: ["build"] }, { name: "docs-reviewer", enabled: false }] }));
  const { skills } = composeSkills({ projectEntries: project, globalEntries: global, kind: "review", skills: library });
  assert.deepEqual(skills.map((entry) => entry.name), ["rust-review"]);
  assert.equal(skills[0].version, "aaa", "an explicit pinned version wins over the active version");

  const build = composeSkills({ projectEntries: [], globalEntries: global, kind: "build", skills: library });
  assert.deepEqual(build.skills.map((entry) => entry.name), ["rust-review", "feature-dev:code-reviewer"]);
});

test("composeSkills marks a missing required skill as required", () => {
  const project = parseSkillsSetting(JSON.stringify({ skills: [{ name: "missing-skill", required: true }] }));
  const { skills, warnings } = composeSkills({ projectEntries: project, globalEntries: [], kind: "", skills: library });
  assert.equal(skills[0].status, "required");
  assert.deepEqual(warnings, ['required skill "missing-skill" is not installed']);
});