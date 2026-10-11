import assert from "node:assert/strict";
import test from "node:test";
import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { Markdown } from "./markdown.js";

const FIXTURE = [
  "# Plan",
  "",
  "## Approach",
  "",
  "Use **bold moves** and *subtle ones* with `inline_code`.",
  "",
  "- first bullet",
  "- second bullet",
  "",
  "1. step one",
  "2. step two",
  "",
  "```js",
  "const x = 1;",
  "```",
  "",
  "A [markdown link](https://example.com/docs) and a bare URL https://example.com/page, plus <img src=x onerror=\"alert(1)\"> raw HTML.",
].join("\n");

test("renders headings, lists and code as HTML", () => {
  const html = renderToStaticMarkup(createElement(Markdown, { source: FIXTURE }));
  assert.match(html, /<h1>Plan<\/h1>/);
  assert.match(html, /<h2>Approach<\/h2>/);
  assert.match(html, /<ul><li>first bullet<\/li><li>second bullet<\/li><\/ul>/);
  assert.match(html, /<ol><li>step one<\/li><li>step two<\/li><\/ol>/);
  assert.match(html, /<strong>bold moves<\/strong>/);
  assert.match(html, /<em>subtle ones<\/em>/);
  assert.match(html, /<code>inline_code<\/code>/);
  assert.match(html, /<pre><code[^>]*>const x = 1;<\/code><\/pre>/);
});

test("does not wrap the body in a pre element", () => {
  const html = renderToStaticMarkup(createElement(Markdown, { source: "Just text." }));
  assert.equal(html.startsWith("<div"), true);
  assert.equal(html.includes("<pre>"), false);
});

test("escapes raw HTML instead of rendering it", () => {
  const html = renderToStaticMarkup(
    createElement(Markdown, { source: "<img src=x onerror=\"alert(1)\">"}),
  );
  assert.equal(html.includes("<img"), false);
  assert.match(html, /&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;/);
});

test("markdown and bare URLs become styled new-tab anchors", () => {
  const html = renderToStaticMarkup(
    createElement(Markdown, { source: "See [docs](https://example.com/docs) and https://example.com/page, done." }),
  );
  const anchors = [...html.matchAll(/<a [^>]*>/g)].map((m) => m[0]);
  assert.equal(anchors.length, 2);
  for (const anchor of anchors) {
    assert.match(anchor, /class="[^"]*text-primary[^"]*underline[^"]*"/);
    assert.match(anchor, /target="_blank"/);
    assert.match(anchor, /rel="noopener noreferrer"/);
  }
  assert.match(html, /href="https:\/\/example\.com\/docs"/);
  // Trailing punctuation is excluded from the bare URL.
  assert.match(html, /href="https:\/\/example\.com\/page"/);
  assert.match(html, /<\/a>, done\./);
});
