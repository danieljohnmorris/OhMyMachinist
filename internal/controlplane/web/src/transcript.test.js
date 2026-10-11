import test from "node:test";
import assert from "node:assert/strict";
import { transcriptIsLive, transcriptLines } from "./transcript.js";

const b64 = (text) => Buffer.from(text, "utf8").toString("base64");
const line = (event) => JSON.stringify(event);

test("joins base64 output chunks into whole lines per stream", () => {
  const events = [
    line({ type: "run.started", message: "command=build" }),
    line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("hel") }),
    line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("lo\nwor") }),
    line({ type: "process.output", stream: "stderr", encoding: "base64", data: b64("warn\n") }),
    line({ type: "run.completed", message: "state=succeeded" }),
  ].join("\n");
  assert.deepEqual(transcriptLines(events), [
    { stream: "event", text: "run.started: command=build" },
    { stream: "stdout", text: "hello" },
    { stream: "stderr", text: "warn" },
    { stream: "event", text: "run.completed: state=succeeded" },
    { stream: "stdout", text: "wor" },
  ]);
});

test("skips malformed lines and keeps only the newest lines", () => {
  const events = ["not json", ...Array.from({ length: 5 }, (_, i) => line({ type: "tick", message: String(i) }))].join("\n");
  assert.deepEqual(transcriptLines(events, 2).map((entry) => entry.text), ["tick: 3", "tick: 4"]);
  assert.deepEqual(transcriptLines(""), []);
});

test("polls only while the run is active", () => {
  assert.equal(transcriptIsLive("running"), true);
  assert.equal(transcriptIsLive("succeeded"), false);
});
