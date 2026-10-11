const zeroTime = "0001-01-01T00:00:00Z";

export function relativeTime(value, now = Date.now()) {
  if (!value || value === zeroTime) return "Not started";
  const reference = now instanceof Date ? now.getTime() : now;
  const seconds = Math.max(
    0,
    Math.floor((reference - Date.parse(value)) / 1000),
  );
  if (seconds < 10) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}
