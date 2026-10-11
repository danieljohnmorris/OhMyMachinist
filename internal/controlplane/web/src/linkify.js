import { createElement } from "react";

const URL_PATTERN = /(https?:\/\/\S+)/g;
const TRAILING_PUNCTUATION = /[.,)]+$/;

export function linkifyText(text) {
  if (!text) return [];
  const parts = [];
  let cursor = 0;

  for (const match of text.matchAll(URL_PATTERN)) {
    const url = match[0].replace(TRAILING_PUNCTUATION, "");
    parts.push(text.slice(cursor, match.index));
    parts.push(
      createElement(
        "a",
        { key: match.index, href: url, target: "_blank", rel: "noopener noreferrer" },
        url,
      ),
    );
    cursor = match.index + url.length;
  }

  parts.push(text.slice(cursor));
  return parts;
}
