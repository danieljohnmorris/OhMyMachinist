// Ticket and pull request links for a job. Metadata written by the control
// plane (tracker, ticket_key, issue_url, pr_url) wins; text detection is the
// fallback for older jobs that predate those fields.

const PLANE_BASE_URL = "https://plane.danieljohnmorris.com";
const PLANE_WORKSPACE = "dan";
const PLANE_TICKET_PATTERN = /\b(OMM|MACH|SPARK|BLOG)-\d+\b/g;
const PLANE_URL_PATTERN = /https:\/\/plane\.danieljohnmorris\.com\/[^/\s]+\/browse\/([A-Za-z]+-\d+)(?![0-9])\/?/g;
const GITHUB_PULL_REQUEST_URL_PATTERN = /https:\/\/github\.com\/([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)\/([A-Za-z0-9._-]+)\/pull\/(\d+)(?![0-9])/g;
const TICKET_KEY_PATTERN = /\b([A-Z][A-Z0-9]*-\d+)\b/i;
const BUILTIN_TICKET_ROUTE = /^#\/tickets\/([^/?#\s]+)$/;

export const trackerNames = { builtin: "Built-in", plane: "Plane", linear: "Linear" };

function matches(text, pattern) {
  if (typeof text !== "string" || !text) return [];
  pattern.lastIndex = 0;
  return [...text.matchAll(pattern)];
}

function firstMatch(texts, pattern) {
  return texts.flatMap((text) => matches(text, pattern))[0] || null;
}

// safeExternalURL returns the normalised href for an http(s) URL and "" for
// anything else (javascript:, data:, relative paths, garbage).
export function safeExternalURL(value) {
  if (typeof value !== "string" || !value.trim()) return "";
  try {
    const url = new URL(value.trim());
    return url.protocol === "https:" || url.protocol === "http:" ? url.href : "";
  } catch {
    return "";
  }
}

function trackerFromURL(href) {
  try {
    const host = new URL(href).hostname;
    if (host === "linear.app" || host.endsWith(".linear.app")) return "linear";
    if (host.includes("plane")) return "plane";
  } catch {
    return "";
  }
  return "";
}

// builtinTicketHref is the in-app route for a built-in ticket key.
export function builtinTicketHref(key) {
  return `#/tickets/${encodeURIComponent(key)}`;
}

// metadataTicket reads the canonical ticket link the control plane stored on
// the job. Built-in tickets route inside the app; tracker URLs must be http(s).
function metadataTicket(job) {
  const metadata = job?.metadata || {};
  const issueURL = typeof metadata.issue_url === "string" ? metadata.issue_url.trim() : "";
  const key = typeof metadata.ticket_key === "string" ? metadata.ticket_key.trim() : "";
  const builtinRoute = issueURL.match(BUILTIN_TICKET_ROUTE);
  if (metadata.tracker === "builtin" || builtinRoute) {
    let builtinKey = key;
    try { builtinKey ||= builtinRoute ? decodeURIComponent(builtinRoute[1]) : ""; }
    catch { return null; }
    if (!/^[A-Z][A-Z0-9]{0,9}-\d+$/.test(builtinKey)) return null;
    return { href: builtinTicketHref(builtinKey), text: builtinKey, tracker: "builtin", external: false };
  }
  const href = safeExternalURL(issueURL);
  if (!href) return null;
  const tracker = metadata.tracker || trackerFromURL(href);
  if (!tracker) return null;
  const text = key || href.match(TICKET_KEY_PATTERN)?.[1]?.toUpperCase() || trackerNames[tracker] || "Ticket";
  return { href, text, tracker, external: true };
}

// detectPlaneTicket is the legacy text fallback for jobs created before the
// ticket metadata existed.
export function detectPlaneTicket(job, result) {
  const title = job?.task?.title || job?.github_issue_title || "";
  const instructions = [job?.task?.spec, job?.prompt];
  const resultText = [result?.summary, result?.error];
  const url = firstMatch([title, ...instructions, ...resultText], PLANE_URL_PATTERN);
  if (url) return { href: url[0], text: url[1], tracker: "plane", external: true };

  const ticket = firstMatch([title, ...instructions, ...resultText], PLANE_TICKET_PATTERN);
  return ticket ? {
    href: `${PLANE_BASE_URL}/${PLANE_WORKSPACE}/browse/${ticket[0]}/`,
    text: ticket[0],
    tracker: "plane",
    external: true,
  } : null;
}

// detectTicket prefers stored metadata and only falls back to text detection.
export function detectTicket(job, result) {
  return metadataTicket(job) || detectPlaneTicket(job, result);
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
  const metadataHref = safeExternalURL(metadataURL);
  if (metadataHref) return { href: metadataHref, text: "Pull request" };

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

// cardLinks is the cheap board-card version: metadata plus title detection,
// no run result.
export function cardLinks(job) {
  return { ticket: detectTicket(job), pullRequest: detectGitHubPullRequest(job) };
}

export function runSidebarLinks(job, result) {
  return {
    ticket: detectTicket(job, result),
    pullRequest: detectGitHubPullRequest(job, result),
  };
}
