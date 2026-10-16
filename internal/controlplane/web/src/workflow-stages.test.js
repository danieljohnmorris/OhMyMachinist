import assert from "node:assert/strict";
import test from "node:test";
import { jobStageLabel, stageStripItems, stageStripText } from "./workflow-stages.js";

const webPRStage = {
  job_type: "web_pr",
  current: "review",
  round: 2,
  stages: [
    { id: "queued", name: "Queued", state: "complete" },
    { id: "planning", name: "Planning", state: "complete" },
    { id: "coding", name: "Coding", state: "complete" },
    { id: "review", name: "PR review", state: "current", round: 2 },
    { id: "uat", name: "UAT", state: "pending" },
    { id: "finished", name: "Finished", state: "pending" },
  ],
};

test("stage strip marks completed stages and the review round", () => {
  assert.equal(
    stageStripText(webPRStage),
    "Queued ✓ · Planning ✓ · Coding ✓ · PR review (round 2) · UAT · Finished",
  );
  const items = stageStripItems(webPRStage);
  assert.equal(items[1].tick, true);
  assert.equal(items[3].current, true);
  assert.equal(items[3].round, 2);
  assert.equal(items[4].state, "pending");
});

test("stage strip flags exhausted review as needing attention", () => {
  const blocked = { ...webPRStage, stages: webPRStage.stages.map((stage) => (stage.id === "review" ? { ...stage, state: "attention", round: 4 } : stage)) };
  const review = stageStripItems(blocked).find((item) => item.id === "review");
  assert.equal(review.attention, true);
  assert.equal(review.current, true);
});

test("stage strip is empty without a derived stage", () => {
  assert.deepEqual(stageStripItems(undefined), []);
  assert.deepEqual(stageStripItems({ stages: [] }), []);
  assert.equal(stageStripText({}), "");
});

test("job stage label names the current stage", () => {
  assert.equal(jobStageLabel({ stage: webPRStage }), "PR review (round 2)");
  const finished = { job_type: "web_pr", current: "finished", stages: webPRStage.stages.map((stage) => ({ ...stage, state: "complete" })) };
  assert.equal(jobStageLabel({ stage: finished }), "Finished");
  assert.equal(jobStageLabel({}), "");
});