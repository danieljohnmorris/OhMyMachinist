import { createElement } from "react";

const URL_PATTERN = /(https?:\/\/\S+)/g;
const TRAILING_PUNCTUATION = /[.,)]+$/;

export function trimTrailingPunctuation(url) {
  return url.replace(TRAILING_PUNCTUATION, "");
}

const LINK_CLASS =
  "break-words font-medium text-primary underline underline-offset-2 hover:text-primary/80";

export function linkAnchor(url, key) {
  const clean = trimTrailingPunctuation(url);
  return createElement(
    "a",
    {
      key,
      className: LINK_CLASS,
      href: clean,
      target: "_blank",
      rel: "noopener noreferrer",
    },
    clean,
  );
}

export function linkifyText(text) {
  if (!text) return [];
  const parts = [];
  let cursor = 0;

  for (const match of text.matchAll(URL_PATTERN)) {
    const url = trimTrailingPunctuation(match[0]);
    parts.push(text.slice(cursor, match.index));
    parts.push(linkAnchor(url, match.index));
    cursor = match.index + url.length;
  }

  parts.push(text.slice(cursor));
  return parts;
}
