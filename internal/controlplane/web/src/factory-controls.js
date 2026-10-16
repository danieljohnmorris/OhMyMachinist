// Factory controls presentation helpers. Kept free of React so they can be
// unit-tested with plain node:test.

// factoryStateLabel maps the dispatch state to operator-facing text.
export function factoryStateLabel(state) {
  switch (state) {
    case "paused":
      return "Paused";
    case "draining":
      return "Draining";
    default:
      return "Running";
  }
}

// factoryStateSummary explains a stop: who stopped it, why and when.
export function factoryStateSummary(factory, relative = () => "") {
  if (!factory || factory.state === "running" || !factory.state) return "";
  const parts = [];
  if (factory.actor) parts.push(`by ${factory.actor}`);
  if (factory.reason) parts.push(`“${factory.reason}”`);
  if (factory.since) parts.push(`since ${relative(factory.since)}`);
  if (!parts.length) return "";
  return `Stopped ${parts.join(" ")}`;
}

// laneStateLabel names one lane slot: busy, idle, offline or disabled.
export function laneStateLabel(lane) {
  switch (lane?.state) {
    case "busy":
      return "Busy";
    case "offline":
      return "Offline";
    case "disabled":
      return "Disabled";
    default:
      return "Idle";
  }
}

// laneTone maps a lane to a badge tone class.
export function laneTone(lane) {
  switch (lane?.state) {
    case "busy":
      return "tone-busy";
    case "offline":
      return "tone-offline";
    case "disabled":
      return "tone-disabled";
    default:
      return "tone-idle";
  }
}

// jobCapMax is the highest cap an operator can pick: the ready (connected and
// enabled) lanes, never below one.
export function jobCapMax(factory) {
  return Math.max(1, factory?.ready_lanes ?? 0);
}

// stepJobCap clamps a stepper move into [1, jobCapMax].
export function stepJobCap(current, delta, factory) {
  const max = jobCapMax(factory);
  const next = (Number.isFinite(current) ? current : 1) + delta;
  return Math.min(max, Math.max(1, next));
}