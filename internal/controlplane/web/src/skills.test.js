import assert from "node:assert/strict";
import test from "node:test";
import { activeVersion, installSkill, loadSkills, pinSkill, shortVersion, skillInstalledAt, skillNames, skillUpdatedAt, skillsFromPayload, uninstallSkill, updateSkill, uploadSkill } from "./skills.js";

const payload = {
  skills: [
    {
      name: "feature-dev:code-reviewer",
      source: "https://github.com/owner/repo/tree/main/skills/reviewer",
      active: "bbb",
      versions: [
        { version: "aaa", resolved: "aaa", installed_at: "2026-01-01T00:00:00Z", scanned: true },
        { version: "bbb", resolved: "bbb", installed_at: "2026-02-01T00:00:00Z", scanned: true },
      ],
      used_by: ["acme-widgets", "shipped review profile"],
    },
  ],
};

test("payload parsing keeps names, versions and references", () => {
  const [skill] = skillsFromPayload(payload);
  assert.equal(skill.name, "feature-dev:code-reviewer");
  assert.equal(skill.active, "bbb");
  assert.equal(skill.versions.length, 2);
  assert.deepEqual(skill.usedBy, ["acme-widgets", "shipped review profile"]);
  assert.equal(activeVersion(skill).version, "bbb");
  assert.equal(skillInstalledAt(skill), "2026-02-01T00:00:00Z");
  assert.equal(skillUpdatedAt(skill), "2026-02-01T00:00:00Z");
  assert.deepEqual(skillNames([skill]), ["feature-dev:code-reviewer"]);
});

test("payload parsing tolerates missing and malformed fields", () => {
  assert.deepEqual(skillsFromPayload(undefined), []);
  assert.deepEqual(skillsFromPayload({ skills: "nope" }), []);
  const [skill] = skillsFromPayload({ skills: [{ name: 7, versions: "nope", used_by: [1, "x"] }] });
  assert.equal(skill.name, "");
  assert.deepEqual(skill.versions, []);
  assert.deepEqual(skill.usedBy, ["x"]);
  assert.equal(activeVersion(skill), null);
  assert.equal(skillInstalledAt(skill), "");
  assert.equal(skillUpdatedAt(skill), "");
});

test("shortVersion trims long identities only", () => {
  assert.equal(shortVersion("0123456789abcdef"), "0123456789ab");
  assert.equal(shortVersion("abc"), "abc");
  assert.equal(shortVersion(undefined), "");
});
test("loadSkills reads the list endpoint", async (t) => {
  t.mock.method(globalThis, "fetch", async (url, options = {}) => {
    assert.equal(String(url), "/api/v1/skills");
    assert.equal(options.headers.Accept, "application/json");
    return { ok: true, json: async () => payload };
  });
  const skills = await loadSkills();
  assert.equal(skills.length, 1);
  assert.equal(skills[0].active, "bbb");
});

test("skill actions call the documented endpoints", async (t) => {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (url, options = {}) => {
    calls.push({ url: String(url), method: options.method || "GET", headers: options.headers || {}, body: options.body });
    return { ok: true, json: async () => ({}) };
  });
  await installSkill("https://example.com/skill", "csrf");
  await updateSkill("feature-dev:code-reviewer", "csrf");
  await pinSkill("feature-dev:code-reviewer", "abc", "csrf");
  await uninstallSkill("feature-dev:code-reviewer", true, "csrf");
  await uninstallSkill("docs-reviewer", false, "csrf");

  assert.deepEqual(calls.map((call) => [call.method, call.url]), [
    ["POST", "/api/v1/skills"],
    ["POST", "/api/v1/skills/feature-dev%3Acode-reviewer/update"],
    ["POST", "/api/v1/skills/feature-dev%3Acode-reviewer/pin"],
    ["DELETE", "/api/v1/skills/feature-dev%3Acode-reviewer?force=true"],
    ["DELETE", "/api/v1/skills/docs-reviewer"],
  ]);
  assert.deepEqual(JSON.parse(calls[0].body), { source: "https://example.com/skill" });
  assert.equal(calls[0].headers["X-Machinist-CSRF"], "csrf");
  assert.deepEqual(JSON.parse(calls[1].body), { activate: true });
  assert.deepEqual(JSON.parse(calls[2].body), { version: "abc" });
});

test("uploadSkill posts a multipart file part", async (t) => {
  let captured;
  t.mock.method(globalThis, "fetch", async (url, options = {}) => {
    captured = { url: String(url), options };
    return { ok: true, json: async () => ({ name: "rust-review" }) };
  });
  await uploadSkill(new Blob(["x"], { type: "text/markdown" }), "csrf");
  assert.equal(captured.url, "/api/v1/skills/upload");
  assert.equal(captured.options.method, "POST");
  assert.equal(captured.options.headers["X-Machinist-CSRF"], "csrf");
  assert.ok(captured.options.body instanceof FormData);
  assert.ok(captured.options.body.get("file"));
});
