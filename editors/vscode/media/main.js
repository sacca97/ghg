(() => {
  const vscode = acquireVsCodeApi();
  const transcript = document.getElementById("transcript");
  const prompt = document.getElementById("prompt");
  const status = document.getElementById("status");
  const references = document.getElementById("references");
  const completionMenu = document.getElementById("completion-menu");
  const composer = document.getElementById("composer");
  const send = document.getElementById("send");
  const role = document.getElementById("role");
  const chatMode = document.getElementById("chat-mode");
  const planMode = document.getElementById("plan-mode");
  const saved = vscode.getState() || {};
  const state = {
    blocks: Array.isArray(saved.blocks) ? saved.blocks : [],
    draft: typeof saved.draft === "string" ? saved.draft : "",
    mode: saved.mode === "plan" ? "plan" : "chat",
    role: ["default", "smart", "tiny", "fast"].includes(saved.role) ? saved.role : "fast",
    models: saved.models && typeof saved.models === "object" ? saved.models : {},
    references: Array.isArray(saved.references) ? saved.references.filter((path) => typeof path === "string") : [],
    proposedPlan: typeof saved.proposedPlan === "string" ? saved.proposedPlan : "",
  };
  let active = false;
  let currentAssistant;
  let currentAssistantElement;
  let model = "";
  let context = 0;
  let contextLimit = 0;
  let activity = "Ready";
  let thinkingSince = 0;
  let completionRequest = 0;
  let completionToken;
  let completionItems = [];
  let completionIndex = 0;
  let completionKind = "reference";
  const tools = new Map();
  const slashCommands = [
    ["/ask", "answer a question"], ["/auth", "configure a provider"], ["/cd", "change directory"],
    ["/clear", "reset conversation"], ["/compact", "compact context"], ["/context-doctor", "audit context"],
    ["/detach", "leave the worker running"], ["/effort", "set reasoning effort"], ["/execute", "execute a plan"],
    ["/export", "export the session"], ["/fork", "fork the session"], ["/goal", "set or manage a goal"],
    ["/goal-from-context", "formulate a goal"], ["/help", "show commands"], ["/mcp", "manage MCP servers"],
    ["/memory", "manage memories"], ["/model", "switch model"], ["/plan", "enter plan mode"],
    ["/pwd", "print working directory"], ["/quit", "exit"], ["/rename", "rename session"],
    ["/report", "create a bug report"], ["/resume", "resume a session"], ["/review", "review a target"],
    ["/schedule", "schedule a turn"], ["/tasks", "show background tasks"], ["/commands", "show commands"],
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

  function text(value) {
    return typeof value === "string" ? value : "";
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

  function renderToolContent(element, block) {
    element.replaceChildren();
    const label = document.createElement("span");
    label.textContent = `⚒ ${block.name || "tool"}`;
    element.append(label);
    if (block.args) {
      const args = document.createElement("code");
      args.textContent = ` ${block.args}`;
      element.append(args);
    }
    if (block.failed) {
      const failed = document.createElement("span");
      failed.className = "failed";
      failed.textContent = " — failed";
      element.append(failed);
    }
  }

  function renderBlock(block) {
    const element = document.createElement("article");
    element.className = `block ${block.kind || "notice"}`;
    if (block.kind === "user") {
    const label = document.createElement("div");
      label.className = "label";
      label.textContent = "You";
      const body = document.createElement("pre");
      body.textContent = text(block.text);
      element.append(label, body);
    } else if (block.kind === "assistant") {
      const header = document.createElement("div");
      header.className = "block-header";
      const label = document.createElement("span");
      label.className = "label";
      label.textContent = "ghg";
      const copy = document.createElement("button");
      copy.type = "button";
      copy.className = "copy";
      copy.textContent = "Copy";
      copy.addEventListener("click", () => {
        void navigator.clipboard?.writeText(text(block.text));
        copy.textContent = "Copied";
        setTimeout(() => { copy.textContent = "Copy"; }, 1200);
      });
      header.append(label, copy);
      const body = document.createElement("div");
      body.className = "markdown";
      renderMarkdown(body, text(block.text));
      element.append(header, body);
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
    transcript.replaceChildren();
    for (const block of state.blocks) {
      renderBlock(block);
    }
    renderReferences();
    updateMode();
  }

  function append(block, force = false) {
    const followNow = force || nearBottom();
    state.blocks.push(block);
    renderBlock(block);
    save();
    if (followNow) {
      follow();
    }
  }

  function assistantBlock() {
    if (!currentAssistant) {
      currentAssistant = { kind: "assistant", text: "" };
      state.blocks.push(currentAssistant);
      currentAssistantElement = renderBlock(currentAssistant);
    }
    return currentAssistant;
  }

  function appendText(delta) {
    const followNow = nearBottom();
    const block = assistantBlock();
    block.text += delta;
    renderMarkdown(currentAssistantElement.body, block.text);
    save();
    if (followNow) {
      follow();
    }
  }

  function formatCount(value) {
    if (!Number.isFinite(value) || value <= 0) return "0";
    if (value >= 1000000) return `${(value / 1000000).toFixed(1)}M`;
    if (value >= 1000) return `${(value / 1000).toFixed(1)}k`;
    return String(Math.round(value));
  }

  function updateStatus() {
    const parts = [];
    if (model) parts.push(model);
    if (contextLimit > 0) parts.push(`${formatCount(context)}/${formatCount(contextLimit)} ctx`);
    parts.push(active && activity === "Thinking" ? `Thinking ${Math.max(0, Math.floor((Date.now() - thinkingSince) / 1000))}s` : activity);
    status.textContent = parts.join(" · ");
  }

  function updateMode() {
    chatMode.classList.toggle("selected", state.mode === "chat");
    planMode.classList.toggle("selected", state.mode === "plan");
    chatMode.setAttribute("aria-pressed", String(state.mode === "chat"));
    planMode.setAttribute("aria-pressed", String(state.mode === "plan"));
    role.disabled = false;
    role.value = state.role;
    for (const option of role.options) {
      option.textContent = state.models[option.value] || "Configured";
    }
  }

  function snapshotMessages(messages) {
    if (!Array.isArray(messages)) return [];
    return messages.flatMap((message) => {
      if (!message || typeof message !== "object") return [];
      if (message.role === "user" && typeof message.content === "string") return [{ kind: "user", text: message.content }];
      if (message.role === "assistant" && typeof message.content === "string" && message.content) return [{ kind: "assistant", text: message.content }];
      if (message.role === "tool" && typeof message.content === "string" && message.content) return [{ kind: "notice", text: message.content }];
      return [];
    });
  }

  function applySnapshot(snapshot) {
    if (!snapshot || typeof snapshot !== "object") return;
    const nextModel = snapshot.model_name || snapshot.model;
    if (typeof nextModel === "string") model = nextModel;
    if (typeof snapshot.context_tokens === "number") context = snapshot.context_tokens;
    if (typeof snapshot.context_limit === "number") contextLimit = snapshot.context_limit;
    if (["default", "smart", "tiny", "fast"].includes(snapshot.role) && typeof nextModel === "string") state.models[snapshot.role] = nextModel;
    if (snapshot.mode === "plan") state.mode = "plan";
    if (snapshot.mode === "execute") state.mode = "chat";
    if (Array.isArray(snapshot.messages)) state.blocks = snapshotMessages(snapshot.messages);
    currentAssistant = undefined;
    currentAssistantElement = undefined;
    rerender();
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
      item.textContent = path;
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
    prompt.value = `${before}${after}`;
    if (!state.references.includes(item.path)) state.references.push(item.path);
    hideCompletions();
    renderReferences();
    save();
    prompt.focus();
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
        activity = "Thinking";
        thinkingSince = Date.now();
        send.textContent = "Stop";
        send.classList.add("stop");
        updateStatus();
        break;
      case "turn_end":
        active = false;
        currentAssistant = undefined;
        currentAssistantElement = undefined;
        activity = "Ready";
        send.textContent = "Send";
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
          state.role = raw.role;
        }
        if (raw.mode === "plan") state.mode = "plan";
        if (raw.mode === "execute") state.mode = "chat";
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
      case "plan_delta":
        if (typeof raw.delta === "string") {
          appendText(raw.delta);
          activity = "Responding";
          updateStatus();
        }
        break;
      case "tool_start": {
        currentAssistant = undefined;
        const block = { kind: "tool", name: text(raw.name), args: text(raw.args), failed: false };
        append(block);
        const element = transcript.lastElementChild;
        tools.set(typeof raw.id === "string" ? raw.id : String(tools.size), { block, element });
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
        }
        activity = "Thinking";
        thinkingSince = Date.now();
        updateStatus();
        break;
      }
      case "prompt_view":
        if (typeof raw.model === "string") model = raw.model;
        if (["default", "smart", "tiny", "fast"].includes(raw.role) && typeof raw.model === "string") {
          state.models[raw.role] = raw.model;
          updateMode();
          save();
        }
        if (typeof raw.estimated_tokens === "number") context = raw.estimated_tokens;
        if (typeof raw.context_limit === "number") contextLimit = raw.context_limit;
        updateStatus();
        break;
      case "model_call_start":
        if (typeof raw.model === "string") model = raw.model;
        if (["default", "smart", "tiny", "fast"].includes(raw.role) && typeof raw.model === "string") {
          state.models[raw.role] = raw.model;
          updateMode();
          save();
        }
        updateStatus();
        break;
      case "models":
        if (raw.models && typeof raw.models === "object" && !Array.isArray(raw.models)) {
          for (const name of ["default", "smart", "tiny", "fast"]) {
            if (typeof raw.models[name] === "string" && raw.models[name]) state.models[name] = raw.models[name];
          }
          updateMode();
          save();
        }
        break;
      case "turn_done":
        if (typeof raw.plan === "string" && raw.plan) {
          state.proposedPlan = raw.plan;
          save();
        }
        if (typeof raw.error === "string" && raw.error) append({ kind: "error", text: raw.error }, true);
        break;
      case "compact_done":
        if (typeof raw.error === "string" && raw.error) append({ kind: "error", text: raw.error }, true);
        else append({ kind: "notice", text: "Context compacted." }, true);
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
        append({ kind: "error", text: text(raw.error) || "ghg failed" }, true);
        activity = "Error";
        if (active) {
          active = false;
          send.textContent = "Send";
          send.classList.remove("stop");
        }
        updateStatus();
        break;
      case "referenceSuggestions":
        if (raw.requestId === completionRequest && Array.isArray(raw.items)) renderCompletions(raw.items);
        break;
      case "new_session":
        state.blocks = [];
        state.references = [];
        currentAssistant = undefined;
        currentAssistantElement = undefined;
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
    if (active || !prompt.value.trim()) {
      if (active) vscode.postMessage({ type: "stop" });
      return;
    }
    append({ kind: "user", text: prompt.value.trim() }, true);
    const message = { type: "send", prompt: prompt.value, mode: state.mode, role: state.role, references: state.references, plan: state.proposedPlan };
    prompt.value = "";
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
  chatMode.addEventListener("click", () => {
    state.mode = "chat";
    updateMode();
    save();
    vscode.postMessage({ type: "configureRole", role: state.role, mode: "chat" });
  });
  planMode.addEventListener("click", () => {
    state.mode = "plan";
    updateMode();
    save();
    vscode.postMessage({ type: "configureRole", role: state.role, mode: "plan" });
  });
  role.addEventListener("change", () => {
    if (["default", "smart", "tiny", "fast"].includes(role.value)) {
      state.role = role.value;
      save();
      vscode.postMessage({ type: "configureRole", role: state.role, mode: state.mode === "plan" ? "plan" : "chat" });
    }
  });
  window.addEventListener("message", (event) => handleEvent(event.data));
  setInterval(() => { if (active && activity === "Thinking") updateStatus(); }, 1000);

  prompt.value = state.draft;
  rerender();
  updateStatus();
})();
