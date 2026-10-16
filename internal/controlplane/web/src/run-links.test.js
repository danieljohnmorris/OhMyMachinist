import assert from "node:assert/strict";
import test from "node:test";
import { detectGitHubPullRequest, detectPlaneTicket, resolutionSummary, runSidebarLinks } from "./run-links.js";

test("detects a Plane ticket from the job title", () => {
  const job = { task: { title: "OMM-3: Keep set -e failures fatal under timings" } };
  assert.deepEqual(detectPlaneTicket(job), {
    href: "https://plane.danieljohnmorris.com/dan/browse/OMM-3/",
    text: "OMM-3",
  });
});

test("uses an explicit Plane URL as-is and before a title ticket", () => {
  const job = {
    task: {
      title: "MACH-6: Add sidebar links",
      spec: "Track at https://plane.danieljohnmorris.com/other-workspace/browse/BLOG-7",
    },
  };
  assert.deepEqual(detectPlaneTicket(job), {
    href: "https://plane.danieljohnmorris.com/other-workspace/browse/BLOG-7",
    text: "BLOG-7",
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

test("prefers result and metadata PRs over instructions", () => {
  const job = {
    metadata: { pr_url: "https://github.com/example/repo/pull/4" },
    task: { spec: "Model PR: https://github.com/omacom/mobile/pull/179" },
  };
  assert.deepEqual(detectGitHubPullRequest(job), {
    href: "https://github.com/example/repo/pull/4",
    text: "example/repo#4",
  });
});

test("does not use a model PR cited in coder instructions", () => {
  const job = {
    task: { spec: "Rules. Model PR: https://github.com/omacom/mobile/pull/179" },
  };
  assert.equal(detectGitHubPullRequest(job), null);
});

test("returns no links when neither source contains a supported reference", () => {
  assert.deepEqual(runSidebarLinks({ task: { title: "No links" } }, { summary: "Done." }), {
    planeTicket: null,
    pullRequest: null,
  });
});

test("links the resolved pull request and its merger", () => {
  const job = {
    state: "resolved",
    resolution_kind: "pr",
    resolution_detail: "PR #33 (merged by danieljohnmorris)",
    repository: "danieljohnmorris/OhMyMachinist",
    metadata: { pr_url: "https://github.com/danieljohnmorris/OhMyMachinist/pull/33" },
  };
  const summary = resolutionSummary(job);
  assert.equal(summary.text, "Resolved via PR #33 (merged by danieljohnmorris)");
  assert.deepEqual(summary.parts, [
    { text: "Resolved via " },
    { text: "PR #33", href: "https://github.com/danieljohnmorris/OhMyMachinist/pull/33" },
    { text: " (merged by " },
    { text: "danieljohnmorris", href: "https://github.com/danieljohnmorris" },
    { text: ")" },
  ]);
});

test("builds the pull request link from the repository when no metadata carries it", () => {
  const job = {
    state: "resolved",
    resolution_kind: "pr",
    resolution_detail: "PR #12 (merged by kim)",
    repository: "example/repo",
  };
  const summary = resolutionSummary(job);
  assert.equal(summary.parts[1].href, "https://github.com/example/repo/pull/12");
  assert.equal(summary.parts[3].href, "https://github.com/kim");
});

test("ignores a job pull request that is not the resolved one", () => {
  const job = {
    state: "resolved",
    resolution_kind: "pr",
    resolution_detail: "PR #33 (merged by danieljohnmorris)",
    repository: "example/repo",
    metadata: { pr_url: "https://github.com/example/repo/pull/7" },
  };
  assert.equal(resolutionSummary(job).parts[1].href, "https://github.com/example/repo/pull/33");
});

test("keeps the merger link when the repository is not a GitHub owner and name", () => {
  const job = { state: "resolved", resolution_kind: "pr", resolution_detail: "PR #4 (merged by kim)", repository: "local-checkout" };
  const summary = resolutionSummary(job);
  assert.deepEqual(summary.parts[1], { text: "PR #4" });
  assert.equal(summary.parts[3].href, "https://github.com/kim");
});

test("resolutions without a merged pull request stay plain text", () => {
  const summary = resolutionSummary({ state: "resolved", resolution_kind: "plane", resolution_detail: "Plane Done" });
  assert.equal(summary.text, "Resolved via Plane Done");
  assert.deepEqual(summary.parts, [{ text: "Resolved via Plane Done" }]);
});

test("only resolved jobs carry a resolution summary", () => {
  assert.equal(resolutionSummary({ state: "succeeded", resolution_kind: "pr", resolution_detail: "PR #12 (merged by dan)" }), null);
  assert.equal(resolutionSummary({ state: "resolved", resolution_kind: "pr", resolution_detail: "  " }), null);
});
