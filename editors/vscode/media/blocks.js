// Builders for one transcript block. Everything here is DOM construction from a
// block record: no transcript, scroll, or turn state is touched, so the caller
// owns where the result is inserted (see renderBlock in main.js).
import { text, formatDuration, hideToolDisplayHash } from "./util.js";
import { renderMarkdown } from "./markdown.js";
import { copyButton, exportButton } from "./widgets.js";

// A tool preview is already bounded by the worker; this only keeps the DOM from
// carrying a 16k-character blob into the collapsed output panel.
const TOOL_RESULT_LIMIT = 4000;

/**
 * @typedef {HTMLElement & { block?: object, body?: HTMLElement }} BlockElement
 * buildBlock returns a BlockElement: the article plus the block record it was
 * rendered from (so an unchanged block can be skipped) and, for markdown
 * blocks, the body element that streaming deltas re-render into.
 */

function compactToolArgs(name, args) {
  let parsed;
  try {
    parsed = JSON.parse(args);
  } catch (_) {
    return text(args).trim();
  }
  if (!parsed || typeof parsed !== "object") return text(args).trim();
  if (name === "read") {
    const ranges = Array.isArray(parsed.ranges)
      ? parsed.ranges
      : parsed.path ? [parsed] : [];
    return ranges.map((range) => {
      const path = text(range.path).trim();
      if (!path) return "";
      const offset = Number(range.offset) > 0 ? Number(range.offset) : 1;
      const limit = Number(range.limit) > 0 ? Number(range.limit) : 0;
      return limit ? `${path}:${offset}-${offset + limit - 1}` : `${path}:${offset}`;
    }).filter(Boolean).join(", ");
  }
  if (text(parsed.command).trim()) return text(parsed.command).trim();
  const parts = [];
  if (text(parsed.path).trim()) parts.push(text(parsed.path).trim());
  if (text(parsed.pattern).trim()) parts.push(text(parsed.pattern).trim());
  else if (text(parsed.query).trim()) parts.push(text(parsed.query).trim());
  return parts.length ? parts.join(" ") : text(args).trim();
}

export function renderToolContent(element, block, result = "") {
  element.replaceChildren();
  const label = document.createElement("span");
  label.textContent = `⚒ ${block.name || "tool"}`;
  element.append(label);
  const summary = hideToolDisplayHash(compactToolArgs(block.name, block.args));
  if (summary) {
    const args = document.createElement("code");
    args.textContent = ` ${summary}`;
    element.append(args);
  }
  if (block.failed) {
    const failed = document.createElement("span");
    failed.className = "failed";
    failed.textContent = " — failed";
    element.append(failed);
  }
  // The worker keeps the model's view of a result; the UI renders a capped copy
  // the user can open instead of guessing from the one-line chip. It is rendered
  // into the element rather than stored on the block, so a multi-kilobyte result
  // never lands in the persisted webview state.
  const output = text(result).trim();
  if (output) {
    const truncated = output.length > TOOL_RESULT_LIMIT;
    const details = document.createElement("details");
    details.className = "tool-result";
    const heading = document.createElement("summary");
    heading.textContent = truncated ? "Output · truncated" : "Output";
    const pre = document.createElement("pre");
    pre.textContent = truncated ? `${output.slice(0, TOOL_RESULT_LIMIT)}\n…` : output;
    details.append(heading, pre);
    element.append(details);
  }
}

export function isInternalReviewTool(name) {
  return name === "submit_review";
}

export function reviewExtensionMarkdown(args) {
  let parsed;
  try {
    parsed = JSON.parse(args);
  } catch (_) {
    return "";
  }
  if (!parsed || typeof parsed !== "object") return "";
  let reason = text(parsed.reason).trim().toLowerCase();
  let area = text(parsed.remaining_area).trim();
  let why = text(parsed.why_it_matters).trim();
  const lookup = text(parsed.remaining_lookup).trim();
  if (!reason && !area && !why) {
    reason = "unresolved_finding";
    area = text(parsed.unresolved_issue).trim();
    why = text(parsed.evidence).trim();
  }
  if (!["incomplete_coverage", "unresolved_finding"].includes(reason) || !area || !why || !lookup) return "";
  return `### Review extension · ${reason.replaceAll("_", " ")}\n\n**Area:** ${area}\n\n**Why:** ${why}\n\n**Next:** ${lookup}`;
}

export function reviewFallbackMarkdown(value) {
  let review;
  try {
    review = JSON.parse(text(value));
  } catch (_) {
    return "";
  }
  if (!review || typeof review !== "object") return "";
  const verdict = text(review.verdict).trim().toUpperCase();
  const summary = text(review.summary).trim();
  if (!verdict || !summary) return "";
  let markdown = `# Review: ${verdict}\n\n${summary}\n`;
  if (Array.isArray(review.checks_performed) && review.checks_performed.length) {
    const checks = review.checks_performed.filter((check) => typeof check === "string");
    if (checks.length) markdown += `\n## Checks performed\n\n${checks.map((check) => `- ${check}`).join("\n")}\n`;
  }
  if (Array.isArray(review.findings) && review.findings.length) {
    markdown += "\n## Findings\n";
    for (const finding of review.findings) {
      if (!finding || typeof finding !== "object") continue;
      const severity = text(finding.severity).trim().toUpperCase();
      const title = text(finding.title).trim();
      if (!severity || !title) continue;
      markdown += `\n### [${severity}] ${title}\n`;
      const file = text(finding.file).trim();
      if (file) markdown += `\n- **Location**: \`${file}${Number(finding.line) > 0 ? `:${Number(finding.line)}` : ""}\`\n`;
      if (text(finding.evidence).trim()) markdown += `- **Evidence**: ${text(finding.evidence).trim()}\n`;
      if (text(finding.recommendation).trim()) markdown += `- **Recommendation**: ${text(finding.recommendation).trim()}\n`;
    }
  }
  return markdown.trim();
}

export function exportKind(value) {
  return value === "plan" || value === "review" ? value : "";
}

export function addExportButton(element, block, post) {
  const kind = exportKind(block.resultKind);
  if (!kind) return;
  const actions = element?.querySelector(".block-actions");
  if (actions && !actions.querySelector(".export")) actions.append(exportButton(kind, post));
}

function thinkingLabel(block) {
  let label = block.finished ? "Reasoning" : "Reasoning…";
  if (Number(block.durationMs) > 0) label += ` · ${formatDuration(block.durationMs)}`;
  if (text(block.effort).trim()) label += ` · ${block.effort}`;
  return label;
}

export function updateThinkingSummary(element, block) {
  const summary = element?.querySelector("summary");
  if (summary) summary.textContent = thinkingLabel(block);
}

export function sameBlock(left, right) {
  if (!left || !right) return false;
  return ["kind", "text", "name", "args", "failed", "resultKind", "effort", "durationMs", "finished"]
    .every((key) => left[key] === right[key]);
}

export function buildBlock(block, post) {
  const element = document.createElement("article");
  element.className = `block ${block.kind || "notice"}`;
  element.block = block;
  if (block.kind === "user") {
    const body = document.createElement("pre");
    body.textContent = text(block.text);
    element.append(body);
  } else if (block.kind === "assistant") {
    const body = document.createElement("div");
    body.className = "markdown";
    renderMarkdown(body, text(block.text));
    const actions = document.createElement("div");
    actions.className = "block-actions";
    actions.append(copyButton("Copy response", () => text(block.text)));
    element.append(body, actions);
    addExportButton(element, block, post);
    element.body = body;
  } else if (block.kind === "thinking") {
    const details = document.createElement("details");
    details.className = "thinking-details";
    const summary = document.createElement("summary");
    summary.className = "thinking-summary";
    summary.textContent = thinkingLabel(block);
    const body = document.createElement("div");
    body.className = "markdown";
    renderMarkdown(body, text(block.text));
    details.append(summary, body);
    element.append(details);
    element.body = body;
  } else if (block.kind === "tool") {
    renderToolContent(element, block);
  } else {
    element.textContent = text(block.text);
  }
  return element;
}
