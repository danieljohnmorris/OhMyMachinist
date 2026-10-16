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

export function detectPlaneTicket(job, result) {
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

// Resolution details are written by the control plane; the merged form carries
// the pull request number and the GitHub login that merged it.
const MERGED_PULL_REQUEST_PATTERN = /^PR #(\d+) \(merged by (.+)\)$/;
// Repositories are stored as the GitHub owner and name when they are hosted on
// GitHub, so their pull requests can be addressed directly.
const GITHUB_REPOSITORY_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\/[A-Za-z0-9._-]+$/;

function pullRequestNumber(href) {
  const match = matches(href, GITHUB_PULL_REQUEST_URL_PATTERN)[0];
  return match ? Number(match[3]) : null;
}

// mergedPullRequestURL resolves the merged pull request behind a resolution.
// The job's own pull request wins when it carries the resolved number; a
// repository that names its GitHub owner still links without job metadata.
function mergedPullRequestURL(job, result, number) {
  const detected = detectGitHubPullRequest(job, result);
  if (detected && pullRequestNumber(detected.href) === number) return detected.href;
  const repository = typeof job?.repository === "string" ? job.repository.trim() : "";
  return GITHUB_REPOSITORY_PATTERN.test(repository) ? `https://github.com/${repository}/pull/${number}` : "";
}

// resolutionSummary describes how a resolved ticket was closed, as text plus
// ordered parts so the pull request and its merger render as links. Unknown
// details stay plain text so no resolution is ever hidden.
export function resolutionSummary(job, result) {
  if (job?.state !== "resolved") return null;
  const detail = typeof job?.resolution_detail === "string" ? job.resolution_detail.trim() : "";
  if (!detail) return null;
  const merged = MERGED_PULL_REQUEST_PATTERN.exec(detail);
  if (!merged) return { text: `Resolved via ${detail}`, parts: [{ text: `Resolved via ${detail}` }] };
  const [, number, login] = merged;
  const href = mergedPullRequestURL(job, result, Number(number));
  return {
    text: `Resolved via PR #${number} (merged by ${login})`,
    parts: [
      { text: "Resolved via " },
      { text: `PR #${number}`, ...(href ? { href } : {}) },
      { text: " (merged by " },
      { text: login, href: `https://github.com/${login}` },
      { text: ")" },
    ],
  };
}

export function runSidebarLinks(job, result) {
  return {
    planeTicket: detectPlaneTicket(job, result),
    pullRequest: detectGitHubPullRequest(job, result),
  };
}
