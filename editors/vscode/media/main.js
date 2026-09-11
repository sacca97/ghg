(() => {
  const vscode = acquireVsCodeApi();
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
  const modeLabels = { execute: "Execute", plan: "Plan", review: "Review" };
  const effortLevels = ["", "low", "medium", "high"];
  const busyStates = new Set(["running", "waiting_approval", "waiting_question", "stopping"]);
  const saved = vscode.getState() || {};
  const initialSettings = window.ghgSettings && typeof window.ghgSettings === "object" ? window.ghgSettings : {};
  const validRole = (value) => ["default", "smart", "tiny", "fast"].includes(value);
  const validMode = (value) => ["execute", "plan", "review"].includes(value);
  const validExecution = (value, values) => typeof value === "string" && values.includes(value);
  const state = {
    blocks: Array.isArray(saved.blocks) ? saved.blocks : [],
    draft: typeof saved.draft === "string" ? saved.draft : "",
    mode: validMode(saved.mode) ? saved.mode : validMode(initialSettings.mode) ? initialSettings.mode : "execute",
    role: validRole(saved.role) ? saved.role : validRole(initialSettings.role) ? initialSettings.role : "fast",
    effort: effortLevels.includes(saved.effort) ? saved.effort : effortLevels.includes(initialSettings.effort) ? initialSettings.effort : "",
    approval: validExecution(saved.approval, ["", "ask", "auto-review", "never"]) ? saved.approval : validExecution(initialSettings.approval, ["", "ask", "auto-review", "never"]) ? initialSettings.approval : "",
    sandbox: validExecution(initialSettings.sandbox, ["", "read-only", "workspace-write", "danger-full-access"]) ? initialSettings.sandbox : "",
    network: validExecution(initialSettings.network, ["", "deny", "host"]) ? initialSettings.network : "",
    models: saved.models && typeof saved.models === "object" ? saved.models : {},
    references: Array.isArray(saved.references) ? saved.references.filter((path) => typeof path === "string") : [],
    proposedPlan: typeof saved.proposedPlan === "string" ? saved.proposedPlan : "",
    settingsOpen: saved.settingsOpen === true,
  };
  let active = false;
  let pendingUser = false;
  let planDeltaSeen = false;
  let currentAssistant;
  let currentAssistantElement;
  let currentThinking;
  let currentThinkingElement;
  let assistantRenderFrame = 0;
  let thinkingRenderFrame = 0;
  let assistantFollow = false;
  let thinkingFollow = false;
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

  function setSendButton(running) {
    send.textContent = running ? "■" : "➤";
    send.setAttribute("aria-label", running ? "Stop" : "Send");
    send.title = running ? "Stop" : "Send";
  }
  const slashCommands = [
    ["/ask", "answer a question"], ["/cd", "change directory"],
    ["/clear", "reset conversation"], ["/compact", "compact context"], ["/context-doctor", "audit context"],
    ["/effort", "set reasoning effort"], ["/execute", "execute a plan"], ["/goal-from-context", "formulate a goal"],
	    ["/help", "show commands"], ["/lsp", "show language server status"], ["/mcp", "manage MCP servers"],
	["/model", "switch or refresh models"], ["/plan", "enter plan mode"],
	["/approval", "switch approval mode (ask|auto-review|never)"],
	["/notify", "Telegram completion notifications (config|on|off)"],
	["/continue", "continue an interrupted turn"],
    ["/pwd", "print working directory"], ["/detach", "detach worker"], ["/quit", "exit"], ["/exit", "exit"], ["/q", "exit"], ["/rename", "rename session"],
    ["/resume", "resume a session"], ["/review", "review a target"], ["/commands", "show commands"],
  ].map(([name, hint]) => ({ name, hint }));

  function save() {
    state.draft = prompt.value;
    vscode.setState(state);
  }

  function nearBottom() {
    return transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 48;
  }

  function follow() {
    transcript.scrollTop = transcript.scrollHeight;
  }

  function setSettings(open) {
    settings.hidden = !open;
    chatView.hidden = open;
    settingsToggle.setAttribute("aria-expanded", String(open));
    state.settingsOpen = open;
    save();
    if (open) hideCompletions();
  }

  function text(value) {
    return typeof value === "string" ? value : "";
  }

  function hideToolDisplayHash(value) {
    return text(value).replace(/sha256:[0-9a-f]{8,}/gi, (match) =>
      match.length > 15 ? `${match.slice(0, 15)}…` : match);
  }

  function inlineMarkdown(element, source) {
    const pattern = /(`[^`\n]+`|\*\*[^*\n]+\*\*|__[^_\n]+__|\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)|\*[^*\n]+\*|_[^_\n]+_)/g;
    let offset = 0;
    for (const match of source.matchAll(pattern)) {
      const token = match[0];
      const start = match.index ?? 0;
      if (start > offset) element.append(document.createTextNode(source.slice(offset, start)));
      if (token.startsWith("`")) {
        const code = document.createElement("code");
        code.textContent = token.slice(1, -1);
        element.append(code);
      } else if (token.startsWith("[")) {
        const link = document.createElement("a");
        link.textContent = match[2];
        link.href = match[3];
        link.target = "_blank";
        link.rel = "noreferrer";
        element.append(link);
      } else if (token.startsWith("**") || token.startsWith("__")) {
        const strong = document.createElement("strong");
        strong.textContent = token.slice(2, -2);
        element.append(strong);
      } else if (token.startsWith("*") || token.startsWith("_")) {
        const emphasis = document.createElement("em");
        emphasis.textContent = token.slice(1, -1);
        element.append(emphasis);
      }
      offset = start + token.length;
    }
    if (offset < source.length) element.append(document.createTextNode(source.slice(offset)));
  }

  function renderMarkdown(element, source) {
    element.replaceChildren();
    const lines = source.replace(/\r\n?/g, "\n").split("\n");
    let paragraph = [];
    let list;
    let listKind;
    let code;
    let fence;

    const flushParagraph = () => {
      if (paragraph.length === 0) return;
      const p = document.createElement("p");
      inlineMarkdown(p, paragraph.join(" "));
      element.append(p);
      paragraph = [];
    };
    const flushList = () => {
      list = undefined;
      listKind = undefined;
    };
    const flushText = () => {
      flushParagraph();
      flushList();
    };

    for (const line of lines) {
      const opening = line.match(/^ {0,3}(`{3,}|~{3,})\s*([\w+-]*)\s*$/);
      if (fence) {
        if (opening && opening[1][0] === fence) {
          fence = undefined;
        } else {
          code.textContent += `${code.textContent ? "\n" : ""}${line}`;
        }
        continue;
      }
      if (opening) {
        flushText();
        const pre = document.createElement("pre");
        code = document.createElement("code");
        const language = opening[2].replace(/[^\w+-]/g, "");
        if (language) code.className = `language-${language}`;
        pre.append(code);
        element.append(pre);
        fence = opening[1][0];
        continue;
      }
      if (!line.trim()) {
        flushText();
        continue;
      }
      const heading = line.match(/^ {0,3}(#{1,6})\s+(.+?)\s*#*$/);
      if (heading) {
        flushText();
        const h = document.createElement(`h${heading[1].length}`);
        inlineMarkdown(h, heading[2]);
        element.append(h);
        continue;
      }
      if (/^ {0,3}([-*_])(?:\s*\1){2,}\s*$/.test(line)) {
        flushText();
        element.append(document.createElement("hr"));
        continue;
      }
      const bullet = line.match(/^ {0,3}[-+*]\s+(.+)$/);
      const ordered = line.match(/^ {0,3}\d+[.)]\s+(.+)$/);
      if (bullet || ordered) {
        flushParagraph();
        const kind = ordered ? "ol" : "ul";
        if (!list || listKind !== kind) {
          flushList();
          listKind = kind;
          list = document.createElement(kind);
          element.append(list);
        }
        const item = document.createElement("li");
        inlineMarkdown(item, (bullet || ordered)[1]);
        list.append(item);
        continue;
      }
      const quote = line.match(/^ {0,3}>\s?(.*)$/);
      if (quote) {
        flushText();
        const blockquote = document.createElement("blockquote");
        inlineMarkdown(blockquote, quote[1]);
        element.append(blockquote);
        continue;
      }
      flushList();
      paragraph.push(line.trim());
    }
    flushText();
  }

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

  function renderToolContent(element, block) {
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
  }

  function isInternalReviewTool(name) {
    return name === "submit_review";
  }

  function reviewExtensionMarkdown(args) {
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

  function reviewFallbackMarkdown(value) {
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

  function exportKind(value) {
    return value === "plan" || value === "review" ? value : "";
  }

  function exportButton(kind) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "copy export";
    button.innerHTML = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 2v7M5 6.5l3 3 3-3M3 11.5v2h10v-2"/></svg>';
    button.setAttribute("aria-label", `Export ${kind}`);
    button.title = `Export ${kind}`;
    button.addEventListener("click", () => vscode.postMessage({ type: "exportResult", kind }));
    return button;
  }

  function addExportButton(element, block) {
    const kind = exportKind(block.resultKind);
    if (!kind) return;
    const header = element?.querySelector(".block-header");
    if (header && !header.querySelector(".export")) header.append(exportButton(kind));
  }

  function callEffort(raw) {
    let effort = text(raw.effort_applied).trim() || text(raw.reasoning_effort).trim();
    if (!effort && raw.reasoning_enabled === true) effort = "on";
    return effort === "off" || effort === "default" ? "" : effort;
  }

  function thinkingLabel(block) {
    let label = block.finished ? "Reasoning" : "Reasoning…";
    if (Number(block.durationMs) > 0) label += ` · ${formatDuration(block.durationMs)}`;
    if (text(block.effort).trim()) label += ` · ${block.effort}`;
    return label;
  }

  function updateThinkingSummary(element, block) {
    const summary = element?.querySelector("summary");
    if (summary) summary.textContent = thinkingLabel(block);
  }

  function renderBlock(block) {
    const element = document.createElement("article");
    element.className = `block ${block.kind || "notice"}`;
    if (block.kind === "user") {
      const body = document.createElement("pre");
      body.textContent = text(block.text);
      element.append(body);
    } else if (block.kind === "assistant") {
      const header = document.createElement("div");
      header.className = "block-header";
      const copy = document.createElement("button");
      copy.type = "button";
      copy.className = "copy";
      copy.innerHTML = '<svg viewBox="0 0 16 16" aria-hidden="true"><rect x="5" y="5" width="8" height="8" rx="1"/><path d="M3 10V3h7"/></svg>';
      copy.setAttribute("aria-label", "Copy response");
      copy.title = "Copy response";
      copy.addEventListener("click", () => {
        void navigator.clipboard?.writeText(text(block.text));
        copy.textContent = "✓";
        copy.title = "Copied";
        setTimeout(() => { copy.innerHTML = '<svg viewBox="0 0 16 16" aria-hidden="true"><rect x="5" y="5" width="8" height="8" rx="1"/><path d="M3 10V3h7"/></svg>'; copy.title = "Copy response"; }, 1200);
      });
      header.append(copy);
      const body = document.createElement("div");
      body.className = "markdown";
      renderMarkdown(body, text(block.text));
      element.append(header, body);
      addExportButton(element, block);
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
      element.thinkingSummary = summary;
      element.body = body;
    } else if (block.kind === "tool") {
      renderToolContent(element, block);
    } else {
      element.textContent = text(block.text);
    }
    transcript.append(element);
    return element;
  }

  function rerender() {
    const followNow = nearBottom();
    const scrollTop = transcript.scrollTop;
    transcript.replaceChildren();
    for (const block of state.blocks) {
      renderBlock(block);
    }
    renderReferences();
    updateMode();
    if (followNow) follow();
    else transcript.scrollTop = scrollTop;
    return followNow;
  }

  function append(block, force = false) {
    // New user input should be visible immediately; background events must
    // respect a user who is reading older messages.
    const followNow = (force && block.kind === "user") || nearBottom();
    state.blocks.push(block);
    renderBlock(block);
    save();
    if (followNow) {
      follow();
    }
  }

  function assistantBlock() {
    if (!currentAssistant) {
      assistantFollow = nearBottom();
      currentAssistant = { kind: "assistant", text: "" };
      state.blocks.push(currentAssistant);
      currentAssistantElement = renderBlock(currentAssistant);
    }
    return currentAssistant;
  }

  function appendText(delta) {
    const block = assistantBlock();
    block.text += delta;
    if (assistantRenderFrame) return;
    assistantRenderFrame = requestAnimationFrame(() => {
      assistantRenderFrame = 0;
      renderMarkdown(currentAssistantElement.body, block.text);
      if (assistantFollow) follow();
    });
  }

  function appendThinking(delta) {
    if (!currentThinking) {
      thinkingFollow = nearBottom();
      currentThinking = { kind: "thinking", text: "", effort: activeReasoningEffort, startedAt: Date.now() };
      state.blocks.push(currentThinking);
      currentThinkingElement = renderBlock(currentThinking);
    }
    currentThinking.text += delta;
    if (thinkingRenderFrame) return;
    thinkingRenderFrame = requestAnimationFrame(() => {
      thinkingRenderFrame = 0;
      renderMarkdown(currentThinkingElement.body, currentThinking.text);
      if (thinkingFollow) follow();
    });
  }

  function closeThinking(durationMs = 0, effort = "") {
    if (!currentThinking) return;
    if (thinkingRenderFrame) {
      cancelAnimationFrame(thinkingRenderFrame);
      thinkingRenderFrame = 0;
    }
    const startedAt = Number(currentThinking.startedAt) || Date.now();
    const measured = Number(durationMs) > 0 ? Number(durationMs) : Date.now() - startedAt;
    currentThinking.durationMs = Math.max(0, measured);
    currentThinking.effort = text(effort).trim() || currentThinking.effort || activeReasoningEffort;
    currentThinking.finished = true;
    if (currentThinkingElement) {
      renderMarkdown(currentThinkingElement.body, currentThinking.text);
      updateThinkingSummary(currentThinkingElement, currentThinking);
    }
    if (thinkingFollow) follow();
    delete currentThinking.startedAt;
    currentThinking = undefined;
    currentThinkingElement = undefined;
    activeReasoningEffort = "";
    thinkingFollow = false;
  }

  function flushAssistantRender() {
    if (assistantRenderFrame) {
      cancelAnimationFrame(assistantRenderFrame);
      assistantRenderFrame = 0;
    }
    if (currentAssistant && currentAssistantElement) {
      renderMarkdown(currentAssistantElement.body, currentAssistant.text);
      if (assistantFollow) follow();
    }
    assistantFollow = false;
  }

  function flushThinking() {
    closeThinking();
  }

  function flushStreaming() {
    flushAssistantRender();
    flushThinking();
  }

  function formatCount(value) {
    if (!Number.isFinite(value) || value <= 0) return "0";
    if (value >= 1000000) return `${(value / 1000000).toFixed(1)}M`;
    if (value >= 1000) return `${(value / 1000).toFixed(1)}k`;
    return String(Math.round(value));
  }

  function formatDuration(milliseconds) {
    const seconds = Math.max(0, Number(milliseconds) || 0) / 1000;
    if (seconds < 1) return `${Math.round(seconds * 1000)}ms`;
    if (seconds < 60) return `${seconds.toFixed(1)}s`;
    const minutes = Math.floor(seconds / 60);
    return `${minutes}m ${Math.floor(seconds % 60)}s`;
  }

  function updateStatus() {
    const parts = [];
    if (model) parts.push(model);
    if (contextLimit > 0) parts.push(`${formatCount(context)}/${formatCount(contextLimit)} ctx`);
    const elapsed = active && turnSince ? formatDuration(Date.now() - turnSince) : "";
    parts.push(active && activity === "Thinking" ? `Thinking ${elapsed}` : active && elapsed ? `${activity} · ${elapsed}` : activity);
    status.textContent = parts.join(" · ");
  }

  function updateMode() {
    const label = modeLabels[state.mode] || modeLabels.execute;
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
    settingsApproval.value = state.approval;
    renderSettingsModels();
  }

  function renderSettingsModels() {
    settingsModels.replaceChildren();
    for (const name of ["default", "smart", "fast", "tiny"]) {
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
      button.addEventListener("click", () => vscode.postMessage({ type: "configureModel", role: name, mode: state.mode }));
      row.append(label, button);
      settingsModels.append(row);
    }
  }

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
    flushStreaming();
    const nextModel = snapshot.model_name || snapshot.model;
    if (typeof nextModel === "string") model = nextModel;
    if (typeof snapshot.context_tokens === "number") context = snapshot.context_tokens;
    if (typeof snapshot.context_limit === "number") contextLimit = snapshot.context_limit;
    if (typeof snapshot.effort === "string" && effortLevels.includes(snapshot.effort)) state.effort = snapshot.effort;
    if (typeof snapshot.approval === "string" && ["", "ask", "auto-review", "never"].includes(snapshot.approval)) state.approval = snapshot.approval;
    if (["default", "smart", "tiny", "fast"].includes(snapshot.role) && typeof nextModel === "string") state.models[snapshot.role] = nextModel;
    if (snapshot.mode === "plan") state.mode = "plan";
    if (snapshot.mode === "execute" && state.mode !== "review") state.mode = "execute";
    active = busyStates.has(snapshot.state);
    if (active && !turnSince) turnSince = Date.now();
    if (!active) turnSince = 0;
    activity = snapshot.state === "waiting_approval" ? "Waiting for approval" : snapshot.state === "waiting_question" ? "Waiting for answer" : active ? "Thinking" : "Ready";
    let liveBlock;
    let liveThinking;
    if (!pendingUser) {
      state.blocks = snapshotMessages(Array.isArray(snapshot.messages) ? snapshot.messages : []);
      if (snapshot.live_text) {
        liveBlock = { kind: "assistant", text: text(snapshot.live_text) };
        state.blocks.push(liveBlock);
      }
      if (snapshot.live_plan) {
        liveBlock = { kind: "assistant", text: text(snapshot.live_plan) };
        state.blocks.push(liveBlock);
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
    currentAssistant = undefined;
    currentAssistantElement = undefined;
    const followNow = rerender();
    setSendButton(active);
    send.classList.toggle("stop", active);
    if (active && liveBlock) {
      currentAssistant = liveBlock;
      currentAssistantElement = transcript.children[state.blocks.indexOf(liveBlock)];
      assistantFollow = followNow;
    }
    if (active && liveThinking) {
      currentThinking = liveThinking;
      currentThinkingElement = transcript.children[state.blocks.indexOf(liveThinking)];
      thinkingFollow = followNow;
    }
    save();
    updateStatus();
  }

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
    completionToken = undefined;
    completionItems = [];
    completionKind = "reference";
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

  function renderCompletions(items) {
    completionKind = "reference";
    completionItems = items.filter((item) => item && typeof item.path === "string");
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
      option.setAttribute("role", "option");
      option.textContent = `${item.path}${item.folder ? "  folder" : ""}`;
      option.addEventListener("mousedown", (event) => event.preventDefault());
      option.addEventListener("click", () => chooseCompletion(item));
      completionMenu.append(option);
      if (index === 0) option.classList.add("selected");
    });
    completionMenu.hidden = false;
  }

  function renderCommandCompletions(items) {
    completionKind = "command";
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
      option.setAttribute("role", "option");
      option.textContent = `${item.name} — ${item.hint}`;
      option.addEventListener("mousedown", (event) => event.preventDefault());
      option.addEventListener("click", () => chooseCommand(item));
      completionMenu.append(option);
      if (index === 0) option.classList.add("selected");
    });
    completionMenu.hidden = false;
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
      const query = command.query.toLowerCase();
      renderCommandCompletions(slashCommands.filter((item) => item.name.startsWith(query)));
      return;
    }
    const token = referenceToken();
    if (!token) {
      hideCompletions();
      return;
    }
    completionToken = token;
    const requestId = ++completionRequest;
    vscode.postMessage({ type: "completeReferences", requestId, query: token.query });
  }

  function handleEvent(raw) {
    if (!raw || typeof raw !== "object" || typeof raw.type !== "string") return;
    switch (raw.type) {
      case "turn_start":
        active = true;
        planDeltaSeen = false;
        currentAssistant = undefined;
        currentAssistantElement = undefined;
        currentThinking = undefined;
        currentThinkingElement = undefined;
        activity = "Thinking";
        turnSince = Date.now();
        activeReasoningEffort = "";
        setSendButton(true);
        send.classList.add("stop");
        updateStatus();
        break;
      case "turn_end":
        flushStreaming();
        if (typeof turnConfiguredEffort === "string") {
          state.effort = turnConfiguredEffort;
          turnConfiguredEffort = undefined;
          updateMode();
        }
        pendingUser = false;
        planDeltaSeen = false;
        tools.clear();
        active = false;
        currentAssistant = undefined;
        currentAssistantElement = undefined;
        currentThinking = undefined;
        currentThinkingElement = undefined;
        turnSince = 0;
        activeReasoningEffort = "";
        activity = "Ready";
        setSendButton(false);
        send.classList.remove("stop");
        updateStatus();
        save();
        break;
      case "snapshot":
        applySnapshot(raw.snapshot);
        break;
      case "route": {
        const nextModel = raw.model_name || raw.model;
        if (typeof nextModel === "string") model = nextModel;
        if (["default", "smart", "tiny", "fast"].includes(raw.role) && typeof nextModel === "string") {
          state.models[raw.role] = nextModel;
        }
        if (raw.mode === "plan") state.mode = "plan";
        if (raw.mode === "execute" && state.mode !== "review") state.mode = "execute";
        if (typeof raw.effort === "string" && effortLevels.includes(raw.effort)) state.effort = raw.effort;
        if (typeof raw.approval === "string" && ["", "ask", "auto-review", "never"].includes(raw.approval)) state.approval = raw.approval;
        updateMode();
        updateStatus();
        save();
        break;
      }
      case "role_model":
        if (["default", "smart", "tiny", "fast"].includes(raw.role) && typeof raw.model === "string") {
          state.role = raw.role;
          state.models[raw.role] = raw.model;
          updateMode();
          save();
        }
        break;
      case "text":
        if (typeof raw.delta === "string") {
          appendText(raw.delta);
          activity = "Responding";
          updateStatus();
        }
        break;
      case "plan_delta":
        if (typeof raw.delta === "string") {
          planDeltaSeen = true;
          appendText(raw.delta);
          activity = "Responding";
          updateStatus();
        }
        break;
      case "think":
        if (typeof raw.delta === "string") {
          appendThinking(raw.delta);
          activity = "Thinking";
          updateStatus();
        }
        break;
      case "tool_start": {
        closeThinking();
        currentAssistant = undefined;
        const name = text(raw.name);
        if (isInternalReviewTool(name)) {
          activity = "Thinking";
          updateStatus();
          break;
        }
        if (name === "request_review_extension") {
          const markdown = reviewExtensionMarkdown(text(raw.args));
          if (markdown) append({ kind: "assistant", text: markdown }, true);
          else append({ kind: "tool", name, args: text(raw.args), failed: false });
          activity = "Thinking";
          updateStatus();
          break;
        }
        const block = { kind: "tool", name, args: text(raw.args), failed: false };
        append(block);
        const element = transcript.lastElementChild;
        const id = typeof raw.id === "string" && raw.id ? raw.id : `anonymous-${++anonymousTool}`;
        tools.set(id, { block, element });
        activity = `Using ${block.name || "tool"}`;
        updateStatus();
        break;
      }
      case "tool_end": {
        const id = typeof raw.id === "string" ? raw.id : "";
        const entry = tools.get(id);
        if (entry) {
          entry.block.failed = typeof raw.result === "string" && /^(error|failed)/i.test(raw.result.trim());
          renderToolContent(entry.element, entry.block);
          tools.delete(id);
          save();
        }
        activity = "Thinking";
        updateStatus();
        break;
      }
      case "state":
        active = busyStates.has(raw.state);
        if (active && !turnSince) turnSince = Date.now();
        if (!active) turnSince = 0;
        activity = raw.state === "waiting_approval" ? "Waiting for approval" : raw.state === "waiting_question" ? "Waiting for answer" : active ? "Thinking" : "Ready";
        updateStatus();
        break;
      case "retry":
        append({ kind: "notice", text: `⚠ retrying request (attempt ${Number(raw.attempt || 0) + 1}/${raw.max || "?"})` }, true);
        break;
      case "steer":
        if (typeof raw.text === "string") append({ kind: "user", text: `${raw.text} (steered)` }, true);
        break;
      case "usage":
        if (typeof raw.prompt_tokens === "number" || typeof raw.completion_tokens === "number") {
          context = (Number(raw.prompt_tokens) || 0) + (Number(raw.completion_tokens) || 0);
          updateStatus();
        }
        break;
      case "task":
        if (typeof raw.description === "string" && typeof raw.status === "string") {
          append({ kind: "notice", text: `Task ${raw.status}: ${raw.description}` }, true);
        }
        break;
      case "goal": {
        const status = text(raw.status || raw.Status);
        const objective = text(raw.objective || raw.Objective);
        if (status || objective) append({ kind: "notice", text: `Goal ${status || "updated"}${objective ? `: ${objective}` : ""}` }, true);
        break;
      }
      case "goal_update": {
        const status = text(raw.status || raw.Status);
        const progress = text(raw.progress || raw.Progress);
        const blocker = text(raw.blocker || raw.Blocker);
        const detail = blocker ? `blocked: ${blocker}` : progress;
        if (status || detail) append({ kind: "notice", text: `Goal ${status || "updated"}${detail ? `: ${detail}` : ""}` }, true);
        break;
      }
      case "mcp":
        append({ kind: "notice", text: "MCP status updated." }, true);
        break;
      case "prompt_view":
        if (typeof raw.model === "string") model = raw.model;
        if (typeof raw.estimated_tokens === "number") context = raw.estimated_tokens;
        if (typeof raw.context_limit === "number") contextLimit = raw.context_limit;
        updateStatus();
        break;
      case "model_call_start":
        closeThinking();
        if (typeof raw.model === "string") model = raw.model;
        activeReasoningEffort = callEffort(raw);
        if (!raw.purpose && (typeof raw.configured_effort === "string" || typeof raw.effort_applied === "string" || typeof raw.selection_reason === "string")) {
          if (turnConfiguredEffort === undefined && typeof raw.configured_effort === "string") {
            turnConfiguredEffort = raw.configured_effort;
          }
          const applied = activeReasoningEffort;
          if (effortLevels.includes(applied)) {
            state.effort = applied;
            updateMode();
          }
        }
        updateStatus();
        break;
      case "model_call_end":
        closeThinking(raw.latency_ms, callEffort(raw));
        updateStatus();
        break;
      case "models":
        refreshModels.disabled = false;
        if (raw.models && typeof raw.models === "object" && !Array.isArray(raw.models)) {
          for (const name of ["default", "smart", "tiny", "fast"]) {
            if (typeof raw.models[name] === "string" && raw.models[name]) state.models[name] = raw.models[name];
          }
          updateMode();
          save();
        }
        break;
      case "extensionSettings": {
        const value = raw.settings;
        if (!value || typeof value !== "object") break;
        if (validExecution(value.sandbox, ["", "read-only", "workspace-write", "danger-full-access"])) state.sandbox = value.sandbox;
        if (validExecution(value.network, ["", "deny", "host"])) state.network = value.network;
        if (validExecution(value.approval, ["", "ask", "auto-review", "never"])) state.approval = value.approval;
        if (typeof value.binary === "string" && value.binary) settingsRuntime.textContent = `Binary: ${value.binary}`;
        updateMode();
        break;
      }
      case "turn_done":
        {
          const visible = state.blocks.filter((block) => block.kind !== "tool" || !isInternalReviewTool(block.name));
          if (visible.length !== state.blocks.length) {
            state.blocks = visible;
            rerender();
          }
        }
        if (typeof raw.plan === "string" && raw.plan) {
          state.proposedPlan = raw.plan;
          if (!planDeltaSeen) {
            append({ kind: "assistant", text: raw.plan, resultKind: "plan" }, true);
          } else if (currentAssistant) {
            currentAssistant.resultKind = "plan";
            if (currentAssistantElement) addExportButton(currentAssistantElement, currentAssistant);
            else rerender();
          }
        }
        const reviewMarkdown = text(raw.review_markdown) || reviewFallbackMarkdown(raw.review) || reviewFallbackMarkdown(raw.final);
        if (reviewMarkdown) {
          append({ kind: "assistant", text: reviewMarkdown, resultKind: "review" }, true);
          save();
        }
        if (typeof raw.error === "string" && raw.error) append({ kind: "error", text: raw.error }, true);
        if (typeof raw.effort === "string" && effortLevels.includes(raw.effort)) {
          state.effort = raw.effort;
          updateMode();
        } else if (typeof turnConfiguredEffort === "string") {
          state.effort = turnConfiguredEffort;
          updateMode();
        }
        break;
      case "compact_done":
        if (typeof raw.error === "string" && raw.error) append({ kind: "error", text: raw.error }, true);
        else append({ kind: "notice", text: "Context compacted." }, true);
        break;
      case "compact":
        append({ kind: "notice", text: "Context compaction completed; raw history preserved." }, true);
        break;
      case "goal_from_context":
        if (raw.goal && typeof raw.goal.objective === "string") append({ kind: "notice", text: `Goal: ${raw.goal.objective}` }, true);
        break;
      case "ack":
        showCommandResult(text(raw.name), raw.payload);
        break;
      case "shell_done":
        if (raw.output) append({ kind: "notice", text: text(raw.output) }, true);
        break;
      case "notice":
        if (typeof raw.text === "string") append({ kind: "notice", text: raw.text });
        break;
      case "error":
        refreshModels.disabled = false;
        flushStreaming();
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
        break;
      case "referenceSuggestions":
        if (raw.requestId === completionRequest && Array.isArray(raw.items)) renderCompletions(raw.items);
        break;
      case "new_session":
        pendingUser = false;
        state.blocks = [];
        state.references = [];
        currentAssistant = undefined;
        currentAssistantElement = undefined;
        currentThinking = undefined;
        currentThinkingElement = undefined;
        turnSince = 0;
        activeReasoningEffort = "";
        rerender();
        save();
        break;
      case "resume_session":
        append({ kind: "notice", text: "Session resumed." }, true);
        break;
    }
  }

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
    vscode.postMessage(message);
  });
  prompt.addEventListener("input", () => { save(); requestCompletions(); });
  prompt.addEventListener("keydown", (event) => {
    if (!completionMenu.hidden) {
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        completionIndex = (completionIndex + (event.key === "ArrowDown" ? 1 : completionItems.length - 1)) % completionItems.length;
        completionMenu.querySelectorAll(".completion").forEach((item, index) => item.classList.toggle("selected", index === completionIndex));
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
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      composer.requestSubmit();
    }
  });
  modeToggle.addEventListener("click", () => {
    const modes = ["execute", "plan", "review"];
    state.mode = modes[(modes.indexOf(state.mode) + 1) % modes.length];
    updateMode();
    save();
    vscode.postMessage({ type: "configureRole", role: state.role, mode: state.mode });
  });
  settingsToggle.addEventListener("click", () => setSettings(settings.hidden));
  settingsClose.addEventListener("click", () => setSettings(false));
  settingsCloseBottom.addEventListener("click", () => setSettings(false));
  refreshModels.addEventListener("click", () => {
    refreshModels.disabled = true;
    vscode.postMessage({ type: "refreshModels" });
  });
  openSettings.addEventListener("click", () => vscode.postMessage({ type: "openSettings" }));
  openAuth.addEventListener("click", () => vscode.postMessage({ type: "openAuth" }));
  send.addEventListener("click", (event) => {
    if (active) {
      event.preventDefault();
      vscode.postMessage({ type: "stop" });
    }
  });
  role.addEventListener("change", () => {
    if (["default", "smart", "tiny", "fast"].includes(role.value)) {
      state.role = role.value;
      save();
      vscode.postMessage({ type: "configureRole", role: state.role, mode: state.mode });
    }
  });
  effort.addEventListener("change", () => {
    if (!effortLevels.includes(effort.value)) return;
    state.effort = effort.value;
    save();
    vscode.postMessage({
      type: "configureRole",
      role: state.role,
      mode: state.mode,
      effort: state.effort,
      updateEffort: true,
    });
  });
  settingsRole.addEventListener("change", () => {
    if (!validRole(settingsRole.value)) return;
    state.role = settingsRole.value;
    updateMode();
    save();
    vscode.postMessage({ type: "setDefaultSetting", name: "defaultRole", value: state.role });
    vscode.postMessage({ type: "configureRole", role: state.role, mode: state.mode });
  });
  settingsMode.addEventListener("change", () => {
    if (!validMode(settingsMode.value)) return;
    state.mode = settingsMode.value;
    updateMode();
    save();
    vscode.postMessage({ type: "setDefaultSetting", name: "defaultMode", value: state.mode });
    vscode.postMessage({ type: "configureRole", role: state.role, mode: state.mode });
  });
  settingsEffort.addEventListener("change", () => {
    if (!effortLevels.includes(settingsEffort.value)) return;
    state.effort = settingsEffort.value;
    updateMode();
    save();
    vscode.postMessage({ type: "setDefaultSetting", name: "defaultEffort", value: state.effort });
    vscode.postMessage({ type: "configureRole", role: state.role, mode: state.mode, effort: state.effort, updateEffort: true });
  });
  for (const input of [settingsSandbox, settingsNetwork, settingsApproval]) {
    input.addEventListener("change", () => {
      const name = input.id.replace("settings-", "");
      state[name] = input.value;
      if (name === "approval") updateMode();
      save();
      vscode.postMessage({ type: "setExecutionSetting", name, value: input.value });
    });
  }
  transcript.addEventListener("click", (event) => {
    const target = event.target;
    const anchor = target instanceof Element ? target.closest("a[href]") : null;
    if (!anchor) return;
    event.preventDefault();
    vscode.postMessage({ type: "openExternal", uri: anchor.href });
  });
  transcript.addEventListener("scroll", () => {
    if (!nearBottom()) {
      assistantFollow = false;
      thinkingFollow = false;
      return;
    }
    if (currentAssistant) assistantFollow = true;
    if (currentThinking) thinkingFollow = true;
  }, { passive: true });
  window.addEventListener("message", (event) => handleEvent(event.data));
  setInterval(() => { if (active) updateStatus(); }, 1000);

  prompt.value = state.draft;
  setSettings(state.settingsOpen);
  rerender();
  setSendButton(false);
  updateStatus();
  vscode.postMessage({ type: "ready" });
})();
