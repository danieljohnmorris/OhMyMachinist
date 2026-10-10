import assert from "node:assert/strict";
import test from "node:test";
import { ISSUE_TRACKERS } from "./issue-trackers.js";
import { detectGitHubPullRequest, detectIssue, runSidebarLinks } from "./run-links.js";

test("maps a title key to the configured Plane tracker", () => {
  const job = { task: { title: "OMM-3: Keep set -e failures fatal under timings" } };
  assert.deepEqual(detectIssue(job), {
    tracker: "Plane",
    href: "https://plane.danieljohnmorris.com/dan/browse/OMM-3/",
    text: "OMM-3",
  });
});

test("detects a Linear URL from instructions without modifying it", () => {
  const job = { task: { spec: "Track https://linear.app/acme/issue/ENG-42/fix-login" } };
  assert.deepEqual(detectIssue(job), {
    tracker: "Linear",
    href: "https://linear.app/acme/issue/ENG-42/fix-login",
    text: "ENG-42",
  });
});

test("prefers a stored issue URL over a title key", () => {
  const job = {
    metadata: { issue_url: "https://linear.app/acme/issue/ENG-7" },
    task: { title: "OMM-3: x" },
  };
  assert.deepEqual(detectIssue(job), {
    tracker: "Linear",
    href: "https://linear.app/acme/issue/ENG-7",
    text: "ENG-7",
  });
});

test("detects Plane URLs on any configured-or-explicit host", () => {
  const job = { task: { spec: "See https://plane.example.com/ws/browse/ABC-9/" } };
  assert.deepEqual(detectIssue(job), {
    tracker: "Plane",
    href: "https://plane.example.com/ws/browse/ABC-9/",
    text: "ABC-9",
  });

  const uuidIssue = {
    metadata: {
      issue_url: "https://plane.example.com/ws/projects/123e4567-e89b-12d3-a456-426614174000/issues/123e4567-e89b-12d3-a456-426614174001/",
    },
  };
  assert.deepEqual(detectIssue(uuidIssue), {
    tracker: "Plane",
    href: uuidIssue.metadata.issue_url,
    text: "Plane issue",
  });
});

test("does not guess a tracker for an unmatched bare key", () => {
  assert.equal(detectIssue({ task: { title: "ENG-5: thing" } }), null);
});

test("prefers explicit URLs in title before bare keys", () => {
  const job = {
    task: {
      title: "OMM-3: Explicit https://linear.app/acme/issue/ENG-42",
    },
  };
  assert.equal(detectIssue(job).text, "ENG-42");
});

test("uses stored source URLs and falls back to the key or host", () => {
  const keyed = detectIssue({
    task: { source_url: "https://issues.example.com/OMM-9/details" },
  });
  assert.deepEqual(keyed, {
    tracker: "Issue",
    href: "https://issues.example.com/OMM-9/details",
    text: "OMM-9",
  });

  const hosted = detectIssue({
    task: { source_url: "https://issues.example.com/tasks/123" },
  });
  assert.deepEqual(hosted, {
    tracker: "Issue",
    href: "https://issues.example.com/tasks/123",
    text: "issues.example.com",
  });
});

test("uses the last GitHub PR URL in the result before instructions", () => {
  const job = {
    task: { spec: "See https://github.com/omacom/mobile/pull/179" },
  };
  const result = {
    summary: "Opened https://github.com/danieljohnmorris/sparkDash/pull/2, then https://github.com/danieljohnmorris/sparkDash/pull/1.",
  };
  assert.deepEqual(detectGitHubPullRequest(job, result), {
    href: "https://github.com/danieljohnmorris/sparkDash/pull/1",
    text: "danieljohnmorris/sparkDash#1",
  });
});

test("prefers metadata PRs over instructions", () => {
  const job = {
    metadata: { pr_url: "https://github.com/example/repo/pull/4" },
    task: { spec: "Model PR: https://github.com/omacom/mobile/pull/179" },
  };
  assert.deepEqual(detectGitHubPullRequest(job), {
    href: "https://github.com/example/repo/pull/4",
    text: "example/repo#4",
  });
});

test("does not use the model PR cited in coder instructions", () => {
  const job = {
    task: { spec: "Rules. Model PR: https://github.com/omacom/mobile/pull/179" },
  };
  assert.equal(detectGitHubPullRequest(job), null);
});

test("returns no rows when no link is detected", () => {
  assert.deepEqual(
    runSidebarLinks({ task: { title: "No links" } }, { summary: "Done." }),
    { issue: null, pullRequest: null },
  );
});

test("tracker configuration has one Plane and one Linear entry", () => {
  assert.deepEqual(ISSUE_TRACKERS.map(({ type }) => type), ["plane", "linear"]);
});
