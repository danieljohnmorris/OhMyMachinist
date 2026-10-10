import assert from "node:assert/strict";
import test from "node:test";
import { groupJobsByProject, resolveJobProject } from "./project.js";

const job = (id, title, updated, metadata) => ({ id, github_issue_title: title, updated_at: updated, metadata });

test("job projects use stored project first, then title, then Other", () => {
  assert.equal(resolveJobProject({ metadata: { project: "OMM" }, github_issue_title: "MACH-5: Other change" }), "OMM");
  assert.equal(resolveJobProject({ github_issue_title: "OMM-3: Sidebar links" }), "OMM");
  assert.equal(resolveJobProject({ github_issue_title: "SPARK-1: Spark" }), "SPARK");
  assert.equal(resolveJobProject({ prompt: "No project" }), "Other");
  assert.equal(resolveJobProject({ github_issue_title: "lower-1: No" }), "Other");
});

test("project stacks use recent activity and preserve jobs in each stack", () => {
  const groups = groupJobsByProject([
    job("untitled", "No key", "2026-01-03T00:00:00Z"),
    job("mach_new", "MACH-6: Sidebar", "2026-01-05T00:00:00Z"),
    job("mach_old", "MACH-5: Run page", "2026-01-04T00:00:00Z"),
    job("omm", "OMM-3: Links", "2026-01-02T00:00:00Z"),
    job("stored", "Ignored title", "2026-01-01T00:00:00Z", { project: "SPARK" }),
  ]);
  assert.deepEqual(groups.map(([project]) => project), ["MACH", "Other", "OMM", "SPARK"]);
  assert.deepEqual(groups[0][1].map(({ id }) => id), ["mach_new", "mach_old"]);
  assert.deepEqual(groups[1][1].map(({ id }) => id), ["untitled"]);
});

test("empty project stacks are omitted", () => {
  assert.deepEqual(groupJobsByProject([]), []);
});
