import assert from "node:assert/strict";
import test, { after } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { createServer } from "vite";

// One attempt fixture with the run signals the reason mapping reads.
const attempt = (id, state, created, extra = {}) => ({
  id,
  state,
  created_at: created,
  runs: extra.runs || [{ id: `run_${id}`, state, completed_at: created }],
  ...extra,
});

// Vite resolves the JSX component the same way the app build does; one server
// is shared across the tests and closed once the file's tests finish.
let modules;
let server;
after(async () => {
  await server?.close();
});

async function load() {
  if (!modules) {
    server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
    modules = {
      strip: await server.ssrLoadModule("/src/attempt-strip.jsx"),
      board: await server.ssrLoadModule("/src/runs-board.js"),
    };
  }
  return modules;
}

const render = (AttemptStrip, attempts, now) =>
  renderToStaticMarkup(createElement(AttemptStrip, { attempts, now: now ? new Date(now) : undefined }));

const dotCount = (html) => (html.match(/data-testid="attempt-dot-/g) || []).length;

test("four attempts render one 'Attempt 4 of 4' line and four dots", async () => {
  const { strip } = await load();
  const attempts = [
    attempt("a", "cancelled", "2026-10-01T00:00:00Z"),
    attempt("b", "timed_out", "2026-10-02T00:00:00Z"),
    attempt("c", "failed", "2026-10-03T00:00:00Z"),
    attempt("d", "running", "2026-10-04T00:00:00Z"),
  ];
  const html = render(strip.AttemptStrip, attempts, "2026-10-04T02:00:00Z");
  assert.match(html, /Attempt 4 of 4/);
  assert.equal(dotCount(html), 4);
  assert.doesNotMatch(html, /\+/, "no overflow for four attempts");
  // Latest attempt is the rightmost dot and links to its run page.
  assert.ok(html.includes('href="#/runs/d"'), "latest attempt dot links to its run");
  assert.ok(html.indexOf('href="#/runs/d"') > html.indexOf('href="#/runs/c"'), "latest attempt sits on the right");
  // Tooltip carries the attempt number, end reason and relative time.
  assert.match(html, /aria-label="Attempt 4 of 4 · running · 2h ago"/);
  assert.match(html, /aria-label="Attempt 1 of 4 · cancelled by operator · 3d ago"/);
  assert.match(html, /aria-label="Attempt 2 of 4 · timeout · 2d ago"/);
});

test("a single attempt renders no strip", async () => {
  const { strip } = await load();
  assert.equal(render(strip.AttemptStrip, [attempt("only", "succeeded", "2026-10-01T00:00:00Z")]), "");
});

test("more than eight attempts show eight dots plus an overflow count", async () => {
  const { strip } = await load();
  const attempts = Array.from({ length: 11 }, (_, index) =>
    attempt(`a${index}`, "failed", `2026-10-${String(index + 1).padStart(2, "0")}T00:00:00Z`),
  );
  const html = render(strip.AttemptStrip, attempts, "2026-10-14T00:00:00Z");
  assert.match(html, /Attempt 11 of 11/);
  assert.equal(dotCount(html), 8, "at most eight dots");
  assert.match(html, /data-testid="attempt-overflow"[^>]*>\+3</);
  // The eight shown dots are the newest, numbered from the overflow boundary.
  assert.match(html, /data-testid="attempt-dot-11"/);
  assert.match(html, /data-testid="attempt-dot-4"/);
  assert.doesNotMatch(html, /data-testid="attempt-dot-3"/);
});

test("dots render with a state colour and never the old identical links", async () => {
  const { strip } = await load();
  const html = render(strip.AttemptStrip, [
    attempt("a", "failed", "2026-10-01T00:00:00Z"),
    attempt("b", "succeeded", "2026-10-02T00:00:00Z"),
  ]);
  assert.match(html, /bg-success/);
  assert.match(html, /bg-danger/);
  assert.match(html, /focus-visible:ring-ring/, "dots are keyboard focusable with a ring");
  assert.doesNotMatch(html, /underline/);
});

test("attemptEndReason derives the reason from state, error and exit code", async () => {
  const { board } = await load();
  assert.equal(board.attemptEndReason({ state: "running", runs: [] }), "running");
  assert.equal(board.attemptEndReason({ state: "timed_out", runs: [{ state: "timed_out", exit_code: 124 }] }), "timeout");
  assert.equal(board.attemptEndReason({ state: "cancelled", runs: [{ state: "cancelled", exit_code: 130, error: "Cancelled by operator" }] }), "cancelled by operator");
  assert.equal(board.attemptEndReason({ state: "interrupted", runs: [{ state: "interrupted", error: "Worker lease expired" }] }), "lease expired");
  assert.equal(board.attemptEndReason({ state: "failed", runs: [{ state: "failed", exit_code: 7 }] }), "exit code 7");
  assert.equal(board.attemptEndReason({ state: "succeeded", runs: [{ state: "succeeded", exit_code: 0 }] }), "succeeded");
  // MACH-46 recorded reasons win when present.
  assert.equal(board.attemptEndReason({ state: "failed", runs: [{ state: "failed", attempts: [{ end_reason: "worker restart" }] }] }), "worker restart");
});

test("attemptTone maps end states onto the dot tones", async () => {
  const { board } = await load();
  assert.equal(board.attemptTone("succeeded"), "success");
  assert.equal(board.attemptTone("resolved"), "success");
  assert.equal(board.attemptTone("failed"), "failed");
  assert.equal(board.attemptTone("cancelled"), "cancelled");
  assert.equal(board.attemptTone("timed_out"), "timedOut");
  assert.equal(board.attemptTone("interrupted"), "stalled");
  assert.equal(board.attemptTone("running"), "running");
});