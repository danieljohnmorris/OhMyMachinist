// Configurable workflow stages (MACH-43). The control plane derives a stage
// strip for every job from its job type and current state; these helpers turn
// that payload into the labels the board renders on a card.

// stripLabel is one stage's display name, with the review round when present.
function stripLabel(stage) {
  const name = typeof stage.name === "string" && stage.name ? stage.name : stage.id;
  return stage.round ? `${name} (round ${stage.round})` : name;
}

// stageStripItems normalizes a job's derived stage into render-ready entries.
// An absent stage yields an empty list so cards without a workflow stay clean.
export function stageStripItems(stage) {
  if (!stage || !Array.isArray(stage.stages) || !stage.stages.length) return [];
  return stage.stages.map((entry) => ({
    id: entry.id,
    label: stripLabel(entry),
    state: typeof entry.state === "string" && entry.state ? entry.state : "pending",
    round: entry.round || 0,
    tick: entry.state === "complete",
    current: entry.state === "current" || entry.state === "attention",
    attention: entry.state === "attention",
  }));
}

// stageStripText renders the strip as one line, e.g.
// "Queued · Planning ✓ · PR review (round 2) · UAT".
export function stageStripText(stage) {
  return stageStripItems(stage)
    .map((item) => `${item.label}${item.tick ? " ✓" : ""}`)
    .join(" · ");
}

// jobStageLabel is the current stage shown on compact rows (the runs list
// where the full strip would not fit).
export function jobStageLabel(job) {
  const items = stageStripItems(job?.stage);
  if (!items.length) return "";
  const byID = items.find((item) => item.id === job.stage.current);
  if (byID) return byID.label;
  const current = items.find((item) => item.current);
  return current ? current.label : items[items.length - 1].label;
}