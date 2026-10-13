const PLANE_BASE_URL = "https://plane.danieljohnmorris.com";
const PLANE_WORKSPACE = "dan";
const PLANE_TICKET_PATTERN = /\b(OMM|MACH|SPARK|BLOG)-\d+\b/g;
const PLANE_URL_PATTERN = /https:\/\/plane\.danieljohnmorris\.com\/[^/\s]+\/browse\/([A-Za-z]+-\d+)(?![0-9])\/?/g;
const GITHUB_PULL_REQUEST_URL_PATTERN = /https:\/\/github\.com\/([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)\/([A-Za-z0-9._-]+)\/pull\/(\d+)(?![0-9])/g;

function matches(text, pattern) {
  if (typeof text !== "string" || !text) return [];
  pattern.lastIndex = 0;
  return [...text.matchAll(pattern)];
}

function firstMatch(texts, pattern) {
  return texts.flatMap((text) => matches(text, pattern))[0] || null;
}

function planeTicketId(match) {
  return match[1];
}

// Jobs started from the native backlog carry their tracker identity on the
// job payload itself. The stored identity wins over host/prefix inference,
// which stays as the fallback for legacy jobs.
export function detectBacklogIssue(job) {
  if (job?.source_kind !== "backlog") return null;
  const id = typeof job?.backlog_issue_id === "string" ? job.backlog_issue_id.trim() : "";
  if (!id) return null;
  const key = typeof job?.backlog_issue_key === "string" ? job.backlog_issue_key.trim() : "";
  return { href: `#/backlog?issue=${encodeURIComponent(id)}`, text: key || id.slice(0, 8) };
}

export function detectPlaneTicket(job, result) {
  if (detectBacklogIssue(job)) return null;
  const title = job?.task?.title || job?.github_issue_title || "";
  const instructions = [job?.task?.spec, job?.prompt];
  const resultText = [result?.summary, result?.error];
  const url = firstMatch([title, ...instructions, ...resultText], PLANE_URL_PATTERN);
  if (url) return { href: url[0], text: planeTicketId(url) };

  const ticket = firstMatch([title, ...instructions, ...resultText], PLANE_TICKET_PATTERN);
  return ticket ? {
    href: `${PLANE_BASE_URL}/${PLANE_WORKSPACE}/browse/${ticket[0]}/`,
    text: ticket[0],
  } : null;
}

function isCitedExamplePullRequest(text, match) {
  const context = text.slice(Math.max(0, match.index - 80), match.index);
  return /model\s+pr\b|example\s+pr\b|reference\s+pr\b/i.test(context);
}

export function detectGitHubPullRequest(job, result) {
  const resultText = [result?.summary, result?.error];
  const resultMatches = resultText.flatMap((text) => matches(text, GITHUB_PULL_REQUEST_URL_PATTERN));
  const lastResult = resultMatches.at(-1);
  if (lastResult) return pullRequestLink(lastResult);

  const metadataURL = job?.metadata?.pr_url;
  const metadataMatch = matches(metadataURL, GITHUB_PULL_REQUEST_URL_PATTERN)[0];
  if (metadataMatch) return pullRequestLink(metadataMatch);

  const instructions = [job?.task?.spec, job?.prompt];
  const instructionMatches = instructions.flatMap((text) => matches(text, GITHUB_PULL_REQUEST_URL_PATTERN));
  const instruction = instructionMatches.at(-1);
  if (instruction && !isCitedExamplePullRequest(instructions.find((text) => text?.includes(instruction[0])), instruction)) {
    return pullRequestLink(instruction);
  }

  return null;
}

function pullRequestLink(match) {
  const [, owner, repository, number] = match;
  return { href: match[0], text: `${owner}/${repository}#${number}` };
}

export function runSidebarLinks(job, result) {
  const backlogIssue = detectBacklogIssue(job);
  return {
    planeTicket: backlogIssue ? null : detectPlaneTicket(job, result),
    backlogIssue,
    pullRequest: detectGitHubPullRequest(job, result),
  };
}
