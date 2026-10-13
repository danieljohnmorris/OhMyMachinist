import assert from "node:assert/strict";
import test from "node:test";
import {
  backlogProjectOptions,
  backlogStateLabel,
  backlogStates,
  createIssuePayload,
  filterBacklogIssues,
  groupIssuesByState,
  issueUpdatePayload,
  startRunPayload,
} from "./backlog-state.js";

test("the five backlog states have stable labels", () => {
  assert.deepEqual(backlogStates, ["backlog", "todo", "started", "completed", "cancelled"]);
  assert.equal(backlogStateLabel("started"), "Started");
  assert.equal(backlogStateLabel("cancelled"), "Cancelled");
  assert.equal(backlogStateLabel("mystery"), "mystery");
  assert.equal(backlogStateLabel(""), "unknown");
});

test("filterBacklogIssues filters by project and matches key, title, and spec text", () => {
  const issues = [
    { id: "1", project: "OMM", key: "OMM-1", title: "Add sidebar links", spec: "Link tickets." },
    { id: "2", project: "SPARK", key: "SPARK-6", title: "Cluster metrics", spec: "" },
  ];
  assert.deepEqual(filterBacklogIssues(issues, { project: "SPARK" }).map(({ id }) => id), ["2"]);
  assert.deepEqual(filterBacklogIssues(issues, { query: "spark-6" }).map(({ id }) => id), ["2"]);
  assert.deepEqual(filterBacklogIssues(issues, { query: "SIDEBAR" }).map(({ id }) => id), ["1"]);
  assert.deepEqual(filterBacklogIssues(issues, { query: "tickets" }).map(({ id }) => id), ["1"]);
  assert.equal(filterBacklogIssues(issues, { project: "OMM", query: "cluster" }).length, 0);
  assert.equal(filterBacklogIssues(issues).length, 2);
});

test("groupIssuesByState buckets every state and parks unknown states in backlog", () => {
  const grouped = groupIssuesByState([
    { id: "1", state: "todo" },
    { id: "2", state: "started" },
    { id: "3", state: "started" },
    { id: "4", state: "something_new" },
  ]);
  assert.deepEqual(
    Object.fromEntries(Object.entries(grouped).map(([state, bucket]) => [state, bucket.map(({ id }) => id)])),
    { backlog: ["4"], todo: ["1"], started: ["2", "3"], completed: [], cancelled: [] },
  );
});

test("backlogProjectOptions merges known projects with repositories", () => {
  assert.deepEqual(
    backlogProjectOptions([{ project: "SPARK" }, { project: "OMM" }, { project: " " }], ["OMM", "blog"]),
    ["OMM", "SPARK", "blog"],
  );
  assert.deepEqual(backlogProjectOptions([], []), []);
});

test("createIssuePayload trims fields and omits empty optionals", () => {
  assert.deepEqual(createIssuePayload({ project: " OMM ", title: " Fix it ", spec: "  ", url: "" }), {
    project: "OMM",
    title: "Fix it",
  });
  assert.deepEqual(createIssuePayload({ project: "OMM", title: "Fix it", spec: " Spec ", url: " https://example.com " }), {
    project: "OMM",
    title: "Fix it",
    spec: "Spec",
    url: "https://example.com",
  });
});

test("issueUpdatePayload carries only provided fields and valid states", () => {
  assert.deepEqual(issueUpdatePayload({ title: " New title " }), { title: "New title" });
  assert.deepEqual(issueUpdatePayload({ spec: "" }), { spec: "" });
  assert.deepEqual(issueUpdatePayload({ state: "started" }), { state: "started" });
  assert.deepEqual(issueUpdatePayload({ state: "bogus", title: "  " }), {});
  assert.deepEqual(issueUpdatePayload(), {});
});

test("startRunPayload trims, prefers workflow over command, and omits empty optionals", () => {
  assert.deepEqual(startRunPayload({ repository: " oma/om ", workflow: " ship ", model: " glm " }), {
    repository: "oma/om",
    workflow: "ship",
    model: "glm",
  });
  assert.deepEqual(startRunPayload({ repository: "oma/om", command: " deploy " }), {
    repository: "oma/om",
    command: "deploy",
  });
  assert.deepEqual(startRunPayload({ repository: "oma/om", workflow: "ship", command: "deploy" }), {
    repository: "oma/om",
    workflow: "ship",
  });
  assert.deepEqual(startRunPayload({ repository: "oma/om" }), { repository: "oma/om" });
});
