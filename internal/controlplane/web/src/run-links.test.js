import assert from "node:assert/strict";
import test from "node:test";
import { detectGitHubPullRequest, detectPlaneTicket, runSidebarLinks } from "./run-links.js";

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
