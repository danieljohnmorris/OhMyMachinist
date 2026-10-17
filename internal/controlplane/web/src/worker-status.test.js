import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("workers show connected and disconnected poll status", async () => {
  const catalog = await readFile(new URL("./catalog.jsx", import.meta.url), "utf8");
  const factory = await readFile(new URL("./factory.jsx", import.meta.url), "utf8");

  assert.match(catalog, /worker\.connected \? "Connected" : "Disconnected"/);
  assert.match(catalog, /tone=\{worker\.connected \? "success" : "neutral"\}/);
  assert.match(catalog, /Last seen \{relativeTime\(worker\.last_seen_at\)\}/);
  // The sidebar shows one merged capacity row: the job limit, live lane state,
  // and the connected-worker count, with the tooltip explaining the terms.
  assert.match(factory, /capacityLabel\(factory\.max_concurrent_jobs, busy, factory\.ready_lanes \|\| 0\)/);
  assert.match(factory, /worker\$\{online === 1 \? "" : "s"\} online/);
  assert.match(factory, /Workers are connected lanes that can take a job\. The limit is how many jobs may run at once\./);
});
