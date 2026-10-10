import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { linkifyText } from "./linkify.js";

test("linkifies URLs without trailing punctuation", () => {
  assert.equal(linkifyText("No URL here.").length, 1);

  const parts = linkifyText("See https://github.com/omacom/mobile/pull/179. Next, https://example.com/page,)");
  assert.equal(parts.filter(part => part?.type === "a").length, 2);
  assert.equal(parts[1].props.href, "https://github.com/omacom/mobile/pull/179");
  assert.equal(parts[1].props.target, "_blank");
  assert.equal(parts[1].props.rel, "noopener noreferrer");
  assert.equal(parts[3].props.href, "https://example.com/page");
});

test("escapes non-URL text around links", () => {
  const html = renderToStaticMarkup(
    createElement(
      "p",
      null,
      linkifyText('<img src=x onerror="alert(1)"> https://example.com/page,'),
    ),
  );
  assert.match(html, /^<p>&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt; <a href="https:\/\/example\.com\/page"/);
  assert.match(html, /<\/a>,<\/p>$/);
});
