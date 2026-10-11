import { createElement } from "react";
import { linkAnchor, linkifyText, trimTrailingPunctuation } from "./linkify.js";

// Inline markdown: code spans, bold, italic, markdown links and bare URLs.
// Everything else — including raw HTML — stays as plain text, which React
// escapes, so instructions can never inject markup.
const INLINE_PATTERN =
  /`([^`]+)`|\*\*([^*]+?)\*\*|\*([^*]+?)\*|\[([^\]]*)\]\(([^)\s]+)\)|(https?:\/\/[^\s]+)/g;

function renderInline(text, keyPrefix = "i") {
  const nodes = [];
  let cursor = 0;
  let index = 0;

  for (const match of text.matchAll(INLINE_PATTERN)) {
    const [raw, code, bold, italic, label, href, url] = match;
    if (match.index > cursor) {
      nodes.push(...linkifyText(text.slice(cursor, match.index)));
    }
    const key = `${keyPrefix}-${index++}`;
    if (code !== undefined) {
      nodes.push(createElement("code", { key }, code));
    } else if (bold !== undefined) {
      nodes.push(createElement("strong", { key }, renderInline(bold, key)));
    } else if (italic !== undefined) {
      nodes.push(createElement("em", { key }, renderInline(italic, key)));
    } else if (href !== undefined) {
      nodes.push(
        createElement(
          "a",
          {
            key,
            className: "break-words font-medium text-primary underline underline-offset-2 hover:text-primary/80",
            href,
            target: "_blank",
            rel: "noopener noreferrer",
          },
          renderInline(label, key),
        ),
      );
    } else if (url !== undefined) {
      // Trailing punctuation stays as text: the anchor stops at the URL.
      const clean = trimTrailingPunctuation(url);
      nodes.push(linkAnchor(url, key));
      cursor = match.index + clean.length;
      continue;
    }
    cursor = match.index + raw.length;
  }

  if (cursor < text.length) {
    nodes.push(...linkifyText(text.slice(cursor)));
  }
  return nodes;
}

function renderCodeBlock(lines, info, key) {
  const codeProps = info ? { "data-lang": info } : {};
  return createElement(
    "pre",
    { key },
    createElement("code", codeProps, lines.join("\n")),
  );
}

function renderBlocks(text) {
  const lines = text.split("\n");
  const blocks = [];
  let paragraph = [];

  const flushParagraph = () => {
    if (paragraph.length) {
      blocks.push(
        createElement("p", { key: `p-${blocks.length}` }, renderInline(paragraph.join(" "))),
      );
      paragraph = [];
    }
  };

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];

    const fence = line.match(/^```\s*(\S*)\s*$/);
    if (fence) {
      flushParagraph();
      const codeLines = [];
      i++;
      while (i < lines.length && !/^```\s*$/.test(lines[i])) {
        codeLines.push(lines[i]);
        i++;
      }
      blocks.push(renderCodeBlock(codeLines, fence[1], `pre-${blocks.length}`));
      continue;
    }

    const heading = line.match(/^(#{1,6})\s+(.*)$/);
    if (heading) {
      flushParagraph();
      blocks.push(
        createElement(
          `h${heading[1].length}`,
          { key: `h-${blocks.length}` },
          renderInline(heading[2].trim()),
        ),
      );
      continue;
    }

    const bullet = line.match(/^\s*[-*]\s+(.*)$/);
    const numbered = line.match(/^\s*\d+[.)]\s+(.*)$/);
    if (bullet || numbered) {
      flushParagraph();
      const ordered = Boolean(numbered);
      const items = [];
      while (i < lines.length) {
        const item = ordered
          ? lines[i].match(/^\s*\d+[.)]\s+(.*)$/)
          : lines[i].match(/^\s*[-*]\s+(.*)$/);
        if (!item) break;
        items.push(createElement("li", { key: `li-${items.length}` }, renderInline(item[1])));
        i++;
      }
      i--;
      blocks.push(
        createElement(ordered ? "ol" : "ul", { key: `l-${blocks.length}` }, items),
      );
      continue;
    }

    if (line.trim() === "") {
      flushParagraph();
      continue;
    }

    paragraph.push(line.trim());
  }

  flushParagraph();
  return blocks;
}

export function Markdown({ source, className }) {
  if (!source) return null;
  return createElement(
    "div",
    { className: ["markdown-body", className].filter(Boolean).join(" ") },
    renderBlocks(source),
  );
}
