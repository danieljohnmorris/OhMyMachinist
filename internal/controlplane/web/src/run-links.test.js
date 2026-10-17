import assert from "node:assert/strict";
import test from "node:test";
import { cardLinks, detectGitHubPullRequest, detectPlaneTicket, detectTicket, runSidebarLinks, safeExternalURL } from "./run-links.js";

test("detects a Plane ticket from the job title", () => {
  const job = { task: { title: "OMM-3: Keep set -e failures fatal under timings" } };
  assert.deepEqual(detectPlaneTicket(job), {
    href: "https://plane.danieljohnmorris.com/dan/browse/OMM-3/",
    text: "OMM-3",
    tracker: "plane",
    external: true,
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
    tracker: "plane",
    external: true,
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
    ticket: null,
    pullRequest: null,
  });
});

test("built-in ticket metadata routes inside the app", () => {
  const job = { metadata: { tracker: "builtin", ticket_key: "MM-4", issue_url: "#/tickets/MM-4" }, task: { title: "OMM-3: ignored title key" } };
  assert.deepEqual(detectTicket(job), { href: "#/tickets/MM-4", text: "MM-4", tracker: "builtin", external: false });
  assert.deepEqual(detectTicket({ metadata: { issue_url: "#/tickets/MM-9" } }), { href: "#/tickets/MM-9", text: "MM-9", tracker: "builtin", external: false });
});

test("Linear and self-hosted Plane metadata links on any host", () => {
  assert.deepEqual(detectTicket({ metadata: { tracker: "linear", ticket_key: "ENG-12", issue_url: "https://linear.app/acme/issue/ENG-12/fix" } }), {
    href: "https://linear.app/acme/issue/ENG-12/fix", text: "ENG-12", tracker: "linear", external: true,
  });
  assert.deepEqual(detectTicket({ metadata: { tracker: "plane", issue_url: "https://plane.example.org/ws/browse/WEB-3/" } }), {
    href: "https://plane.example.org/ws/browse/WEB-3/", text: "WEB-3", tracker: "plane", external: true,
  });
});

test("unsafe metadata URLs never become links", () => {
  assert.equal(safeExternalURL("javascript:alert(1)"), "");
  assert.equal(safeExternalURL("/relative"), "");
  assert.equal(safeExternalURL("https://github.com/a/b/pull/1"), "https://github.com/a/b/pull/1");
  assert.equal(detectTicket({ metadata: { tracker: "linear", issue_url: "javascript:alert(1)" } }), null);
  assert.equal(detectGitHubPullRequest({ metadata: { pr_url: "javascript:alert(1)" } }), null);
});

test("card links combine the ticket and the stored PR", () => {
  const job = { metadata: { tracker: "builtin", ticket_key: "MM-1", pr_url: "https://github.com/acme/app/pull/7" } };
  assert.deepEqual(cardLinks(job), {
    ticket: { href: "#/tickets/MM-1", text: "MM-1", tracker: "builtin", external: false },
    pullRequest: { href: "https://github.com/acme/app/pull/7", text: "acme/app#7" },
  });
});

test("malformed internal ticket references cannot crash the board", () => {
  assert.equal(detectTicket({ metadata: { issue_url: "#/tickets/%ZZ" } }), null);
  assert.equal(detectTicket({ metadata: { tracker: "builtin", ticket_key: "../runs" } }), null);
});
