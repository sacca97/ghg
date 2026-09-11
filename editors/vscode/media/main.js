// ghg chat webview controller. Rendering of individual blocks lives in
// blocks.js/markdown.js, the shared enum tables in constants.js, and pure
// formatting in util.js; this file owns persisted state, the streaming
// handles, the event dispatch table, and the DOM wiring.
import {
  ROLES, MODES, EFFORT_LEVELS, APPROVALS, SANDBOXES, NETWORKS, BUSY_STATES,
  ROLE_LABELS, MODE_LABELS, EFFORT_LABELS, APPROVAL_LABELS, SANDBOX_LABELS, NETWORK_LABELS,
  isRole, isMode, isEffort, isApproval, isSandbox, isNetwork, fillSelect,
} from "./constants.js";
import { text, formatCount, formatDuration } from "./util.js";
import { renderMarkdown } from "./markdown.js";
import {
  buildBlock, renderToolContent, updateThinkingSummary, addExportButton, sameBlock,
  isInternalReviewTool, reviewExtensionMarkdown, reviewFallbackMarkdown,
} from "./blocks.js";

const vscode = acquireVsCodeApi();
const post = (message) => vscode.postMessage(message);

const settings = document.getElementById("settings");
const settingsToggle = document.getElementById("settings-toggle");
const settingsClose = document.getElementById("settings-close");
const settingsCloseBottom = document.getElementById("settings-close-bottom");
const refreshModels = document.getElementById("refresh-models");
const openSettings = document.getElementById("open-settings");
const openAuth = document.getElementById("open-auth");
const settingsModels = document.getElementById("settings-models");
const settingsRole = document.getElementById("settings-role");
const settingsMode = document.getElementById("settings-mode");
const settingsEffort = document.getElementById("settings-effort");
const settingsSandbox = document.getElementById("settings-sandbox");
const settingsNetwork = document.getElementById("settings-network");
const settingsApproval = document.getElementById("settings-approval");
const configureSearch = document.getElementById("configure-search");
const settingsRuntime = document.getElementById("settings-runtime");
const chatView = document.getElementById("chat-view");
const transcript = document.getElementById("transcript");
const prompt = document.getElementById("prompt");
const status = document.getElementById("status");
const references = document.getElementById("references");
const completionMenu = document.getElementById("completion-menu");
const composer = document.getElementById("composer");
const send = document.getElementById("send");
const role = document.getElementById("role");
const effort = document.getElementById("effort");
const modeToggle = document.getElementById("mode-toggle");

const slashCommands = [
  ["/ask", "answer a question"], ["/cd", "change directory"],
  ["/clear", "reset conversation"], ["/compact", "compact context"], ["/context-doctor", "audit context"],
  ["/effort", "set reasoning effort"], ["/execute", "execute a plan"], ["/goal-from-context", "formulate a goal"],
  ["/help", "show commands"], ["/lsp", "show language server status"], ["/mcp", "manage MCP servers"],
  ["/model", "switch or refresh models"], ["/plan", "enter plan mode"],
  ["/approval", "switch approval mode (ask|auto|never)"],
  ["/notify", "Telegram completion notifications (config|on|off)"],
  ["/continue", "continue an interrupted turn"],
  ["/pwd", "print working directory"], ["/detach", "detach worker"], ["/quit", "exit"], ["/exit", "exit"], ["/q", "exit"], ["/rename", "rename session"],
  ["/resume", "resume a session"], ["/review", "review a target"], ["/commands", "show commands"],
].map(([name, hint]) => ({ name, hint }));

// --- persisted state -------------------------------------------------------

const saved = vscode.getState() || {};
const initialSettings = window.ghgSettings && typeof window.ghgSettings === "object" ? window.ghgSettings : {};
const state = {
  blocks: Array.isArray(saved.blocks) ? saved.blocks : [],
  draft: typeof saved.draft === "string" ? saved.draft : "",
  mode: isMode(saved.mode) ? saved.mode : isMode(initialSettings.mode) ? initialSettings.mode : "execute",
  role: isRole(saved.role) ? saved.role : isRole(initialSettings.role) ? initialSettings.role : "fast",
  effort: isEffort(saved.effort) ? saved.effort : isEffort(initialSettings.effort) ? initialSettings.effort : "",
  approval: isApproval(saved.approval) ? saved.approval : isApproval(initialSettings.approval) ? initialSettings.approval : "",
  sandbox: isSandbox(initialSettings.sandbox) ? initialSettings.sandbox : "",
  network: isNetwork(initialSettings.network) ? initialSettings.network : "",
  models: saved.models && typeof saved.models === "object" ? saved.models : {},
  references: Array.isArray(saved.references) ? saved.references.filter((path) => typeof path === "string") : [],
  proposedPlan: typeof saved.proposedPlan === "string" ? saved.proposedPlan : "",
  settingsOpen: saved.settingsOpen === true,
};

function save() {
  state.draft = prompt.value;
  vscode.setState(state);
}

let active = false;
let pendingUser = false;
let planDeltaSeen = false;
let model = "";
let context = 0;
let contextLimit = 0;
let activity = "Ready";
let turnSince = 0;
let activeReasoningEffort = "";
let turnConfiguredEffort;
let completionRequest = 0;
let completionToken;
let completionItems = [];
let completionIndex = 0;
let completionKind = "reference";
const tools = new Map();
let anonymousTool = 0;

const activityFor = (value) => {
  if (value === "waiting_approval") return "Waiting for approval";
  if (value === "waiting_question") return "Waiting for answer";
  return BUSY_STATES.has(value) ? "Thinking" : "Ready";
};

// --- rendering -------------------------------------------------------------

function renderBlock(block, appendToTranscript = true) {
  const element = buildBlock(block, post);
  if (appendToTranscript) transcript.append(element);
  return element;
}

function rerender() {
  const followNow = nearBottom();
  const scrollTop = transcript.scrollTop;
  const existing = [...transcript.children];
  state.blocks.forEach((block, index) => {
    const current = existing[index];
    if (current?.block && sameBlock(current.block, block)) return;
    const replacement = renderBlock(block, false);
    if (current) current.replaceWith(replacement);
    else transcript.append(replacement);
  });
  for (let index = existing.length - 1; index >= state.blocks.length; index -= 1) {
    existing[index].remove();
  }
  renderReferences();
  syncControls();
  if (followNow) follow();
  else transcript.scrollTop = scrollTop;
  return followNow;
}

function append(block, force = false) {
  // New user input should be visible immediately; background events must
  // respect a user who is reading older messages.
  const followNow = (force && block.kind === "user") || nearBottom();
  state.blocks.push(block);
  const element = renderBlock(block);
  save();
  if (followNow) {
    follow();
  }
  return element;
}

// --- streaming -------------------------------------------------------------
// One open stream per kind. Text, plan, and thinking deltas arrive
// interleaved, so each keeps its own block, element, and follow flag: a delta
// can only ever re-render the block it belongs to.

const streams = { text: undefined, plan: undefined, thinking: undefined };

function nearBottom() {
  return transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 48;
}

function follow() {
  transcript.scrollTop = transcript.scrollHeight;
}

function bindStream(kind, block, followNow) {
  const element = transcript.children[state.blocks.indexOf(block)];
  if (!element) return undefined;
  const stream = { kind, block, element, frame: 0, follow: followNow };
  streams[kind] = stream;
  return stream;
}

function openStream(kind, block) {
  const followNow = nearBottom();
  state.blocks.push(block);
  const stream = { kind, block, element: renderBlock(block), frame: 0, follow: followNow };
  streams[kind] = stream;
  return stream;
}

function streamFor(kind) {
  const existing = streams[kind];
  if (existing) return existing;
  if (kind === "thinking") {
    return openStream(kind, { kind: "thinking", text: "", effort: activeReasoningEffort, startedAt: Date.now() });
  }
  return openStream(kind, { kind: "assistant", text: "" });
}

function appendDelta(kind, delta) {
  const stream = streamFor(kind);
  stream.block.text += delta;
  if (stream.frame) return;
  stream.frame = requestAnimationFrame(() => {
    stream.frame = 0;
    renderMarkdown(stream.element.body, stream.block.text);
    if (stream.follow) follow();
  });
}

function flushStream(kind) {
  const stream = streams[kind];
  if (!stream) return;
  if (stream.frame) {
    cancelAnimationFrame(stream.frame);
    stream.frame = 0;
  }
  renderMarkdown(stream.element.body, stream.block.text);
  if (stream.follow) follow();
  stream.follow = false;
}

function finishThinking(durationMs = 0, effort = "") {
  const stream = streams.thinking;
  if (!stream) return;
  flushStream("thinking");
  const block = stream.block;
  const startedAt = Number(block.startedAt) || Date.now();
  const measured = Number(durationMs) > 0 ? Number(durationMs) : Date.now() - startedAt;
  block.durationMs = Math.max(0, measured);
  block.effort = text(effort).trim() || block.effort || activeReasoningEffort;
  block.finished = true;
  updateThinkingSummary(stream.element, block);
  delete block.startedAt;
  streams.thinking = undefined;
  activeReasoningEffort = "";
}

function flushStreams() {
  flushStream("text");
  flushStream("plan");
  finishThinking();
}

function resetStreams() {
  streams.text = undefined;
  streams.plan = undefined;
  streams.thinking = undefined;
}

// --- status and controls ---------------------------------------------------

function setSendButton(running) {
  send.textContent = running ? "■" : "➤";
  send.setAttribute("aria-label", running ? "Stop" : "Send");
  send.title = running ? "Stop" : "Send";
}

function updateStatus() {
  const parts = [];
  // Activity first: #status is nowrap with an ellipsis, so the turn state and
  // its timer must not be the segment that gets clipped.
  const elapsed = active && turnSince ? formatDuration(Date.now() - turnSince) : "";
  parts.push(active && activity === "Thinking" ? `Thinking ${elapsed}` : active && elapsed ? `${activity} · ${elapsed}` : activity);
  if (model) parts.push(model);
  if (contextLimit > 0) parts.push(`${formatCount(context)}/${formatCount(contextLimit)} ctx`);
  status.textContent = parts.join(" · ");
}

function setupOptionLists() {
  // index.html ships empty selects; every value and label comes from the tables
  // in constants.js so the composer and the settings panel cannot disagree.
  fillSelect(settingsRole, ROLES, ROLE_LABELS);
  fillSelect(settingsMode, MODES, MODE_LABELS);
  fillSelect(settingsEffort, EFFORT_LEVELS, EFFORT_LABELS);
  fillSelect(settingsSandbox, SANDBOXES, SANDBOX_LABELS);
  fillSelect(settingsNetwork, NETWORKS, NETWORK_LABELS);
  fillSelect(settingsApproval, APPROVALS, APPROVAL_LABELS);
  fillSelect(role, ROLES, (name) => state.models[name] || "Configured");
  fillSelect(effort, EFFORT_LEVELS, EFFORT_LABELS);
}

function syncControls() {
  const label = MODE_LABELS[state.mode] || MODE_LABELS.execute;
  modeToggle.textContent = label;
  modeToggle.setAttribute("aria-label", `Mode: ${label}. Click to switch`);
  modeToggle.title = `Switch mode (currently ${label})`;
  role.disabled = false;
  role.value = state.role;
  effort.value = state.effort;
  for (const option of role.options) {
    option.textContent = state.models[option.value] || "Configured";
  }
  settingsRole.value = state.role;
  settingsMode.value = state.mode;
  settingsEffort.value = state.effort;
  settingsSandbox.value = state.sandbox;
  settingsNetwork.value = state.network;
  settingsApproval.value = state.approval;
  renderSettingsModels();
}

function renderSettingsModels() {
  settingsModels.replaceChildren();
  for (const name of ROLES) {
    const row = document.createElement("div");
    row.className = "settings-model-row";
    const label = document.createElement("span");
    label.className = "settings-model-role";
    label.textContent = name;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "settings-model-value";
    button.textContent = state.models[name] || "Not configured";
    button.title = `Configure ${name}`;
    button.addEventListener("click", () => post({ type: "configureModel", role: name, mode: state.mode }));
    row.append(label, button);
    settingsModels.append(row);
  }
}

function setSettings(open) {
  settings.hidden = !open;
  chatView.hidden = open;
  settingsToggle.setAttribute("aria-expanded", String(open));
  state.settingsOpen = open;
  save();
  if (open) hideCompletions();
}

function closeSettings() {
  setSettings(false);
  settingsToggle.focus();
}

// --- snapshots -------------------------------------------------------------

function snapshotMessages(messages) {
  if (!Array.isArray(messages)) return [];
  return messages.flatMap((message) => {
    if (!message || typeof message !== "object") return [];
    if (message.role === "user" && typeof message.content === "string") return [{ kind: "user", text: message.content }];
    if (message.role === "assistant") {
      const blocks = [];
      const content = text(message.content);
      const plan = content.match(/<proposed_plan>\s*([\s\S]*?)\s*<\/proposed_plan>/);
      const visible = plan ? content.replace(plan[0], "").trim() : content;
      if (visible) blocks.push({ kind: "assistant", text: visible });
      if (plan && plan[1].trim()) blocks.push({ kind: "assistant", text: plan[1].trim(), resultKind: "plan" });
      for (const call of Array.isArray(message.tool_calls) ? message.tool_calls : []) {
        if (!call || typeof call !== "object" || !call.function || typeof call.function !== "object") continue;
        const name = text(call.function.name);
        if (name === "request_review_extension") {
          const markdown = reviewExtensionMarkdown(text(call.function.arguments));
          if (markdown) blocks.push({ kind: "assistant", text: markdown });
          continue;
        }
        if (isInternalReviewTool(name)) {
          const markdown = reviewFallbackMarkdown(text(call.function.arguments));
          if (markdown) blocks.push({ kind: "assistant", text: markdown });
          continue;
        }
        blocks.push({ kind: "tool", name, args: text(call.function.arguments), failed: false });
      }
      return blocks;
    }
    return [];
  });
}

function applySnapshot(snapshot) {
  if (!snapshot || typeof snapshot !== "object") return;
  flushStreams();
  const nextModel = snapshot.model_name || snapshot.model;
  if (typeof nextModel === "string") model = nextModel;
  if (typeof snapshot.context_tokens === "number") context = snapshot.context_tokens;
  if (typeof snapshot.context_limit === "number") contextLimit = snapshot.context_limit;
  if (typeof snapshot.effort === "string" && isEffort(snapshot.effort)) state.effort = snapshot.effort;
  if (typeof snapshot.approval === "string" && isApproval(snapshot.approval)) state.approval = snapshot.approval;
  if (isRole(snapshot.role) && typeof nextModel === "string") state.models[snapshot.role] = nextModel;
  if (snapshot.mode === "plan") state.mode = "plan";
  if (snapshot.mode === "execute" && state.mode !== "review") state.mode = "execute";
  active = BUSY_STATES.has(snapshot.state);
  if (active && !turnSince) turnSince = Date.now();
  if (!active) turnSince = 0;
  activity = activityFor(snapshot.state);
  let liveText;
  let livePlan;
  let liveThinking;
  if (!pendingUser) {
    state.blocks = snapshotMessages(Array.isArray(snapshot.messages) ? snapshot.messages : []);
    if (snapshot.live_text) {
      liveText = { kind: "assistant", text: text(snapshot.live_text) };
      state.blocks.push(liveText);
    }
    if (snapshot.live_plan) {
      livePlan = { kind: "assistant", text: text(snapshot.live_plan) };
      state.blocks.push(livePlan);
    }
    if (snapshot.live_think) {
      liveThinking = { kind: "thinking", text: text(snapshot.live_think), effort: text(snapshot.effort).trim() || activeReasoningEffort, startedAt: Date.now() };
      state.blocks.push(liveThinking);
    }
    if (snapshot.active_tool) state.blocks.push({ kind: "tool", name: text(snapshot.active_tool), args: "", failed: false });
    if (Array.isArray(snapshot.tasks)) {
      for (const task of snapshot.tasks) {
        if (task && typeof task === "object" && typeof task.description === "string") {
          state.blocks.push({ kind: "notice", text: `Task ${task.status || "unknown"}: ${task.description}` });
        }
      }
    }
  }
  resetStreams();
  const followNow = rerender();
  setSendButton(active);
  send.classList.toggle("stop", active);
  // Each live stream rebinds to its own block, so a later delta updates the
  // text and the plan independently instead of both landing in whichever block
  // the snapshot happened to push last.
  if (active && liveText) bindStream("text", liveText, followNow);
  if (active && livePlan) bindStream("plan", livePlan, followNow);
  if (active && liveThinking) bindStream("thinking", liveThinking, followNow);
  save();
  updateStatus();
}

// --- references and completions -------------------------------------------

function showCommandResult(name, payload) {
  if (payload && typeof payload === "object" && payload.accepted) return;
  if (name === "lsp_status" || name === "mcp_status") {
    const items = Array.isArray(payload) ? payload : [];
    append({ kind: "notice", text: items.length ? items.map((item) => JSON.stringify(item)).join("\n") : "No entries." }, true);
    return;
  }
  if (name === "context_doctor" && payload && typeof payload.report === "string") {
    append({ kind: "notice", text: payload.report }, true);
    return;
  }
  if (name === "chdir" && payload && typeof payload.cwd === "string") {
    append({ kind: "notice", text: payload.cwd }, true);
    return;
  }
  if (name === "rename" && payload && typeof payload.title === "string") {
    append({ kind: "notice", text: `Session renamed to ${payload.title}` }, true);
  }
}

function renderReferences() {
  references.replaceChildren();
  for (const path of state.references) {
    const item = document.createElement("span");
    item.className = "reference";
    const label = document.createElement("span");
    label.textContent = path;
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "reference-remove";
    remove.setAttribute("aria-label", `Remove ${path}`);
    remove.title = "Remove reference";
    remove.textContent = "×";
    remove.addEventListener("click", () => {
      state.references = state.references.filter((value) => value !== path);
      renderReferences();
      save();
    });
    item.append(label, remove);
    references.append(item);
  }
}

function referenceToken() {
  const before = prompt.value.slice(0, prompt.selectionStart);
  const match = before.match(/(?:^|\s)@([^\s]*)$/);
  if (!match) return;
  return { query: match[1], start: before.length - match[1].length - 1, end: before.length };
}

function hideCompletions() {
  completionMenu.hidden = true;
  prompt.setAttribute("aria-expanded", "false");
  prompt.removeAttribute("aria-activedescendant");
  completionToken = undefined;
  completionItems = [];
  completionKind = "reference";
}

function updateCompletionSelection() {
  const options = completionMenu.querySelectorAll(".completion");
  options.forEach((item, index) => {
    const selected = index === completionIndex;
    item.classList.toggle("selected", selected);
    item.setAttribute("aria-selected", String(selected));
  });
  const selected = options[completionIndex];
  if (selected) prompt.setAttribute("aria-activedescendant", selected.id);
}

function chooseCompletion(item) {
  if (!completionToken) return;
  const before = prompt.value.slice(0, completionToken.start);
  const after = prompt.value.slice(completionToken.end);
  const replacement = `@${item.path}`;
  prompt.value = `${before}${replacement}${after}`;
  if (!state.references.includes(item.path)) state.references.push(item.path);
  hideCompletions();
  renderReferences();
  save();
  prompt.focus();
  prompt.selectionStart = prompt.selectionEnd = before.length + replacement.length;
}

function chooseCommand(item) {
  if (!completionToken) return;
  const before = prompt.value.slice(0, completionToken.start);
  const after = prompt.value.slice(completionToken.end);
  prompt.value = `${before}${item.name} ${after}`;
  hideCompletions();
  save();
  prompt.focus();
  prompt.selectionStart = prompt.selectionEnd = before.length + item.name.length + 1;
}

function renderCompletionOptions(items, label) {
  completionItems = items;
  completionIndex = 0;
  completionMenu.replaceChildren();
  if (completionItems.length === 0) {
    hideCompletions();
    return;
  }
  completionItems.forEach((item, index) => {
    const option = document.createElement("button");
    option.type = "button";
    option.className = "completion";
    option.id = `completion-${completionKind}-${index}`;
    option.setAttribute("role", "option");
    option.textContent = label(item);
    option.addEventListener("mousedown", (event) => event.preventDefault());
    option.addEventListener("click", () => (completionKind === "command" ? chooseCommand(item) : chooseCompletion(item)));
    completionMenu.append(option);
  });
  completionMenu.hidden = false;
  prompt.setAttribute("aria-expanded", "true");
  updateCompletionSelection();
}

function renderCompletions(items) {
  completionKind = "reference";
  renderCompletionOptions(
    items.filter((item) => item && typeof item.path === "string"),
    (item) => `${item.path}${item.folder ? "  folder" : ""}`,
  );
}

function commandToken() {
  const before = prompt.value.slice(0, prompt.selectionStart);
  const match = before.match(/^(\/[^\s]*)$/);
  if (!match) return;
  return { query: match[1], start: before.length - match[1].length, end: before.length };
}

function requestCompletions() {
  const command = commandToken();
  if (command) {
    completionToken = { ...command, kind: "command" };
    completionKind = "command";
    const query = command.query.toLowerCase();
    renderCompletionOptions(
      slashCommands.filter((item) => item.name.startsWith(query)),
      (item) => `${item.name} — ${item.hint}`,
    );
    return;
  }
  const token = referenceToken();
  if (!token) {
    hideCompletions();
    return;
  }
  completionToken = token;
  const requestId = ++completionRequest;
  post({ type: "completeReferences", requestId, query: token.query });
}

// --- worker events ---------------------------------------------------------
// One named handler per event type the bridge forwards. Anything else is
// ignored, exactly as the previous switch's missing default case did.

const callEffort = (raw) => {
  let effort = text(raw.effort_applied).trim() || text(raw.reasoning_effort).trim();
  if (!effort && raw.reasoning_enabled === true) effort = "on";
  return effort === "off" || effort === "default" ? "" : effort;
};

const handlers = {
  turn_start() {
    active = true;
    planDeltaSeen = false;
    resetStreams();
    activity = "Thinking";
    turnSince = Date.now();
    activeReasoningEffort = "";
    setSendButton(true);
    send.classList.add("stop");
    updateStatus();
  },

  turn_end() {
    flushStreams();
    if (typeof turnConfiguredEffort === "string") {
      state.effort = turnConfiguredEffort;
      turnConfiguredEffort = undefined;
      syncControls();
    }
    pendingUser = false;
    planDeltaSeen = false;
    tools.clear();
    active = false;
    resetStreams();
    turnSince = 0;
    activeReasoningEffort = "";
    activity = "Ready";
    setSendButton(false);
    send.classList.remove("stop");
    updateStatus();
    save();
  },

  busy(raw) {
    // The host owns the busy state: a dropped or out-of-order event can no
    // longer leave the button showing Send while a turn is still running.
    active = raw.value === true;
    if (active && !turnSince) turnSince = Date.now();
    if (!active) turnSince = 0;
    if (active && activity === "Ready") activity = "Thinking";
    setSendButton(active);
    send.classList.toggle("stop", active);
    updateStatus();
  },

  snapshot(raw) {
    applySnapshot(raw.snapshot);
  },

  route(raw) {
    const nextModel = raw.model_name || raw.model;
    if (typeof nextModel === "string") model = nextModel;
    if (isRole(raw.role) && typeof nextModel === "string") state.models[raw.role] = nextModel;
    if (raw.mode === "plan") state.mode = "plan";
    if (raw.mode === "execute" && state.mode !== "review") state.mode = "execute";
    if (isEffort(raw.effort)) state.effort = raw.effort;
    if (isApproval(raw.approval)) state.approval = raw.approval;
    syncControls();
    updateStatus();
    save();
  },

  role_model(raw) {
    if (isRole(raw.role) && typeof raw.model === "string") {
      state.role = raw.role;
      state.models[raw.role] = raw.model;
      syncControls();
      save();
    }
  },

  text(raw) {
    if (typeof raw.delta !== "string") return;
    appendDelta("text", raw.delta);
    activity = "Responding";
    updateStatus();
  },

  plan_delta(raw) {
    if (typeof raw.delta !== "string") return;
    planDeltaSeen = true;
    appendDelta("plan", raw.delta);
    activity = "Responding";
    updateStatus();
  },

  think(raw) {
    if (typeof raw.delta !== "string") return;
    appendDelta("thinking", raw.delta);
    activity = "Thinking";
    updateStatus();
  },

  tool_start(raw) {
    finishThinking();
    // A tool call ends the current assistant segment: the next delta opens a
    // new block after the tool chip.
    streams.text = undefined;
    streams.plan = undefined;
    const name = text(raw.name);
    if (isInternalReviewTool(name)) {
      activity = "Thinking";
      updateStatus();
      return;
    }
    if (name === "request_review_extension") {
      const markdown = reviewExtensionMarkdown(text(raw.args));
      if (markdown) append({ kind: "assistant", text: markdown }, true);
      else append({ kind: "tool", name, args: text(raw.args), failed: false });
      activity = "Thinking";
      updateStatus();
      return;
    }
    const block = { kind: "tool", name, args: text(raw.args), failed: false };
    const element = append(block);
    const id = typeof raw.id === "string" && raw.id ? raw.id : `anonymous-${++anonymousTool}`;
    tools.set(id, { block, element });
    activity = `Using ${block.name || "tool"}`;
    updateStatus();
  },

  tool_end(raw) {
    const id = typeof raw.id === "string" ? raw.id : "";
    const entry = tools.get(id);
    if (entry) {
      entry.block.failed = typeof raw.result === "string" && /^(error|failed)/i.test(raw.result.trim());
      renderToolContent(entry.element, entry.block, text(raw.result));
      tools.delete(id);
      save();
    }
    activity = "Thinking";
    updateStatus();
  },

  state(raw) {
    active = BUSY_STATES.has(raw.state);
    if (active && !turnSince) turnSince = Date.now();
    if (!active) turnSince = 0;
    activity = activityFor(raw.state);
    updateStatus();
  },

  retry(raw) {
    append({ kind: "notice", text: `⚠ retrying request (attempt ${Number(raw.attempt || 0) + 1}/${raw.max || "?"})` }, true);
  },

  steer(raw) {
    if (typeof raw.text === "string") append({ kind: "user", text: `${raw.text} (steered)` }, true);
  },

  usage(raw) {
    if (typeof raw.prompt_tokens === "number" || typeof raw.completion_tokens === "number") {
      context = (Number(raw.prompt_tokens) || 0) + (Number(raw.completion_tokens) || 0);
      updateStatus();
    }
  },

  task(raw) {
    if (typeof raw.description === "string" && typeof raw.status === "string") {
      append({ kind: "notice", text: `Task ${raw.status}: ${raw.description}` }, true);
    }
  },

  goal(raw) {
    const status = text(raw.status || raw.Status);
    const objective = text(raw.objective || raw.Objective);
    if (status || objective) append({ kind: "notice", text: `Goal ${status || "updated"}${objective ? `: ${objective}` : ""}` }, true);
  },

  goal_update(raw) {
    const status = text(raw.status || raw.Status);
    const progress = text(raw.progress || raw.Progress);
    const blocker = text(raw.blocker || raw.Blocker);
    const detail = blocker ? `blocked: ${blocker}` : progress;
    if (status || detail) append({ kind: "notice", text: `Goal ${status || "updated"}${detail ? `: ${detail}` : ""}` }, true);
  },

  mcp() {
    append({ kind: "notice", text: "MCP status updated." }, true);
  },

  prompt_view(raw) {
    if (typeof raw.model === "string") model = raw.model;
    if (typeof raw.estimated_tokens === "number") context = raw.estimated_tokens;
    if (typeof raw.context_limit === "number") contextLimit = raw.context_limit;
    updateStatus();
  },

  model_call_start(raw) {
    finishThinking();
    if (typeof raw.model === "string") model = raw.model;
    activeReasoningEffort = callEffort(raw);
    if (!raw.purpose && (typeof raw.configured_effort === "string" || typeof raw.effort_applied === "string" || typeof raw.selection_reason === "string")) {
      if (turnConfiguredEffort === undefined && typeof raw.configured_effort === "string") {
        turnConfiguredEffort = raw.configured_effort;
      }
      const applied = activeReasoningEffort;
      if (isEffort(applied)) {
        state.effort = applied;
        syncControls();
      }
    }
    updateStatus();
  },

  model_call_end(raw) {
    finishThinking(raw.latency_ms, callEffort(raw));
    updateStatus();
  },

  models(raw) {
    refreshModels.disabled = false;
    if (raw.models && typeof raw.models === "object" && !Array.isArray(raw.models)) {
      for (const name of ROLES) {
        if (typeof raw.models[name] === "string" && raw.models[name]) state.models[name] = raw.models[name];
      }
      syncControls();
      save();
    }
  },

  extensionSettings(raw) {
    const value = raw.settings;
    if (!value || typeof value !== "object") return;
    if (isSandbox(value.sandbox)) state.sandbox = value.sandbox;
    if (isNetwork(value.network)) state.network = value.network;
    if (isApproval(value.approval)) state.approval = value.approval;
    if (typeof value.binary === "string" && value.binary) settingsRuntime.textContent = `Binary: ${value.binary}`;
    syncControls();
  },

  turn_done(raw) {
    const visible = state.blocks.filter((block) => block.kind !== "tool" || !isInternalReviewTool(block.name));
    if (visible.length !== state.blocks.length) {
      state.blocks = visible;
      rerender();
    }
    if (typeof raw.plan === "string" && raw.plan) {
      state.proposedPlan = raw.plan;
      if (!planDeltaSeen) {
        append({ kind: "assistant", text: raw.plan, resultKind: "plan" }, true);
      } else if (streams.plan) {
        streams.plan.block.resultKind = "plan";
        addExportButton(streams.plan.element, streams.plan.block, post);
      }
    }
    const reviewMarkdown = text(raw.review_markdown) || reviewFallbackMarkdown(raw.review) || reviewFallbackMarkdown(raw.final);
    if (reviewMarkdown) {
      append({ kind: "assistant", text: reviewMarkdown, resultKind: "review" }, true);
      save();
    }
    if (typeof raw.error === "string" && raw.error) append({ kind: "error", text: raw.error }, true);
    if (typeof raw.effort === "string" && isEffort(raw.effort)) {
      state.effort = raw.effort;
      syncControls();
    } else if (typeof turnConfiguredEffort === "string") {
      state.effort = turnConfiguredEffort;
      syncControls();
    }
  },

  compact_done(raw) {
    if (typeof raw.error === "string" && raw.error) append({ kind: "error", text: raw.error }, true);
    else append({ kind: "notice", text: "Context compacted." }, true);
  },

  compact() {
    append({ kind: "notice", text: "Context compaction completed; raw history preserved." }, true);
  },

  goal_from_context(raw) {
    if (raw.goal && typeof raw.goal.objective === "string") append({ kind: "notice", text: `Goal: ${raw.goal.objective}` }, true);
  },

  ack(raw) {
    showCommandResult(text(raw.name), raw.payload);
  },

  shell_done(raw) {
    if (raw.output) append({ kind: "notice", text: text(raw.output) }, true);
  },

  notice(raw) {
    if (typeof raw.text === "string") append({ kind: "notice", text: raw.text });
  },

  error(raw) {
    refreshModels.disabled = false;
    flushStreams();
    pendingUser = false;
    append({ kind: "error", text: text(raw.error) || "ghg failed" }, true);
    activity = "Error";
    if (active) {
      active = false;
      turnSince = 0;
      activeReasoningEffort = "";
      setSendButton(false);
      send.classList.remove("stop");
    }
    updateStatus();
  },

  referenceSuggestions(raw) {
    if (raw.requestId === completionRequest && Array.isArray(raw.items)) renderCompletions(raw.items);
  },

  new_session() {
    pendingUser = false;
    state.blocks = [];
    state.references = [];
    resetStreams();
    turnSince = 0;
    activeReasoningEffort = "";
    rerender();
    save();
  },

  resume_session() {
    append({ kind: "notice", text: "Session resumed." }, true);
  },
};

function handleEvent(raw) {
  if (!raw || typeof raw !== "object" || typeof raw.type !== "string") return;
  const handler = Object.hasOwn(handlers, raw.type) ? handlers[raw.type] : undefined;
  if (handler) handler(raw);
}

// --- DOM wiring ------------------------------------------------------------

composer.addEventListener("submit", (event) => {
  event.preventDefault();
  if (!prompt.value.trim()) return;
  if (!active) {
    append({ kind: "user", text: prompt.value.trim() }, true);
    pendingUser = true;
  }
  const message = { type: "send", prompt: prompt.value, mode: state.mode, role: state.role, references: state.references, plan: state.proposedPlan };
  prompt.value = "";
  state.references = [];
  save();
  post(message);
});
prompt.addEventListener("input", () => { save(); requestCompletions(); });
prompt.addEventListener("keydown", (event) => {
  if (!completionMenu.hidden) {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      completionIndex = (completionIndex + (event.key === "ArrowDown" ? 1 : completionItems.length - 1)) % completionItems.length;
      updateCompletionSelection();
      completionMenu.children[completionIndex]?.scrollIntoView({ block: "nearest" });
      return;
    }
    if (event.key === "Enter" || event.key === "Tab") {
      event.preventDefault();
      const item = completionItems[completionIndex];
      if (completionKind === "command") {
        if (event.key === "Enter" && item.name === prompt.value.trim()) {
          hideCompletions();
          composer.requestSubmit();
        } else {
          chooseCommand(item);
        }
      } else {
        chooseCompletion(item);
      }
      return;
    }
    if (event.key === "Escape") {
      event.preventDefault();
      hideCompletions();
      return;
    }
  }
  if (event.key === "Escape" && active) {
    event.preventDefault();
    post({ type: "stop" });
    return;
  }
  if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
    event.preventDefault();
    composer.requestSubmit();
  }
});
modeToggle.addEventListener("click", () => {
  state.mode = MODES[(MODES.indexOf(state.mode) + 1) % MODES.length];
  syncControls();
  save();
  post({ type: "configureRole", role: state.role, mode: state.mode });
});
settingsToggle.addEventListener("click", () => {
  const open = settings.hidden;
  setSettings(open);
  if (open) settingsClose.focus();
});
settingsClose.addEventListener("click", closeSettings);
settingsCloseBottom.addEventListener("click", closeSettings);
refreshModels.addEventListener("click", () => {
  refreshModels.disabled = true;
  post({ type: "refreshModels" });
});
openSettings.addEventListener("click", () => post({ type: "openSettings" }));
openAuth.addEventListener("click", () => post({ type: "openAuth" }));
configureSearch?.addEventListener("click", () => post({ type: "configureSearchProvider" }));
send.addEventListener("click", (event) => {
  if (active) {
    event.preventDefault();
    post({ type: "stop" });
  }
});
role.addEventListener("change", () => {
  if (!isRole(role.value)) return;
  state.role = role.value;
  save();
  post({ type: "configureRole", role: state.role, mode: state.mode });
});
effort.addEventListener("change", () => {
  if (!isEffort(effort.value)) return;
  state.effort = effort.value;
  save();
  post({
    type: "configureRole",
    role: state.role,
    mode: state.mode,
    effort: state.effort,
    updateEffort: true,
  });
});
settingsRole.addEventListener("change", () => {
  if (!isRole(settingsRole.value)) return;
  state.role = settingsRole.value;
  syncControls();
  save();
  post({ type: "setDefaultSetting", name: "defaultRole", value: state.role });
  post({ type: "configureRole", role: state.role, mode: state.mode });
});
settingsMode.addEventListener("change", () => {
  if (!isMode(settingsMode.value)) return;
  state.mode = settingsMode.value;
  syncControls();
  save();
  post({ type: "setDefaultSetting", name: "defaultMode", value: state.mode });
  post({ type: "configureRole", role: state.role, mode: state.mode });
});
settingsEffort.addEventListener("change", () => {
  if (!isEffort(settingsEffort.value)) return;
  state.effort = settingsEffort.value;
  syncControls();
  save();
  post({ type: "setDefaultSetting", name: "defaultEffort", value: state.effort });
  post({ type: "configureRole", role: state.role, mode: state.mode, effort: state.effort, updateEffort: true });
});
for (const input of [settingsSandbox, settingsNetwork, settingsApproval]) {
  input.addEventListener("change", () => {
    const name = input.id.replace("settings-", "");
    state[name] = input.value;
    save();
    post({ type: "setExecutionSetting", name, value: input.value });
  });
}
transcript.addEventListener("click", (event) => {
  const target = event.target;
  const anchor = target instanceof Element ? target.closest("a[href]") : null;
  if (!anchor) return;
  event.preventDefault();
  post({ type: "openExternal", uri: anchor.href });
});
transcript.addEventListener("scroll", () => {
  const atBottom = nearBottom();
  for (const stream of Object.values(streams)) {
    if (stream) stream.follow = atBottom;
  }
}, { passive: true });
window.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !settings.hidden) closeSettings();
});
window.addEventListener("message", (event) => handleEvent(event.data));
setInterval(() => { if (active) updateStatus(); }, 1000);

setupOptionLists();
prompt.value = state.draft;
setSettings(state.settingsOpen);
rerender();
setSendButton(false);
updateStatus();
post({ type: "ready" });
