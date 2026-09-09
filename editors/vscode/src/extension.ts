import { randomBytes } from "node:crypto";
import * as readline from "node:readline";
import { ChildProcess, spawn } from "node:child_process";
import * as vscode from "vscode";

type Event = { type?: unknown; [key: string]: unknown };
type Workspace = vscode.WorkspaceFolder | undefined;
type Role = "default" | "smart" | "tiny" | "fast";
const effortLevels = new Set(["", "low", "medium", "high"]);

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

function commandMayRunDuringTurn(prompt: string): boolean {
	const name = prompt.trim().split(/\s+/, 1)[0];
	return ["/approval", "/commands", "/detach", "/help", "/notify", "/pwd", "/rename", "/q", "/quit", "/exit"].includes(name);
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
	let parsed: unknown;
	try {
		parsed = JSON.parse(output);
	} catch {
		return [];
	}
	if (!Array.isArray(parsed)) return [];
	return parsed.flatMap((item): SessionPick[] => {
		if (!item || typeof item !== "object") return [];
		const value = item as Record<string, unknown>;
		if (typeof value.id !== "string" || value.id === "") return [];
		const title = typeof value.title === "string" && value.title !== "" ? value.title : "(untitled)";
		const model = typeof value.model === "string" ? value.model : "";
		const updated = typeof value.updated_at === "string" ? value.updated_at : "";
		return [{ label: value.id, description: [title, model, updated].filter(Boolean).join(" · "), sessionId: value.id }];
	});
}

function listSessions(binary: string, cwd: string | undefined): Promise<SessionPick[]> {
	return new Promise((resolve, reject) => {
		const child = spawn(binary, ["sessions", "--format", "json"], { cwd, stdio: ["ignore", "pipe", "pipe"] });
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
				reject(new Error(error.trim() || `ghg ${args.join(" ")} exited with code ${code ?? "unknown"}`));
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
		<button type="button" id="mode-toggle" aria-label="Mode: Execute" title="Switch mode">Execute</button>
			<select id="role" aria-label="Current model">
			  <option value="default">Configured</option>
			  <option value="smart">Configured</option>
			  <option value="tiny">Configured</option>
			  <option value="fast">Configured</option>
			</select>
			<select id="effort" aria-label="Thinking effort">
		  <option value="">Off</option>
		  <option value="low">Low</option>
		  <option value="medium">Medium</option>
		  <option value="high">High</option>
		</select>
      <button type="submit" id="send" aria-label="Send" title="Send">➤</button>
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
	private webviewReady = false;
	private bridgeWorkspace = "";
	private bridgeSession = "";
	private bridgeBinary = "";
	private bufferedEvents: Event[] = [];
	private lastSnapshot?: Event;
	private promptIDs = new Set<string>();
	private detachWaiters = new Map<string, { resolve: () => void; reject: (error: Error) => void }>();

	constructor(private readonly extension: vscode.ExtensionContext) {}

	resolveWebviewView(view: vscode.WebviewView): void {
		this.view = view;
		this.webviewReady = false;
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
				this.webviewReady = false;
			}
		});
	}

	private async refreshModels(): Promise<void> {
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		try {
			this.post({ type: "models", models: await listModels(binary, workspace?.uri.fsPath) });
		} catch (error) {
			this.post({ type: "notice", text: `model discovery unavailable: ${error instanceof Error ? error.message : String(error)}` });
		}
	}

	private post(message: unknown): void {
		const event = message && typeof message === "object" ? message as Event : undefined;
		if (event?.type === "snapshot") {
			this.lastSnapshot = event;
			if (!this.view) this.bufferedEvents = [];
		}
		if (!this.view || !this.webviewReady) {
			if (event?.type !== "snapshot") this.bufferedEvents.push(event || {});
			return;
		}
		void this.view.webview.postMessage(message);
	}

	private flushBufferedEvents(): void {
		if (!this.view || !this.webviewReady) return;
		if (this.lastSnapshot) void this.view.webview.postMessage(this.lastSnapshot);
		const events = this.bufferedEvents;
		this.bufferedEvents = [];
		for (const event of events) void this.view.webview.postMessage(event);
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
		}, rolePick.role, mode === "plan" ? "plan" : "execute");
		this.post({ type: "role_model", role: rolePick.role, model: modelPick.item.model });
	}

	private async ensureBridge(initialRole: Role = "fast", initialMode: "execute" | "plan" = "execute"): Promise<void> {
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		const sessionId = this.extension.workspaceState.get<string>(sessionKey(workspace)) || "";
		const workspaceID = workspace?.uri.toString() || "global";
		const sessionMatches = this.bridgeSession === sessionId || (sessionId === "" && this.bridgeSession !== "");
		if (this.bridge && this.bridgeReady && this.bridgeWorkspace === workspaceID && sessionMatches && this.bridgeBinary === binary) {
			return this.bridgeReady;
		}
		if (this.bridge) await this.stopBridge();
		const args = ["bridge", "--role", initialRole, "--mode", initialMode];
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
		this.bridgeWorkspace = workspaceID;
		this.bridgeSession = sessionId;
		this.bridgeBinary = binary;
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
					this.bridgeSession = event.session_id;
					void this.extension.workspaceState.update(sessionKey(workspace), event.session_id);
				}
				resolveReady();
				return;
			}
			if (event.type === "detach_ack" && typeof event.request_id === "string") {
				const waiter = this.detachWaiters.get(event.request_id);
				if (waiter) {
					this.detachWaiters.delete(event.request_id);
					waiter.resolve();
				}
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
		child.stdin.on("error", (error: Error) => {
			if (this.bridge === child) {
				this.post({ type: "error", error: `ghg bridge input failed: ${error.message}` });
			}
		});
		child.stderr.on("data", () => {});
		child.once("error", (error) => {
			if (!ready) rejectReady(error instanceof Error ? error : new Error(String(error)));
			this.post({ type: "error", error: error.message });
		});
		child.once("close", (code) => {
			if (!ready) rejectReady(new Error(`ghg bridge exited with code ${code ?? "unknown"}`));
			for (const waiter of this.detachWaiters.values()) waiter.reject(new Error("ghg bridge closed before detaching"));
			this.detachWaiters.clear();
			if (this.bridge === child) {
				this.bridge = undefined;
				this.bridgeReady = undefined;
				this.bridgeWorkspace = "";
				this.bridgeSession = "";
				this.bridgeBinary = "";
				this.lastSnapshot = undefined;
			}
			if (this.active) {
				this.active = false;
				this.post({ type: "turn_end" });
			}
		});
		return promise;
	}

	private async showApproval(approval: Record<string, unknown>): Promise<void> {
		const id = typeof approval.id === "string" ? approval.id : "";
		const child = this.bridge;
		if (!id || !child || this.promptIDs.has(id)) return;
		this.promptIDs.add(id);
		try {
			const tool = String(approval.tool || "tool");
			const command = String(approval.command || "Allow operation?");
			const rule = String(approval.rule || "");
			const choice = await vscode.window.showQuickPick(
				["Allow once", "Allow always", "Deny", "Deny with instruction"],
				{ placeHolder: `${tool}: ${command}${rule ? ` · always: ${rule}` : ""}` },
			);
			if (this.bridge !== child) return;
			if (choice === "Deny with instruction") {
				const redirect = await vscode.window.showInputBox({
					prompt: "Tell ghg what to do instead",
					ignoreFocusOut: true,
					validateInput: (value) => value.length > 4096 ? "Instruction is limited to 4096 characters." : undefined,
				});
				await this.writeBridgeCommand(child, "approve", {
					id, decision: "reject", redirect: redirect?.trim() || "approval instruction was dismissed",
				});
				return;
			}
			const decision = choice === "Allow always" ? "allow_always" : choice === "Allow once" ? "allow_once" : "reject";
			await this.writeBridgeCommand(child, "approve", { id, decision, redirect: choice ? undefined : "approval prompt was dismissed" });
		} finally {
			this.promptIDs.delete(id);
		}
	}

	private async showQuestion(request: Record<string, unknown>): Promise<void> {
		const id = typeof request.id === "string" ? request.id : "";
		const child = this.bridge;
		if (!id || !child || this.promptIDs.has(id)) return;
		this.promptIDs.add(id);
		try {
			const answers: Array<{ id: string; value: string }> = [];
			for (const item of Array.isArray(request.questions) ? request.questions : []) {
				if (!item || typeof item !== "object") continue;
				const question = item as Record<string, unknown>;
				if (typeof question.id !== "string") continue;
				const options = Array.isArray(question.options) ? question.options
					.filter((option) => option && typeof option === "object")
					.map((option) => {
						const value = option as Record<string, unknown>;
						return {
							label: String(value.label || ""),
							description: typeof value.description === "string" ? value.description : undefined,
							value: String(value.label || ""),
						};
					}) : [];
				let value: string | undefined;
				if (options.length > 0) {
					const choice = await vscode.window.showQuickPick(
						[...options, { label: "Other…", description: "Enter a custom answer", value: "__other__" }],
						{ placeHolder: String(question.question || "Choose an option"), matchOnDescription: true },
					);
					if (choice?.value === "__other__") {
						value = await vscode.window.showInputBox({
							prompt: String(question.question || "Answer"),
							ignoreFocusOut: true,
							validateInput: (input) => input.length > 4096 ? "Answer is limited to 4096 characters." : undefined,
						});
					} else {
						value = choice?.value;
					}
				} else {
					value = await vscode.window.showInputBox({
						prompt: String(question.question || "Answer"),
						ignoreFocusOut: true,
						validateInput: (input) => input.length > 4096 ? "Answer is limited to 4096 characters." : undefined,
					});
				}
				if (value === undefined) {
					if (this.bridge === child) await this.writeBridgeCommand(child, "answer_question", { id, cancelled: true });
					return;
				}
				answers.push({ id: question.id, value: value.trim() });
			}
			if (this.bridge === child) await this.writeBridgeCommand(child, "answer_question", { id, answers });
		} finally {
			this.promptIDs.delete(id);
		}
	}

	private async handleBridgeEvent(event: Event): Promise<void> {
		if (event.type === "permission_request") {
			const approval = event.approval as Record<string, unknown> | undefined;
			if (approval) await this.showApproval(approval);
			return;
		}
		if (event.type === "question_request") {
			await this.showQuestion(event);
			return;
		}
		if (event.type === "snapshot") {
			const snapshot = event.snapshot as Record<string, unknown> | undefined;
			if (!snapshot) return;
			if (snapshot.pending_approval && typeof snapshot.pending_approval === "object") {
				await this.showApproval(snapshot.pending_approval as Record<string, unknown>);
			}
			if (snapshot.pending_question && typeof snapshot.pending_question === "object") {
				await this.showQuestion(snapshot.pending_question as Record<string, unknown>);
			}
		}
	}

	private writeBridgeCommand(child: ChildProcess, name: string, payload: unknown = null, requestID = `vscode-${++this.bridgeRequest}`): Promise<void> {
		const stdin = child.stdin;
		if (!stdin || stdin.destroyed || stdin.writableEnded) return Promise.reject(new Error("ghg bridge input is unavailable"));
		const request = {
			type: "command",
			request_id: requestID,
			name,
			payload,
		};
		return new Promise((resolve, reject) => {
			let settled = false;
			const finish = (error?: Error) => {
				if (settled) return;
				settled = true;
				stdin.removeListener("error", onError);
				stdin.removeListener("drain", onDrain);
				child.removeListener("close", onClose);
				if (error) reject(error);
				else resolve();
			};
			const onError = (error: Error) => finish(error);
			const onDrain = () => finish();
			const onClose = () => finish(new Error("ghg bridge closed before accepting the command"));
			stdin.once("error", onError);
			child.once("close", onClose);
			let accepted = false;
			try {
				accepted = stdin.write(JSON.stringify(request) + "\n", "utf8", () => {
					if (accepted) finish();
				});
				if (!accepted) stdin.once("drain", onDrain);
			} catch (error) {
				finish(error instanceof Error ? error : new Error(String(error)));
			}
		});
	}

	private async detachBridge(): Promise<void> {
		const child = this.bridge;
		if (!child) return;
		const requestID = `vscode-${++this.bridgeRequest}`;
		const detached = new Promise<void>((resolve, reject) => this.detachWaiters.set(requestID, { resolve, reject }));
		try {
			await this.writeBridgeCommand(child, "detach", null, requestID);
			await detached;
			this.active = false;
			await this.stopBridge();
		} finally {
			this.detachWaiters.delete(requestID);
		}
	}

	private async bridgeCommand(name: string, payload: unknown = null, initialRole: Role = "fast", initialMode: "execute" | "plan" = "execute"): Promise<void> {
		await this.ensureBridge(initialRole, initialMode);
		if (!this.bridge?.stdin) {
			throw new Error("ghg bridge is unavailable");
		}
		const child = this.bridge;
		if (!child) throw new Error("ghg bridge is unavailable");
		await this.writeBridgeCommand(child, name, payload);
	}

	private stopBridge(): Promise<void> {
		const child = this.bridge;
		this.bridge = undefined;
		this.bridgeReady = undefined;
		this.bridgeWorkspace = "";
		this.bridgeSession = "";
		this.bridgeBinary = "";
		this.lastSnapshot = undefined;
		this.bufferedEvents = [];
		this.active = false;
		if (!child) return Promise.resolve();
		if (child.exitCode !== null) return Promise.resolve();
		const stopped = new Promise<void>((resolve) => child.once("close", () => resolve()));
		child.stdin?.end();
		const timer = setTimeout(() => interrupt(child), 2000);
		child.once("close", () => clearTimeout(timer));
		return stopped;
	}

	async reloadBridge(): Promise<void> {
		if (!this.active) await this.stopBridge();
	}

	private async submitInput(prompt: string, role: Role, mode: "execute" | "plan", flags: { ask?: boolean; review?: boolean }, references: string[]): Promise<void> {
		await this.bridgeCommand("configure_role", { role, mode }, role, mode);
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
			await this.bridgeCommand("configure_role", { role, mode: "plan" }, role, "plan");
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
			await this.submitInput(args, role, "execute", { review: true }, references);
			return;
		case "/continue": {
			if (args) throw new Error("usage: /continue");
			if (this.active) throw new Error("A ghg turn is already running.");
			const continueMode = message.mode === "plan" ? "plan" : "execute";
			this.active = true;
			this.post({ type: "turn_start", mode: continueMode });
			await this.bridgeCommand("input", {
				input: "continue",
				authored: true,
				plan_mode: continueMode === "plan",
				review_mode: message.mode === "review",
			});
			return;
		}
		case "/compact":
			if (args === "retry") return this.bridgeCommand("compact_retry");
			if (args === "log") throw new Error("/compact log is not available in the extension yet");
			if (args) throw new Error("usage: /compact [retry]");
			return this.bridgeCommand("compact");
		case "/approval":
			if (args !== "ask" && args !== "auto-review" && args !== "never") throw new Error("usage: /approval <ask|auto-review|never>");
			return this.bridgeCommand("configure", { approval: args });
		case "/notify":
			if (args === "config") {
				const botToken = await vscode.window.showInputBox({
					prompt: "Telegram bot token",
					password: true,
					ignoreFocusOut: true,
				});
				if (!botToken?.trim()) return;
				const chatID = await vscode.window.showInputBox({
					prompt: "Telegram chat ID",
					ignoreFocusOut: true,
				});
				if (!chatID?.trim()) return;
				return this.bridgeCommand("notify", { action: "config", bot_token: botToken.trim(), chat_id: chatID.trim() });
			}
			if (args !== "" && args !== "on" && args !== "off") throw new Error("usage: /notify [config|on|off]");
			return this.bridgeCommand("notify", { action: args || "status" });
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
			{
				const effort = args.toLowerCase() === "off" ? "" : args.toLowerCase();
				if (!effortLevels.has(effort)) throw new Error("usage: /effort <off|low|medium|high>");
				const mode = message.mode === "plan" ? "plan" : "execute";
				return this.bridgeCommand("configure_role", { role, mode, effort, update_effort: true }, role, mode);
			}
		case "/model": {
			if (!args) return this.pickModel(message.mode === "plan" ? "plan" : "chat");
			const [model, provider] = fields.slice(1);
			const mode = message.mode === "plan" ? "plan" : "execute";
			await this.bridgeCommand("set_role_model", { model, provider, role, mode }, role, mode);
			this.post({ type: "role_model", role, model });
			return;
		}
		case "/pwd":
			this.post({ type: "notice", text: currentWorkspace()?.uri.fsPath || "" });
			return;
		case "/detach":
			await this.detachBridge();
			this.post({ type: "notice", text: "ghg worker detached; it will continue in the background." });
			return;
		case "/clear":
			return this.newSession();
		case "/resume":
			return this.resumeSession(args || undefined);
		case "/quit":
		case "/exit":
		case "/q":
			void this.stopBridge();
			return;
		case "/commands":
		case "/help":
			this.post({ type: "notice", text: "Extension commands: /ask /plan /execute /review /continue /compact /approval /notify /lsp /mcp /context-doctor /goal-from-context /cd /detach /rename /effort /model /pwd /clear /resume /quit (/exit, /q) and !<command>" });
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
			case "ready":
				this.webviewReady = true;
				this.flushBufferedEvents();
				break;
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
				if (this.active) {
					break;
				}
				const role = message.role === "default" || message.role === "smart" || message.role === "tiny" || message.role === "fast" ? message.role : "fast";
				const mode = message.mode === "plan" ? "plan" : "execute";
				try {
					const updateEffort = message.updateEffort === true || typeof message.effort === "string";
					const effort = typeof message.effort === "string" ? message.effort.trim().toLowerCase() : "";
					const normalizedEffort = effort === "off" ? "" : effort;
					if (updateEffort && !effortLevels.has(normalizedEffort)) {
						throw new Error("unsupported thinking effort; choose off, low, medium, or high");
					}
					await this.bridgeCommand("configure_role", { role, mode, effort: normalizedEffort, update_effort: updateEffort }, role, mode);
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
			case "openExternal": {
				if (typeof message.uri !== "string") break;
				const uri = vscode.Uri.parse(message.uri);
				if (!(["http", "https", "file"] as string[]).includes(uri.scheme)) {
					this.post({ type: "error", error: "Only http, https, and file links can be opened." });
					break;
				}
				await vscode.env.openExternal(uri);
				break;
			}
		}
	}

	private async send(message: WebviewMessage): Promise<void> {
		if (typeof message.prompt !== "string" || !message.prompt.trim()) {
			this.post({ type: "error", error: "Enter a prompt first." });
			return;
		}
		const prompt = message.prompt.trim();
		const activeBefore = this.active;
		if (activeBefore && !prompt.startsWith("/") && !prompt.startsWith("!")) {
			try {
				await this.bridgeCommand("append", { content: promptWithReferences(prompt, cleanReferences(message.references)) });
			} catch (error) {
				this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
			}
			return;
		}
		if (activeBefore && !commandMayRunDuringTurn(prompt)) {
			this.post({ type: "error", error: "A ghg turn is already running." });
			return;
		}
		const mode = message.mode === "plan" ? "plan" : message.mode === "review" ? "review" : message.mode === "execute" || message.mode === "chat" ? "execute" : undefined;
		if (!mode) {
			return;
		}
		const role: Role = message.role === "default" || message.role === "smart" || message.role === "tiny" || message.role === "fast" ? message.role : "fast";
		try {
			if (prompt.startsWith("/") || prompt.startsWith("!")) {
				await this.sendCommand(prompt, message);
				return;
			}
			this.active = true;
			this.post({ type: "turn_start", mode });
			if (mode === "review") {
				await this.submitInput(prompt, role, "execute", { review: true }, cleanReferences(message.references));
			} else {
				await this.submitInput(prompt, role, mode === "plan" ? "plan" : "execute", {}, cleanReferences(message.references));
			}
		} catch (error) {
			this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
			if (!activeBefore) {
				this.active = false;
				this.post({ type: "turn_end" });
			}
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
		} else if (!id) {
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
		vscode.workspace.onDidChangeConfiguration((event) => {
			if (event.affectsConfiguration("ghg.binaryPath")) void provider.reloadBridge();
		}),
		{ dispose: () => provider.dispose() },
	);
}

export function deactivate(): void {}
