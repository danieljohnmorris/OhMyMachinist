import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("workers show connected and disconnected poll status", async () => {
  const catalog = await readFile(new URL("./catalog.jsx", import.meta.url), "utf8");
  const main = await readFile(new URL("./main.jsx", import.meta.url), "utf8");

  assert.match(catalog, /worker\.connected \? "Connected" : "Disconnected"/);
  assert.match(catalog, /worker\.connected \? "[^"]*text-success"/);
  assert.match(catalog, /Last seen \{relativeTime\(worker\.last_seen_at\)\}/);
  assert.match(main, /status\.workers\.filter\(\(worker\) => worker\.connected\)\.length/);
  assert.match(main, /worker\$\{connectedWorkers === 1 \? "" : "s"\} online/);
  assert.match(main, /truncate whitespace-nowrap/);
  assert.match(main, /concurrencyLabel\(status\.max_concurrent_jobs, status\.jobs\.filter\(\(job\) => job\.state === "running"\)\.length\)/);
  assert.match(main, /Workers are connected lanes that can take a job\. The limit is how many jobs may run at once\./);
});
