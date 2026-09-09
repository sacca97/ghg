import { randomBytes } from "node:crypto";
import * as readline from "node:readline";
import { ChildProcess, spawn } from "node:child_process";
import * as vscode from "vscode";

type Event = { type?: unknown; [key: string]: unknown };
type Workspace = vscode.WorkspaceFolder | undefined;
type Role = "default" | "smart" | "tiny" | "fast";

type SessionPick = vscode.QuickPickItem & { sessionId: string };
type ReferenceSuggestion = { path: string; folder: boolean };

const sessionKey = (workspace: Workspace): string =>
	`session:${workspace?.uri.toString() ?? "global"}`;

function currentWorkspace(): Workspace {
	const editor = vscode.window.activeTextEditor;
	return editor ? vscode.workspace.getWorkspaceFolder(editor.document.uri) : vscode.workspace.workspaceFolders?.[0];
}

function cleanReferences(value: unknown): string[] {
	if (!Array.isArray(value)) {
		return [];
	}
	const paths = new Set<string>();
	for (const item of value) {
		if (typeof item !== "string") {
			continue;
		}
		const path = item.trim();
		if (!path || path === ".." || path.startsWith("../") || path.startsWith("/") || /[\r\n\0]/.test(path)) {
			continue;
		}
		paths.add(path);
	}
	return [...paths].slice(0, 32);
}

function promptWithReferences(prompt: string, paths: string[]): string {
	if (paths.length === 0) {
		return prompt;
	}
	return `${prompt}\n\nReferenced workspace paths:\n${paths.map((path) => `- ${path}`).join("\n")}`;
}

function literalGlob(value: string): string {
	return value.replace(/[\\{}()[\]*?]/g, (character) => `\\${character}`);
}

async function referenceSuggestions(workspace: vscode.WorkspaceFolder, query: string): Promise<ReferenceSuggestion[]> {
	const normalized = query.replace(/\\/g, "/").trim();
	if (normalized.startsWith("/") || normalized === ".." || normalized.startsWith("../") || /[\r\n\0]/.test(normalized)) {
		return [];
	}
	const pattern = normalized ? `${literalGlob(normalized)}**` : "**/*";
	const files = await vscode.workspace.findFiles(
		new vscode.RelativePattern(workspace, pattern),
		"{**/.git/**,**/.ghg/**,**/node_modules/**}",
		200,
	);
	const prefix = normalized.toLowerCase();
	const candidates = new Map<string, boolean>();
	for (const uri of files) {
		const path = vscode.workspace.asRelativePath(uri, false).replace(/\\/g, "/");
		if (!path.toLowerCase().startsWith(prefix)) {
			continue;
		}
		const remainder = path.slice(normalized.length);
		const slash = remainder.indexOf("/");
		const candidate = slash < 0 ? path : path.slice(0, normalized.length + slash + 1);
		candidates.set(candidate, candidate.endsWith("/"));
	}
	return [...candidates.entries()]
		.sort(([a], [b]) => a.localeCompare(b))
		.slice(0, 50)
		.map(([path, folder]) => ({ path, folder }));
}

function parseSessions(output: string): SessionPick[] {
	return output
		.split(/\r?\n/)
		.map((line) => line.trim())
		.filter((line) => line && !line.startsWith("no sessions"))
		.map((line) => {
			const fields = line.split(/\s{2,}/);
			const sessionId = fields[0].split(/\s+/, 1)[0];
			return {
				label: sessionId,
				description: fields.slice(1).join(" · ") || "session",
				sessionId,
			};
		})
		.filter((pick) => pick.sessionId !== "");
}

function listSessions(binary: string, cwd: string | undefined): Promise<SessionPick[]> {
	return new Promise((resolve, reject) => {
		const child = spawn(binary, ["sessions"], { cwd, stdio: ["ignore", "pipe", "pipe"] });
		if (!child.stdout || !child.stderr) {
			reject(new Error("ghg did not expose piped output"));
			return;
		}
		let output = "";
		let error = "";
		child.stdout.on("data", (chunk: Buffer) => { output += chunk.toString(); });
		child.stderr.on("data", (chunk: Buffer) => { error += chunk.toString(); });
		child.once("error", reject);
		child.once("close", (code) => {
			if (code !== 0) {
				reject(new Error(error.trim() || `ghg sessions exited with code ${code ?? "unknown"}`));
				return;
			}
			resolve(parseSessions(output));
		});
	});
}

function runJSONCommand(binary: string, cwd: string | undefined, args: string[]): Promise<unknown> {
	return new Promise((resolve, reject) => {
		const child = spawn(binary, args, { cwd, stdio: ["ignore", "pipe", "pipe"] });
		if (!child.stdout || !child.stderr) {
			reject(new Error("ghg did not expose piped output"));
			return;
		}
		let output = "";
		let error = "";
		child.stdout.on("data", (chunk: Buffer) => { output += chunk.toString(); });
		child.stderr.on("data", (chunk: Buffer) => { error += chunk.toString(); });
		child.once("error", reject);
		child.once("close", (code) => {
			if (code !== 0) {
				reject(new Error(error.trim() || `ghg models exited with code ${code ?? "unknown"}`));
				return;
			}
			try { resolve(JSON.parse(output)); } catch (parseError) { reject(parseError); }
		});
	});
}

function listModels(binary: string, cwd: string | undefined): Promise<Record<string, string>> {
	return runJSONCommand(binary, cwd, ["models", "--format", "json"]).then((parsed) => {
		if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("ghg models returned invalid JSON");
		const models: Record<string, string> = {};
		for (const role of ["default", "smart", "tiny", "fast"]) {
			const model = (parsed as Record<string, unknown>)[role];
			if (typeof model === "string" && model !== "") models[role] = model;
		}
		return models;
	});
}

type CatalogModel = { model: string; provider: string };

function listCatalogModels(binary: string, cwd: string | undefined): Promise<CatalogModel[]> {
	return runJSONCommand(binary, cwd, ["models", "--all", "--format", "json"]).then((parsed) => {
		if (!Array.isArray(parsed)) throw new Error("ghg models returned invalid catalog JSON");
		return parsed.filter((item): item is CatalogModel => Boolean(item) && typeof item === "object" &&
			typeof (item as Record<string, unknown>).model === "string" && typeof (item as Record<string, unknown>).provider === "string");
	});
}

function interrupt(child: ChildProcess): void {
	if (child.exitCode !== null) {
		return;
	}
	child.kill("SIGINT");
	const timer = setTimeout(() => {
		if (child.exitCode === null) {
			child.kill("SIGKILL");
		}
	}, 2000);
	child.once("close", () => clearTimeout(timer));
}

function htmlFor(webview: vscode.Webview, extensionUri: vscode.Uri): string {
	const nonce = randomBytes(16).toString("hex");
	const script = webview.asWebviewUri(vscode.Uri.joinPath(extensionUri, "media", "main.js"));
	const style = webview.asWebviewUri(vscode.Uri.joinPath(extensionUri, "media", "main.css"));
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src ${webview.cspSource}; script-src 'nonce-${nonce}';">
<link rel="stylesheet" href="${style}">
</head>
<body>
<main id="app">
  <section id="transcript" aria-live="polite"></section>
  <div id="status" role="status">Ready</div>
  <div id="references" aria-label="Referenced files"></div>
  <div id="completion-menu" role="listbox" hidden></div>
  <form id="composer">
    <textarea id="prompt" rows="3" placeholder="Ask ghg anything…"></textarea>
    <div class="composer-row">
      <div class="mode" role="group" aria-label="Mode">
        <button type="button" id="chat-mode">Chat</button>
        <button type="button" id="plan-mode">Plan</button>
      </div>
			<select id="role" aria-label="Current model">
			  <option value="default">Configured</option>
			  <option value="smart">Configured</option>
			  <option value="tiny">Configured</option>
			  <option value="fast">Configured</option>
			</select>
      <button type="submit" id="send">Send</button>
    </div>
  </form>
</main>
<script nonce="${nonce}" src="${script}"></script>
</body>
</html>`;
}

type WebviewMessage = { type?: unknown; [key: string]: unknown };

class GHGViewProvider implements vscode.WebviewViewProvider {
	private view?: vscode.WebviewView;
	private bridge?: ChildProcess;
	private bridgeReady?: Promise<void>;
	private bridgeRequest = 0;
	private active = false;

	constructor(private readonly extension: vscode.ExtensionContext) {}

	resolveWebviewView(view: vscode.WebviewView): void {
		this.view = view;
		view.webview.options = {
			enableScripts: true,
			localResourceRoots: [vscode.Uri.joinPath(this.extension.extensionUri, "media")],
		};
		view.webview.html = htmlFor(view.webview, this.extension.extensionUri);
		view.webview.onDidReceiveMessage((message: unknown) => {
			void this.handleMessage(message);
		});
		void this.refreshModels();
		view.onDidDispose(() => {
			if (this.view === view) {
				this.view = undefined;
			}
		});
	}

	private async refreshModels(): Promise<void> {
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		try {
			this.post({ type: "models", models: await listModels(binary, workspace?.uri.fsPath) });
		} catch {
			// Model discovery is optional until the configured binary is available.
		}
	}

	private post(message: unknown): void {
		void this.view?.webview.postMessage(message);
	}

	private async pickModel(mode: "chat" | "plan"): Promise<void> {
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		const configured = await listModels(binary, workspace?.uri.fsPath);
		const rolePick = await vscode.window.showQuickPick(
			(["default", "smart", "fast", "tiny"] as Role[])
				.filter((role) => configured[role])
				.map((role) => ({ label: role, description: configured[role], role })),
			{ placeHolder: "Choose the model role to configure" },
		);
		if (!rolePick) return;
		const available = await listCatalogModels(binary, workspace?.uri.fsPath);
		const picks = new Map<string, CatalogModel>();
		for (const item of available) {
			if (!picks.has(`${item.provider}\x00${item.model}`)) picks.set(`${item.provider}\x00${item.model}`, item);
		}
		if (picks.size === 0) throw new Error("no configured catalog models found");
		const modelPick = await vscode.window.showQuickPick(
			[...picks.values()].map((item) => ({ label: `${item.provider}/${item.model}`, description: item.model, item })),
			{ placeHolder: `Choose the model for ${rolePick.role}`, matchOnDescription: true },
		);
		if (!modelPick) return;
		await this.bridgeCommand("set_role_model", {
			role: rolePick.role,
			model: modelPick.item.model,
			provider: modelPick.item.provider,
			mode: mode === "plan" ? "plan" : "execute",
		});
		this.post({ type: "role_model", role: rolePick.role, model: modelPick.item.model });
	}

	private async ensureBridge(): Promise<void> {
		if (this.bridge && this.bridgeReady) {
			return this.bridgeReady;
		}
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		const args = ["bridge", "--role", "fast"];
		const sessionId = this.extension.workspaceState.get<string>(sessionKey(workspace));
		if (sessionId) {
			args.push("--session", sessionId);
		}
		const child = spawn(binary, args, {
			cwd: workspace?.uri.fsPath,
			stdio: ["pipe", "pipe", "pipe"],
		});
		if (!child.stdin || !child.stdout || !child.stderr) {
			child.kill();
			throw new Error("ghg bridge did not expose piped streams");
		}
		this.bridge = child;
		let ready = false;
		let resolveReady!: () => void;
		let rejectReady!: (error: Error) => void;
		const promise = new Promise<void>((resolve, reject) => {
			resolveReady = resolve;
			rejectReady = reject;
		});
		this.bridgeReady = promise;
		const lines = readline.createInterface({ input: child.stdout });
		lines.on("line", (line) => {
			if (!line.trim()) return;
			let event: Event;
			try {
				event = JSON.parse(line) as Event;
			} catch (error) {
				this.post({ type: "error", error: `ghg bridge returned invalid JSON: ${String(error)}` });
				return;
			}
			if (event.type === "bridge_ready") {
				ready = true;
				if (typeof event.session_id === "string" && event.session_id !== "") {
					void this.extension.workspaceState.update(sessionKey(workspace), event.session_id);
				}
				resolveReady();
				return;
			}
			if (event.type === "turn_end") {
				this.active = false;
			}
			if (event.type === "error" && this.active) {
				this.active = false;
				this.post({ type: "turn_end" });
			}
			void this.handleBridgeEvent(event).catch((error: unknown) => {
				this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
			});
			this.post(event);
		});
		child.stderr.on("data", () => {});
		child.once("error", (error) => {
			if (!ready) rejectReady(error instanceof Error ? error : new Error(String(error)));
			this.post({ type: "error", error: error.message });
		});
		child.once("close", (code) => {
			if (!ready) rejectReady(new Error(`ghg bridge exited with code ${code ?? "unknown"}`));
			if (this.bridge === child) {
				this.bridge = undefined;
				this.bridgeReady = undefined;
			}
			if (this.active) {
				this.active = false;
				this.post({ type: "turn_end" });
			}
		});
		return promise;
	}

	private async handleBridgeEvent(event: Event): Promise<void> {
		if (event.type === "permission_request") {
			const approval = event.approval as Record<string, unknown> | undefined;
			if (!approval || typeof approval.id !== "string") return;
			const choice = await vscode.window.showQuickPick(
				["Allow once", "Allow always", "Deny"],
				{ placeHolder: `${String(approval.tool || "tool")}: ${String(approval.command || "Allow operation?")}` },
			);
			const decision = choice === "Allow always" ? "allow_always" : choice === "Allow once" ? "allow_once" : "deny";
			await this.bridgeCommand("approve", { id: approval.id, decision });
			return;
		}
		if (event.type === "question_request" && Array.isArray(event.questions)) {
			const answers: Array<{ id: string; value: string }> = [];
			for (const item of event.questions as Array<Record<string, unknown>>) {
				if (typeof item.id !== "string") continue;
				const options = Array.isArray(item.options) ? item.options.map((option) => String((option as Record<string, unknown>).label || "")) : [];
				const value = options.length > 0
					? await vscode.window.showQuickPick(options, { placeHolder: String(item.question || "Choose an option") })
					: await vscode.window.showInputBox({ prompt: String(item.question || "Answer") });
				if (value === undefined) {
					await this.bridgeCommand("answer_question", { id: event.id, cancelled: true });
					return;
				}
				answers.push({ id: item.id, value });
			}
			await this.bridgeCommand("answer_question", { id: event.id, answers });
		}
	}

	private async bridgeCommand(name: string, payload: unknown = null): Promise<void> {
		await this.ensureBridge();
		if (!this.bridge?.stdin) {
			throw new Error("ghg bridge is unavailable");
		}
		const request = {
			type: "command",
			request_id: `vscode-${++this.bridgeRequest}`,
			name,
			payload,
		};
		this.bridge.stdin.write(JSON.stringify(request) + "\n");
	}

	private stopBridge(): Promise<void> {
		const child = this.bridge;
		this.bridge = undefined;
		this.bridgeReady = undefined;
		this.active = false;
		if (!child) return Promise.resolve();
		const stopped = new Promise<void>((resolve) => child.once("close", () => resolve()));
		child.stdin?.end();
		const timer = setTimeout(() => interrupt(child), 2000);
		child.once("close", () => clearTimeout(timer));
		return stopped;
	}

	private async submitInput(prompt: string, role: Role, mode: "execute" | "plan", flags: { ask?: boolean; review?: boolean }, references: string[]): Promise<void> {
		await this.bridgeCommand("configure_role", { role, mode });
		await this.bridgeCommand("input", {
			input: promptWithReferences(prompt, references),
			authored: true,
			plan_mode: mode === "plan",
			review_mode: flags.review === true,
			ask_mode: flags.ask === true,
		});
	}

	private async sendCommand(prompt: string, message: WebviewMessage): Promise<void> {
		if (prompt.startsWith("!")) {
			const command = prompt.slice(1).trim();
			if (!command) throw new Error("usage: !<command>");
			return this.bridgeCommand("shell", { command });
		}
		const fields = prompt.trim().split(/\s+/);
		const name = fields[0];
		const args = prompt.trim().slice(name.length).trim();
		const role: Role = message.role === "default" || message.role === "smart" || message.role === "tiny" || message.role === "fast" ? message.role : "fast";
		const references = cleanReferences(message.references);
		switch (name) {
		case "/ask":
			if (!args) throw new Error("usage: /ask <question>");
			this.active = true;
			this.post({ type: "turn_start", mode: "chat" });
			await this.submitInput(args, role, "execute", { ask: true }, references);
			return;
		case "/plan":
			await this.bridgeCommand("configure_role", { role, mode: "plan" });
			if (!args) {
				this.post({ type: "notice", text: "switched to plan mode (read-only exploration)" });
				return;
			}
			this.active = true;
			this.post({ type: "turn_start", mode: "plan" });
			await this.bridgeCommand("input", { input: promptWithReferences(args, references), authored: true, plan_mode: true });
			return;
		case "/execute": {
			const plan = args || (typeof message.plan === "string" ? message.plan : "");
			if (!plan) throw new Error("no plan to execute — use /plan <goal> first");
			this.active = true;
			this.post({ type: "turn_start", mode: "chat" });
			await this.submitInput(`Execute the following approved plan. Create and maintain a todowrite checklist while implementing it.\n\n${plan}`, "fast", "execute", {}, references);
			return;
		}
		case "/review":
			if (!args) throw new Error("usage: /review <target or instructions>");
			this.active = true;
			this.post({ type: "turn_start", mode: "chat" });
			await this.submitInput(args, "smart", "execute", { review: true }, references);
			return;
		case "/compact":
			if (args === "retry") return this.bridgeCommand("compact_retry");
			if (args === "log") throw new Error("/compact log is not available in the extension yet");
			if (args) throw new Error("usage: /compact [retry]");
			return this.bridgeCommand("compact");
		case "/lsp":
			return this.bridgeCommand("lsp_status");
		case "/context-doctor":
			return this.bridgeCommand("context_doctor");
		case "/goal-from-context":
			return this.bridgeCommand("goal_from_context", { window: args ? Number(args) : 8 });
		case "/mcp": {
			if (!args) return this.bridgeCommand("mcp_status");
			const [server, action = "reconnect"] = fields.slice(1);
			if (!["reconnect", "enable", "disable"].includes(action)) throw new Error("usage: /mcp [name] [reconnect|enable|disable]");
			return this.bridgeCommand(`mcp_${action}`, { name: server });
		}
		case "/cd":
			if (!args) throw new Error("usage: /cd <directory>");
			return this.bridgeCommand("chdir", args);
		case "/rename":
			if (!args) throw new Error("usage: /rename <title>");
			return this.bridgeCommand("rename", { title: args });
		case "/effort":
			if (!args) throw new Error("usage: /effort <off|low|medium|high>");
			return this.bridgeCommand("configure_role", { role, mode: message.mode === "plan" ? "plan" : "execute", effort: args });
		case "/model": {
			if (!args) return this.pickModel(message.mode === "plan" ? "plan" : "chat");
			const [model, provider] = fields.slice(1);
			await this.bridgeCommand("set_role_model", { model, provider, role, mode: message.mode === "plan" ? "plan" : "execute" });
			this.post({ type: "role_model", role, model });
			return;
		}
		case "/pwd":
			this.post({ type: "notice", text: currentWorkspace()?.uri.fsPath || "" });
			return;
		case "/clear":
			return this.newSession();
		case "/resume":
			return this.resumeSession(args || undefined);
		case "/quit":
			void this.stopBridge();
			return;
		case "/commands":
		case "/help":
			this.post({ type: "notice", text: "Worker commands: /ask /plan /execute /review /compact /lsp /mcp /context-doctor /goal-from-context /cd /rename /effort /model /pwd /clear /resume /quit and !<command>" });
			return;
		default:
			throw new Error(`${name} is not available in the extension yet`);
		}
	}

	private async handleMessage(raw: unknown): Promise<void> {
		if (!raw || typeof raw !== "object" || typeof (raw as WebviewMessage).type !== "string") {
			return;
		}
		const message = raw as WebviewMessage;
		switch (message.type) {
			case "send":
				await this.send(message);
				break;
			case "stop":
				if (this.bridge) {
					try {
						await this.bridgeCommand("cancel");
					} catch (error) {
						this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
					}
				}
				break;
			case "configureRole": {
				const role = message.role === "default" || message.role === "smart" || message.role === "tiny" || message.role === "fast" ? message.role : "fast";
				const mode = message.mode === "plan" ? "plan" : "execute";
				try {
					await this.bridgeCommand("configure_role", { role, mode });
				} catch (error) {
					this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
				}
				break;
			}
			case "completeReferences": {
				const workspace = currentWorkspace();
				const query = typeof message.query === "string" ? message.query.slice(0, 256) : "";
				const requestId = typeof message.requestId === "number" ? message.requestId : 0;
				let items: ReferenceSuggestion[] = [];
				try {
					items = workspace ? await referenceSuggestions(workspace, query) : [];
				} catch {
					// Completion is optional when workspace search is unavailable.
				}
				this.post({ type: "referenceSuggestions", requestId, items });
				break;
			}
		}
	}

	private async send(message: WebviewMessage): Promise<void> {
		if (this.active) {
			this.post({ type: "error", error: "A ghg turn is already running." });
			return;
		}
		if (typeof message.prompt !== "string" || !message.prompt.trim()) {
			this.post({ type: "error", error: "Enter a prompt first." });
			return;
		}
		const mode = message.mode === "plan" ? "plan" : message.mode === "chat" ? "chat" : undefined;
		if (!mode) {
			return;
		}
		const role: Role = message.role === "default" || message.role === "smart" || message.role === "tiny" || message.role === "fast" ? message.role : "fast";
		try {
			const prompt = message.prompt.trim();
			if (prompt.startsWith("/") || prompt.startsWith("!")) {
				await this.sendCommand(prompt, message);
				return;
			}
			this.active = true;
			this.post({ type: "turn_start", mode });
			await this.submitInput(prompt, role, mode === "plan" ? "plan" : "execute", {}, cleanReferences(message.references));
		} catch (error) {
			this.active = false;
			this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
			this.post({ type: "turn_end" });
		}
	}

	async newSession(): Promise<void> {
		if (this.active) {
			this.post({ type: "error", error: "Stop the current ghg turn before starting a new session." });
			return;
		}
		await this.stopBridge();
		await this.extension.workspaceState.update(sessionKey(currentWorkspace()), undefined);
		this.post({ type: "new_session" });
	}

	dispose(): void {
		void this.stopBridge();
	}

	async resumeSession(requestedID?: string): Promise<void> {
		if (this.active) {
			this.post({ type: "error", error: "Stop the current ghg turn before resuming a session." });
			return;
		}
		await this.stopBridge();
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		let sessions: SessionPick[] = [];
		try {
			sessions = await listSessions(binary, workspace?.uri.fsPath);
		} catch {
			// Fall back to manual entry when the session database is unavailable.
		}
		let id: string | undefined = requestedID;
		if (!id && sessions.length > 0) {
			const pick = await vscode.window.showQuickPick(sessions, {
				placeHolder: "Resume a ghg session",
				matchOnDescription: true,
			});
			id = pick?.sessionId;
		} else {
			id = await vscode.window.showInputBox({ prompt: "ghg session ID" });
		}
		if (!id?.trim()) {
			return;
		}
		await this.extension.workspaceState.update(sessionKey(workspace), id.trim());
		try {
			await this.ensureBridge();
		} catch (error) {
			this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
			return;
		}
		this.post({ type: "resume_session", session_id: id.trim() });
	}
}

export function activate(extension: vscode.ExtensionContext): void {
	const provider = new GHGViewProvider(extension);
	extension.subscriptions.push(
		vscode.window.registerWebviewViewProvider("ghg.chatView", provider),
		vscode.commands.registerCommand("ghg.newSession", () => provider.newSession()),
		vscode.commands.registerCommand("ghg.resumeSession", () => provider.resumeSession()),
		{ dispose: () => provider.dispose() },
	);
}

export function deactivate(): void {}
