// Per-project skills list (MACH-51): edits the skills.project.<project>
// setting and previews the composed list. The setting holds the same document
// as a repository .machinist/skills.yaml, so the editor accepts JSON (what it
// writes) and the documented YAML subset (what an operator may have pasted).
// Composition mirrors internal/skills/resolve.go: project layer, then global
// layer, de-duplicated by name with the first mention winning, and an entry
// with enabled: false removing an inherited one.

export function splitSkillName(value) {
  const text = String(value ?? "").trim();
  const index = text.lastIndexOf("@");
  if (index > 0) return { name: text.slice(0, index).trim(), version: text.slice(index + 1).trim() };
  return { name: text, version: "" };
}

// skillKey mirrors internal/skills.Key: case-folded with colons folded to
// underscores, so `feature-dev:code-reviewer` and `feature-task:code-reviewer`
// compare consistently.
export function skillKey(name) {
  return String(name ?? "").trim().toLowerCase().replaceAll(":", "_");
}

function stringList(value) {
  if (!Array.isArray(value)) return [];
  return value.map((item) => String(item ?? "").trim().toLowerCase()).filter(Boolean);
}

function normalizeEntry(entry, libraryNames) {
  const source = entry && typeof entry === "object" ? entry : {};
  const name = String(source.name ?? "").trim();
  return {
    name,
    appliesTo: stringList(source.applies_to ?? source.appliesTo),
    required: Boolean(source.required),
    rule: typeof source.rule === "string" ? source.rule : "",
    enabled: source.enabled === undefined || source.enabled === null ? undefined : Boolean(source.enabled),
    known: libraryNames ? libraryNames.has(skillKey(splitSkillName(name).name)) : true,
  };
}

function parseScalar(raw) {
  const value = String(raw ?? "").trim();
  if (value.startsWith("[") && value.endsWith("]")) {
    return value
      .slice(1, -1)
      .split(",")
      .map((item) => item.trim().replace(/^["']|["']$/g, ""))
      .filter(Boolean);
  }
  if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
    return value.slice(1, -1);
  }
  if (value === "true") return true;
  if (value === "false") return false;
  return value;
}

function assignEntryField(entry, key, raw) {
  switch (key) {
    case "name":
      entry.name = String(parseScalar(raw));
      break;
    case "applies_to":
      entry.applies_to = parseScalar(raw);
      break;
    case "required":
      entry.required = parseScalar(raw) === true;
      break;
    case "enabled":
      entry.enabled = parseScalar(raw) === true;
      break;
    case "rule":
      entry.rule = String(parseScalar(raw));
      break;
    default:
      break;
  }
}

// parseYamlEntries reads the documented sequence-of-mappings subset: a list
// item (`- name: x`) with indented `key: value` fields beneath it.
function parseYamlEntries(raw) {
  const entries = [];
  let current = null;
  for (const line of raw.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const item = /^-\s*(.*)$/.exec(trimmed);
    if (item) {
      current = {};
      entries.push(current);
      const rest = item[1].trim();
      if (rest) {
        const field = /^([A-Za-z_]+)\s*:\s*(.*)$/.exec(rest);
        if (field) assignEntryField(current, field[1], field[2]);
      }
      continue;
    }
    if (!current) continue;
    const field = /^([A-Za-z_]+)\s*:\s*(.*)$/.exec(trimmed);
    if (field) assignEntryField(current, field[1], field[2]);
  }
  return entries;
}

function listFromDocument(document) {
  if (Array.isArray(document)) return document;
  if (document && typeof document === "object" && Array.isArray(document.skills)) return document.skills;
  return null;
}

// parseSkillsSetting turns a stored setting value into editable entries. A
// value that is neither JSON nor the YAML subset yields no entries.
export function parseSkillsSetting(value, libraryNames) {
  const raw = typeof value === "string" ? value.trim() : "";
  if (!raw) return [];
  let list = null;
  try {
    list = listFromDocument(JSON.parse(raw));
  } catch {
    list = null;
  }
  if (list === null) list = parseYamlEntries(raw);
  if (list === null) return [];
  return list.map((entry) => normalizeEntry(entry, libraryNames)).filter((entry) => entry.name);
}

// serializeSkillsSetting writes the JSON document stored in the setting. rule
// and enabled are carried through so re-saving an operator's YAML does not drop
// fields the editor does not manage.
export function serializeSkillsSetting(entries) {
  const skills = (entries || [])
    .filter((entry) => String(entry?.name ?? "").trim())
    .map((entry) => {
      const output = {
        name: String(entry.name).trim(),
        applies_to: stringList(entry.appliesTo),
        required: Boolean(entry.required),
      };
      if (entry.rule) output.rule = entry.rule;
      if (entry.enabled !== undefined) output.enabled = entry.enabled;
      return output;
    });
  return JSON.stringify({ skills });
}

// unknownSkillNames lists the entry names that are not in the installed
// library. Repository-bundled skills are not in the library, so this is a
// warning, not a hard error.
export function unknownSkillNames(entries, libraryNames) {
  const known = libraryNames instanceof Set
    ? libraryNames
    : new Set((libraryNames || []).map((item) => skillKey(typeof item === "string" ? item : item?.name)));
  const unknown = [];
  for (const entry of entries || []) {
    const { name } = splitSkillName(entry?.name);
    if (!name) continue;
    if (!known.has(skillKey(name)) && !unknown.includes(name)) unknown.push(name);
  }
  return unknown;
}

// appliesToKind mirrors internal/skills.resolve: an empty applies_to or kind
// matches everything.
export function appliesToKind(entry, kind) {
  if (!kind || !entry?.appliesTo?.length) return true;
  const target = String(kind).toLowerCase();
  return entry.appliesTo.map((value) => value.toLowerCase()).includes(target);
}

// composeSkills previews the resolved list for a run kind: the project layer
// wins over the global layer, de-duplicated by name, with each entry's version
// taken from an explicit name@version or the installed active version.
export function composeSkills({ projectEntries = [], globalEntries = [], kind = "", skills = [] } = {}) {
  const library = new Map((skills || []).map((skill) => [skillKey(skill.name), skill]));
  const resolved = [];
  const warnings = [];
  const seen = new Set();
  for (const [scope, entries] of [["project", projectEntries], ["global", globalEntries]]) {
    for (const entry of entries || []) {
      const { name, version } = splitSkillName(entry?.name);
      if (!name) continue;
      const key = skillKey(name);
      if (seen.has(key)) continue;
      seen.add(key);
      if (entry.enabled === false) continue;
      if (!appliesToKind(entry, kind)) continue;
      const installed = library.get(key);
      const resolvedVersion = version || installed?.active || "";
      const status = installed ? "installed" : entry.required ? "required" : "missing";
      resolved.push({
        name,
        version: resolvedVersion,
        scope,
        required: Boolean(entry.required),
        appliesTo: entry.appliesTo || [],
        status,
      });
      if (!installed) {
        warnings.push(
          entry.required
            ? `required skill "${entry.name}" is not installed`
            : `skill "${entry.name}" is not installed; skipping`,
        );
      }
    }
  }
  return { skills: resolved, warnings };
}