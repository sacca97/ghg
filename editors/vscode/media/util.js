// Pure text and number formatting helpers shared by the block builders and the
// status line. No DOM access above the textContent level, no module state.
export function text(value) {
  return typeof value === "string" ? value : "";
}

export function hideToolDisplayHash(value) {
  return text(value).replace(/sha256:[0-9a-f]{8,}/gi, (match) =>
    match.length > 15 ? `${match.slice(0, 15)}…` : match);
}

export function formatCount(value) {
  if (!Number.isFinite(value) || value <= 0) return "0";
  if (value >= 1000000) return `${(value / 1000000).toFixed(1)}M`;
  if (value >= 1000) return `${(value / 1000).toFixed(1)}k`;
  return String(Math.round(value));
}

export function formatDuration(milliseconds) {
  const seconds = Math.max(0, Number(milliseconds) || 0) / 1000;
  if (seconds < 1) return `${Math.round(seconds * 1000)}ms`;
  if (seconds < 60) return `${seconds.toFixed(1)}s`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes}m ${Math.floor(seconds % 60)}s`;
}
