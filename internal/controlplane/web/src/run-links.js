import { ISSUE_TRACKERS } from "./issue-trackers.js";

const PLANE_URL_PATTERN = /https?:\/\/[^/\s?#]+\/[^/\s?#]+\/browse\/(?<key>[A-Z][A-Z0-9]{1,9}-\d+(?![0-9]))\/?/g;
const PLANE_ISSUE_URL_PATTERN = /https?:\/\/[^/\s?#]+\/([^/\s?#]+)\/projects\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\/issues\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\/?/gi;
const LINEAR_URL_PATTERN = /https?:\/\/linear\.app\/[^/\s?#]+\/issue\/(?<key>[A-Z][A-Z0-9]{1,9}-\d+(?![0-9]))(?:\/[^\s?#]*)?/g;
const GITHUB_PULL_REQUEST_URL_PATTERN = /https:\/\/github\.com\/([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)\/([A-Za-z0-9._-]+)\/pull\/(\d+)(?![0-9])/g;
const BARE_KEY_PATTERN = /\b([A-Z][A-Z0-9]{1,9})-\d+\b/g;
const URL_KEY_PATTERN = /\/(?<key>[A-Z][A-Z0-9]{1,9}-\d+(?![0-9]))/;

function matches(text, pattern) {
  if (typeof text !== "string" || !text) return [];
  pattern.lastIndex = 0;
  return [...text.matchAll(pattern)];
}

function firstMatch(texts, pattern) {
  return texts.flatMap((text) => matches(text, pattern))[0] || null;
}

function httpURL(url) {
  return typeof url === "string" && /^https?:\/\//i.test(url) ? url : null;
}

function storedIssueLink(job) {
  const urls = [
    job?.metadata?.issue_url,
    job?.task?.source_url,
    job?.source_url,
  ];

  for (const candidate of urls) {
    const url = httpURL(candidate);
    if (!url) continue;
    const parsed = new URL(url);
    const linearMatch = matches(url, LINEAR_URL_PATTERN)[0];
    if (linearMatch) {
      return { tracker: "Linear", href: url, text: linearMatch.groups.key };
    }

    const planeMatch = matches(url, PLANE_URL_PATTERN)[0];
    if (planeMatch) {
      return { tracker: "Plane", href: url, text: planeMatch.groups.key };
    }

    if (matches(url, PLANE_ISSUE_URL_PATTERN)[0]) {
      return { tracker: "Plane", href: url, text: "Plane issue" };
    }

    const keyMatch = URL_KEY_PATTERN.exec(parsed.pathname);
    return {
      tracker: "Issue",
      href: url,
      text: keyMatch ? keyMatch.groups.key : parsed.host,
    };
  }

  return null;
}

function explicitIssueLink(job, result) {
  const texts = issueTexts(job, result);
  const linear = firstMatch(texts, LINEAR_URL_PATTERN);
  if (linear) {
    return { tracker: "Linear", href: linear[0], text: linear.groups.key };
  }

  const planeBrowse = firstMatch(texts, PLANE_URL_PATTERN);
  if (planeBrowse) {
    return { tracker: "Plane", href: planeBrowse[0], text: planeBrowse.groups.key };
  }

  const planeIssue = firstMatch(texts, PLANE_ISSUE_URL_PATTERN);
  if (planeIssue) {
    return { tracker: "Plane", href: planeIssue[0], text: "Plane issue" };
  }

  return null;
}

function issueTexts(job, result) {
  const title = job?.task?.title || job?.github_issue_title || "";
  return [title, job?.task?.spec, job?.prompt, result?.summary, result?.error];
}

function trackerForBareKey(key) {
  return ISSUE_TRACKERS.find(
    (tracker) => tracker.workspace && tracker.keys.includes(key),
  );
}

function bareKeyLink(job) {
  const title = job?.task?.title || job?.github_issue_title || "";
  const titleMatch = title.match(/^([A-Z][A-Z0-9]{1,9})-\d+\b/);
  const instructions = [job?.task?.spec, job?.prompt];
  const instructionMatch = firstMatch(instructions, BARE_KEY_PATTERN);
  const match = titleMatch || instructionMatch;
  if (!match) return null;

  const tracker = trackerForBareKey(match[1]);
  if (!tracker) return null;
  const key = match[0];
  if (tracker.type === "plane") {
    return {
      tracker: "Plane",
      href: `${tracker.baseUrl}/${tracker.workspace}/browse/${key}/`,
      text: key,
    };
  }

  return {
    tracker: "Linear",
    href: `https://linear.app/${tracker.workspace}/issue/${key}`,
    text: key,
  };
}

export function detectIssue(job, result) {
  return storedIssueLink(job) || explicitIssueLink(job, result) || bareKeyLink(job);
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
  return {
    issue: detectIssue(job, result),
    pullRequest: detectGitHubPullRequest(job, result),
  };
}
