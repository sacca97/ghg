import { randomBytes } from "node:crypto";
import { ChildProcess, execFileSync, spawn } from "node:child_process";
import * as fs from "node:fs";
import { isAbsolute, resolve } from "node:path";
import * as readline from "node:readline";
import * as vscode from "vscode";

type Event = { type?: unknown; [key: string]: unknown };
type Workspace = vscode.WorkspaceFolder | undefined;
// One list per enum, mirroring media/constants.js. Parsing, validation, and the
// option order the webview renders are all derived from these tables.
const roles = ["default", "smart", "fast", "tiny"] as const;
const modes = ["execute", "plan", "review"] as const;
const composerModes = ["execute", "plan", "review", "ask"] as const;
const approvals = ["", "ask", "auto", "never"] as const;
const sandboxes = ["", "read-only", "workspace-write", "danger-full-access"] as const;
const networks = ["", "deny", "host"] as const;

type Role = (typeof roles)[number];
type Mode = (typeof composerModes)[number];

const oneOf = <T extends string>(values: readonly T[], value: unknown): value is T =>
	typeof value === "string" && (values as readonly string[]).includes(value);

function normalizeEffort(value: unknown): string | undefined {
	if (typeof value !== "string") return undefined;
	const effort = value.trim().toLowerCase();
	if (effort === "" || effort === "off" || effort === "none" || effort === "default") return "";
	return /^[a-z0-9][a-z0-9_-]{0,31}$/.test(effort) ? effort : undefined;
}

const maxChildOutput = 1024 * 1024;
const busyStates = new Set(["running", "waiting_approval", "waiting_question", "stopping"]);
const executionSettings = new Set(["sandbox", "network", "approval"]);
const defaultSettings = new Set(["defaultRole", "defaultMode", "defaultEffort"]);
const binaryPathCache = new Map<string, string>();

type SessionPick = vscode.QuickPickItem & { sessionId: string };
type ReferenceSuggestion = { path: string; folder: boolean };
type SteerRequest = { prompt: string; role: Role; mode: Mode; references: string[] };
type ExtensionSettings = {
	role: Role;
	mode: Mode;
	effort: string;
	sandbox: string;
	network: string;
	approval: string;
	binary: string;
};

const sessionKey = (workspace: Workspace): string =>
	`session:${workspace?.uri.toString() ?? "global"}`;

function currentWorkspace(): Workspace {
	const editor = vscode.window.activeTextEditor;
	return (editor && vscode.workspace.getWorkspaceFolder(editor.document.uri)) || vscode.workspace.workspaceFolders?.[0];
}

function displayBinaryPath(binary: string): string {
	const value = binary.trim() || "ghg";
	if (isAbsolute(value)) return value;
	const cwd = currentWorkspace()?.uri.fsPath || process.cwd();
	const cacheKey = `${cwd}\0${value}`;
	const cached = binaryPathCache.get(cacheKey);
	if (cached !== undefined) return cached;
	if (value.includes("/") || value.includes("\\")) {
		const resolved = resolve(cwd, value);
		binaryPathCache.set(cacheKey, resolved);
		return resolved;
	}
	let resolved = value;
	try {
		const resolver = process.platform === "win32" ? "where.exe" : "which";
		const path = execFileSync(resolver, [value], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] })
			.split(/\r?\n/, 1)[0]
			?.trim();
		if (path) resolved = path;
	} catch {
		// Keep the configured command when it is not available on PATH.
	}
	binaryPathCache.set(cacheKey, resolved);
	return resolved;
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
		const path = cleanWorkspacePath(item);
		if (!path) {
			continue;
		}
		paths.add(path);
	}
	return [...paths].slice(0, 32);
}

function cleanWorkspacePath(value: string): string | undefined {
	const path = value.trim().replace(/\\/g, "/").replace(/^\.\//, "");
	if (!path || path.startsWith("/") || /^[A-Za-z]:/.test(path) || /[\r\n\0]/.test(path)) {
		return undefined;
	}
	if (path.split("/").some((segment) => segment === "..")) {
		return undefined;
	}
	return path;
}

function promptWithReferences(prompt: string, paths: string[]): string {
	if (paths.length === 0) {
		return prompt;
	}
	return `${prompt}\n\nReferenced workspace paths:\n${paths.map((path) => `- ${path}`).join("\n")}`;
}

function commandMayRunDuringTurn(prompt: string): boolean {
	const name = prompt.trim().split(/\s+/, 1)[0];
	return ["/approval", "/commands", "/detach", "/export", "/export-result", "/export-chat", "/export-log", "/help", "/notify", "/search-providers", "/pwd", "/rename", "/q", "/quit", "/exit"].includes(name);
}

function literalGlob(value: string): string {
	return value.replace(/[\\{}()[\]*?]/g, (character) => `\\${character}`);
}

function excludedReferencePath(path: string): boolean {
	return path.split("/").some((segment) => segment === ".git" || segment === ".ghg" || segment === "node_modules");
}

async function directoryReferenceSuggestions(workspace: vscode.WorkspaceFolder, query: string): Promise<ReferenceSuggestion[]> {
	const slash = query.lastIndexOf("/");
	if (slash < 0) return [];
	const parent = query.slice(0, slash);
	if (excludedReferencePath(parent)) return [];
	const parentURI = parent ? vscode.Uri.joinPath(workspace.uri, ...parent.split("/")) : workspace.uri;
	const entries = await vscode.workspace.fs.readDirectory(parentURI);
	return entries
		.map(([name, type]) => {
			const folder = (type & vscode.FileType.Directory) !== 0;
			return {
				path: `${parent ? `${parent}/` : ""}${name}${folder ? "/" : ""}`,
				folder,
			};
		})
		.filter((item) => item.path.toLowerCase().startsWith(query.toLowerCase()) && !excludedReferencePath(item.path))
		.sort((a, b) => a.path.localeCompare(b.path))
		.slice(0, 50);
}

async function referenceSuggestions(workspace: vscode.WorkspaceFolder, query: string): Promise<ReferenceSuggestion[]> {
	const normalized = cleanWorkspacePath(query) ?? (query.trim() === "" ? "" : undefined);
	if (normalized === undefined) {
		return [];
	}
	if (normalized.includes("/")) {
		return directoryReferenceSuggestions(workspace, normalized);
	}
	const pattern = normalized ? `**/*${literalGlob(normalized)}*` : "**/*";
	const files = await vscode.workspace.findFiles(
		new vscode.RelativePattern(workspace, pattern),
		"{**/.git/**,**/.ghg/**,**/node_modules/**}",
		200,
	);
	const prefix = normalized.toLowerCase();
	const candidates = new Map<string, boolean>();
	for (const uri of files) {
		const path = vscode.workspace.asRelativePath(uri, false).replace(/\\/g, "/");
		const lowerPath = path.toLowerCase();
		const basename = path.slice(path.lastIndexOf("/") + 1).toLowerCase();
		// normalized never contains "/" at this point (slash queries return early
		// above), so a match has to come from the basename.
		if (!lowerPath.startsWith(prefix) && !basename.startsWith(prefix)) {
			continue;
		}
		if (!lowerPath.startsWith(prefix)) {
			candidates.set(path, false);
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

function appendChildOutput(current: string, chunk: Buffer, keepTail = false): string {
	const next = current + chunk.toString();
	return next.length <= maxChildOutput ? next : keepTail ? next.slice(-maxChildOutput) : next.slice(0, maxChildOutput);
}

function runCommand(binary: string, cwd: string | undefined, args: string[]): Promise<string> {
	return new Promise((resolve, reject) => {
		const child = spawn(binary, args, { cwd, stdio: ["ignore", "pipe", "pipe"] });
		if (!child.stdout || !child.stderr) {
			reject(new Error("ghg did not expose piped output"));
			return;
		}
		let output = "";
		let error = "";
		child.stdout.on("data", (chunk: Buffer) => { output = appendChildOutput(output, chunk); });
		child.stderr.on("data", (chunk: Buffer) => { error = appendChildOutput(error, chunk, true); });
		child.once("error", reject);
		child.once("close", (code) => {
			if (code !== 0) {
				reject(new Error(error.trim() || `ghg ${args.join(" ")} exited with code ${code ?? "unknown"}`));
				return;
			}
			resolve(output);
		});
	});
}

function runJSONCommand(binary: string, cwd: string | undefined, args: string[]): Promise<unknown> {
	return runCommand(binary, cwd, args).then((output) => JSON.parse(output));
}

function defaultExportFilename(kind: string, format = "markdown"): string {
	const now = new Date();
	const pad = (value: number) => String(value).padStart(2, "0");
	const stamp = `${now.getUTCFullYear()}${pad(now.getUTCMonth() + 1)}${pad(now.getUTCDate())}-${pad(now.getUTCHours())}${pad(now.getUTCMinutes())}${pad(now.getUTCSeconds())}`;
	return `${kind || "result"}-${stamp}.${format === "json" ? "json" : "md"}`;
}

function normalizeExportKind(value: string): string | undefined {
	switch (value.toLowerCase()) {
	case "chat":
	case "plan":
	case "review":
		return value.toLowerCase();
	case "last":
	case "message":
	case "response":
		return "message";
	case "log":
	case "transcript":
		return "chat";
	default:
		return undefined;
	}
}

function listModels(binary: string, cwd: string | undefined): Promise<Record<string, string>> {
	return runJSONCommand(binary, cwd, ["models", "--format", "json"]).then(parseRoleModels);
}

function parseRoleModels(parsed: unknown): Record<string, string> {
	if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("ghg models returned invalid JSON");
	const models: Record<string, string> = {};
	for (const role of roles) {
		const model = (parsed as Record<string, unknown>)[role];
		if (typeof model === "string" && model !== "") models[role] = model;
	}
	return models;
}

type CatalogModel = { model: string; provider: string; reasoningEfforts?: string[] };

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

function htmlFor(webview: vscode.Webview, extensionUri: vscode.Uri, settings: ExtensionSettings): string {
	const nonce = randomBytes(16).toString("hex");
	const script = webview.asWebviewUri(vscode.Uri.joinPath(extensionUri, "media", "main.js"));
	const style = webview.asWebviewUri(vscode.Uri.joinPath(extensionUri, "media", "main.css"));
	const initialSettings = JSON.stringify(settings).replace(/</g, "\\u003c");
	let html = fs.readFileSync(vscode.Uri.joinPath(extensionUri, "media", "index.html").fsPath, "utf8");
	const values = { cspSource: webview.cspSource, nonce, scriptUri: script.toString(), styleUri: style.toString(), initialSettings };
	return html.replace(/{{(cspSource|nonce|scriptUri|styleUri|initialSettings)}}/g, (_, key: keyof typeof values) => values[key]);
}

type WebviewMessage = { type?: unknown; [key: string]: unknown };

// CommandSpec mirrors internal/worker.CommandSpec. The bridge sends the
// canonical catalogue on startup, so both clients render the same hints
// without a second hand-maintained command list.
type CommandSpec = {
	name: string;
	aliases?: string[];
	hint: string;
	owner: string;
};

// The commands this extension implements. Hints and usage text come from the
// bridge catalogue; only the supported set is declared here.
const extensionCommands = [
	"!cmd",
	"/approval", "/ask", "/cd", "/clear", "/compact", "/context-doctor", "/continue",
	"/detach", "/dynamic-reasoning", "/effort", "/execute", "/export", "/goal-from-context", "/help",
	"/lsp", "/mcp", "/model", "/notify", "/plan", "/pwd", "/quit", "/rename",
	"/resume", "/review", "/search-providers",
];

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
	private commands: CommandSpec[] = [];
	private pendingSteers: SteerRequest[] = [];
	private steerCancelRequested = false;
	private modelCache?: { key: string; models: Record<string, string>; capabilities: CatalogModel[] };

	constructor(private readonly extension: vscode.ExtensionContext) {}

	extensionSettings(): ExtensionSettings {
		const cfg = vscode.workspace.getConfiguration("ghg");
		const role = cfg.get<string>("defaultRole", "fast");
		const mode = cfg.get<string>("defaultMode", "execute");
		const effort = cfg.get<string>("defaultEffort", "");
		return {
			role: oneOf(roles, role) ? role : "fast",
			mode: oneOf(modes, mode) ? mode : "execute",
			effort: normalizeEffort(effort) ?? "",
			sandbox: cfg.get<string>("sandbox", ""),
			network: cfg.get<string>("network", ""),
			approval: cfg.get<string>("approval", ""),
			binary: displayBinaryPath(cfg.get<string>("binaryPath", "ghg")),
		};
	}

	resolveWebviewView(view: vscode.WebviewView): void {
		this.view = view;
		this.webviewReady = false;
		view.webview.options = {
			enableScripts: true,
			localResourceRoots: [vscode.Uri.joinPath(this.extension.extensionUri, "media")],
		};
		view.webview.html = htmlFor(view.webview, this.extension.extensionUri, this.extensionSettings());
		view.webview.onDidReceiveMessage((message: unknown) => {
			void this.handleMessage(message);
		});
		void this.loadModels();
		view.onDidDispose(() => {
			if (this.view === view) {
				this.view = undefined;
				this.webviewReady = false;
			}
		});
	}

	private async loadModels(): Promise<void> {
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		const key = `${workspace?.uri.fsPath || ""}\0${binary}`;
		if (this.modelCache?.key === key) {
			this.post({ type: "models", models: this.modelCache.models, capabilities: this.modelCache.capabilities });
			return;
		}
		try {
			const models = await listModels(binary, workspace?.uri.fsPath);
			let capabilities: CatalogModel[] = [];
			try {
				capabilities = await listCatalogModels(binary, workspace?.uri.fsPath);
			} catch {
				// Model names remain useful when the optional catalog cache is unavailable.
			}
			this.modelCache = { key, models, capabilities };
			this.post({ type: "models", models, capabilities });
		} catch (error) {
			this.post({ type: "notice", text: `model discovery unavailable: ${error instanceof Error ? error.message : String(error)}` });
		}
	}

	private async refreshModelCatalogs(): Promise<void> {
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		const parsed = await runJSONCommand(binary, workspace?.uri.fsPath, ["models", "--refresh", "--format", "json"]);
		let capabilities: CatalogModel[] = [];
		try {
			capabilities = await listCatalogModels(binary, workspace?.uri.fsPath);
		} catch {
			// Keep the refreshed role names even if catalog metadata is unavailable.
		}
		const models = parseRoleModels(parsed);
		this.modelCache = { key: `${workspace?.uri.fsPath || ""}\0${binary}`, models, capabilities };
		this.post({ type: "models", models, capabilities });
		this.post({ type: "notice", text: "model catalogs refreshed" });
	}

	post(message: unknown): void {
		const event = message && typeof message === "object" ? message as Event : undefined;
		if (event?.type === "snapshot") {
			this.lastSnapshot = event;
			if (!this.view || !this.webviewReady) this.bufferedEvents = [];
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

	private setActive(value: boolean): void {
		if (this.active === value) return;
		this.active = value;
		this.post({ type: "busy", value });
	}

	private async submitSteered(request: SteerRequest): Promise<void> {
		this.setActive(true);
		this.post({ type: "turn_start", mode: request.mode });
		try {
			if (request.mode === "review") {
				await this.submitInput(request.prompt, request.role, "execute", { review: true }, request.references);
			} else if (request.mode === "ask") {
				await this.submitInput(request.prompt, request.role, "execute", { ask: true }, request.references);
			} else {
				await this.submitInput(request.prompt, request.role, request.mode === "plan" ? "plan" : "execute", {}, request.references);
			}
		} catch (error) {
			this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
			this.setActive(false);
			this.post({ type: "turn_end" });
		}
	}

	private async pickModel(mode: "chat" | "plan", selectedRole?: Role): Promise<void> {
		const workspace = currentWorkspace();
		const binary = vscode.workspace.getConfiguration("ghg").get<string>("binaryPath", "ghg");
		const configured = await listModels(binary, workspace?.uri.fsPath);
		let role = selectedRole;
		if (!role) {
			const rolePick = await vscode.window.showQuickPick(
				roles.filter((name) => configured[name])
					.map((name) => ({ label: name, description: configured[name], role: name })),
				{ placeHolder: "Choose the model role to configure" },
			);
			if (!rolePick) return;
			role = rolePick.role;
		}
		if (!role) return;
		const available = await listCatalogModels(binary, workspace?.uri.fsPath);
		const picks = new Map<string, CatalogModel>();
		for (const item of available) {
			if (!picks.has(`${item.provider}\x00${item.model}`)) picks.set(`${item.provider}\x00${item.model}`, item);
		}
		if (picks.size === 0) throw new Error("no configured catalog models found");
		const modelPick = await vscode.window.showQuickPick(
			[...picks.values()].map((item) => ({ label: `${item.provider}/${item.model}`, description: item.model, item })),
			{ placeHolder: `Choose the model for ${role}`, matchOnDescription: true },
		);
		if (!modelPick) return;
		await this.bridgeCommand("configure", {
			role,
			model: modelPick.item.model,
			provider: modelPick.item.provider,
			mode: mode === "plan" ? "plan" : "execute",
			persist_role_model: true,
		}, roles.find((name) => configured[name]) || role, mode === "plan" ? "plan" : "execute");
		this.post({ type: "role_model", role, model: modelPick.item.model });
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
		const cfg = this.extensionSettings();
		if (cfg.sandbox) args.push("--sandbox", cfg.sandbox);
		if (cfg.network) args.push("--network", cfg.network);
		if (cfg.approval) args.push("--approval", cfg.approval);
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
		let stderr = "";
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
				if (Array.isArray(event.commands)) {
					this.commands = event.commands.filter((entry): entry is CommandSpec =>
						!!entry && typeof entry === "object" && typeof (entry as CommandSpec).name === "string");
				}
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
			if (event.type === "error" && typeof event.request_id === "string") {
				const waiter = this.detachWaiters.get(event.request_id);
				if (waiter) {
					this.detachWaiters.delete(event.request_id);
					waiter.reject(new Error(typeof event.error === "string" ? event.error : "ghg detach failed"));
				}
			}
			if (event.type === "turn_end") {
				this.setActive(false);
				this.steerCancelRequested = false;
				const nextSteer = this.pendingSteers.shift();
				if (nextSteer) queueMicrotask(() => void this.submitSteered(nextSteer));
			}
			if (event.type === "error" && this.active) {
				this.setActive(false);
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
		child.stderr.on("data", (chunk: Buffer) => {
			stderr = (stderr + chunk.toString()).slice(-4096);
		});
		child.once("error", (error) => {
			if (!ready) rejectReady(error instanceof Error ? error : new Error(String(error)));
			this.post({ type: "error", error: error.message });
		});
		child.once("close", (code) => {
			if (!ready) {
				const detail = stderr.trim() || `ghg bridge exited with code ${code ?? "unknown"}`;
				rejectReady(new Error(detail.replace(/^ghg:\s*/, "")));
			}
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
				this.setActive(false);
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
			this.setActive(typeof snapshot.state === "string" && busyStates.has(snapshot.state));
			if (snapshot.pending_approval && typeof snapshot.pending_approval === "object") {
				await this.showApproval(snapshot.pending_approval as Record<string, unknown>);
			}
			if (snapshot.pending_question && typeof snapshot.pending_question === "object") {
				await this.showQuestion(snapshot.pending_question as Record<string, unknown>);
			}
		}
		if (event.type === "state") {
			this.setActive(typeof event.state === "string" && busyStates.has(event.state));
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
			this.setActive(false);
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
		await this.writeBridgeCommand(this.bridge, name, payload);
	}

	private stopBridge(): Promise<void> {
		this.pendingSteers = [];
		this.steerCancelRequested = false;
		const child = this.bridge;
		this.bridge = undefined;
		this.bridgeReady = undefined;
		this.bridgeWorkspace = "";
		this.bridgeSession = "";
		this.bridgeBinary = "";
		this.lastSnapshot = undefined;
		this.bufferedEvents = [];
		this.setActive(false);
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

	async refreshModels(): Promise<void> {
		try {
			await this.refreshModelCatalogs();
		} catch (error) {
			this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
		}
	}

	async exportChat(): Promise<void> {
		const choice = await vscode.window.showQuickPick(
			[
				{ label: "Markdown", value: "markdown" },
				{ label: "JSON", value: "json" },
			],
			{ placeHolder: "Export chat as…" },
		);
		if (!choice) return;
		try {
			await this.exportResult("chat", "", choice.value);
		} catch (error) {
			this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
		}
	}

	private async exportResult(kind = "", destination = "", format = "", force = false): Promise<void> {
		const workspace = currentWorkspace();
		if (!workspace) throw new Error("open a workspace before exporting");
		const binary = this.extensionSettings().binary;
		const resolvedFormat = format || (destination.toLowerCase().endsWith(".json") ? "json" : "markdown");
		if (resolvedFormat !== "markdown" && resolvedFormat !== "json") throw new Error("usage: /export [chat|plan|review|last] [path] [--format json|markdown] [--force]");
		const filename = destination || defaultExportFilename(kind, resolvedFormat);
		const sessionID = this.bridgeSession || this.extension.workspaceState.get<string>(sessionKey(workspace)) || "";
		const args = ["export"];
		if (sessionID) args.push("--session", sessionID);
		if (kind) args.push("--kind", kind);
		args.push("--format", resolvedFormat, "--output", filename);
		if (force) args.push("--force");
		await runCommand(binary, workspace.uri.fsPath, args);
		this.post({ type: "notice", text: `Exported ${kind || "latest result"} to ${filename}` });
	}

	async openSettings(): Promise<void> {
		await vscode.commands.executeCommand("workbench.action.openSettings", "@ext:sacca97.ghg-vscode");
	}

	showSettings(): void {
		this.post({ type: "showSettings" });
	}

	async openAuth(): Promise<void> {
		const cfg = this.extensionSettings();
		const provider = await vscode.window.showInputBox({
			prompt: "Provider id",
			placeHolder: "openai, anthropic, or another configured profile",
			ignoreFocusOut: true,
		});
		if (!provider?.trim()) return;
		const terminal = vscode.window.createTerminal({
			name: "ghg auth",
			cwd: currentWorkspace()?.uri.fsPath,
			shellPath: cfg.binary,
			shellArgs: ["auth", provider.trim()],
		});
		terminal.show();
	}

	async configureSearchProvider(): Promise<void> {
		const action = await vscode.window.showQuickPick(
			[
				{ label: "Add or update SearXNG endpoint", value: "add" },
				{ label: "Select active provider", value: "use" },
				{ label: "Remove SearXNG endpoint", value: "remove" },
			],
			{ placeHolder: "Configure web search" },
		);
		if (!action) return;
		if (action.value === "use") {
			const name = await vscode.window.showInputBox({
				prompt: "Provider name, or brave to use BRAVE_SEARCH_API_KEY",
				ignoreFocusOut: true,
			});
			if (name?.trim()) await this.bridgeCommand("search_provider", { action: "use", name: name.trim() });
			return;
		}
		if (action.value === "remove") {
			const name = await vscode.window.showInputBox({ prompt: "SearXNG provider name to remove", ignoreFocusOut: true });
			if (name?.trim()) await this.bridgeCommand("search_provider", { action: "remove", name: name.trim() });
			return;
		}
		const name = await vscode.window.showInputBox({
			prompt: "SearXNG provider name",
			placeHolder: "personal",
			ignoreFocusOut: true,
		});
		if (!name?.trim()) return;
		const baseURL = await vscode.window.showInputBox({
			prompt: "SearXNG base URL",
			placeHolder: "https://search.example.com",
			ignoreFocusOut: true,
		});
		if (!baseURL?.trim()) return;
		const apiKey = await vscode.window.showInputBox({
			prompt: "SearXNG API key (optional)",
			password: true,
			ignoreFocusOut: true,
		});
		await this.bridgeCommand("search_provider", {
			action: "add", name: name.trim(), base_url: baseURL.trim(), api_key: apiKey?.trim() || "",
		});
		this.post({ type: "notice", text: `SearXNG provider ${name.trim()} saved and selected` });
	}

	private async setExecutionSetting(name: string, value: string): Promise<void> {
		if (!executionSettings.has(name)) throw new Error("unknown execution setting");
		const allowed: Record<string, Set<string>> = {
			sandbox: new Set<string>(sandboxes),
			network: new Set<string>(networks),
			approval: new Set<string>(approvals),
		};
		if (!allowed[name].has(value)) throw new Error(`invalid ${name} setting`);
		await vscode.workspace.getConfiguration("ghg").update(name, value || undefined, vscode.ConfigurationTarget.Global);
		this.post({ type: "extensionSettings", settings: this.extensionSettings() });
		if (name === "approval" && value && this.bridge) {
			await this.bridgeCommand("configure", { approval: value });
		}
	}

	private async setDefaultSetting(name: string, value: string): Promise<void> {
		if (!defaultSettings.has(name)) throw new Error("unknown default setting");
		const valid = name === "defaultRole" ? oneOf(roles, value) : name === "defaultMode" ? oneOf(modes, value) : normalizeEffort(value) !== undefined;
		if (!valid) throw new Error(`invalid ${name} setting`);
		const savedValue = name === "defaultEffort" ? normalizeEffort(value) : value;
		await vscode.workspace.getConfiguration("ghg").update(name, savedValue, vscode.ConfigurationTarget.Global);
		this.post({ type: "extensionSettings", settings: this.extensionSettings() });
	}

	private async submitInput(prompt: string, role: Role, mode: "execute" | "plan", flags: { ask?: boolean; review?: boolean }, references: string[]): Promise<void> {
		await this.bridgeCommand("configure", { role, mode }, role, mode);
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
		const role: Role = oneOf(roles, message.role) ? message.role : "fast";
		const references = cleanReferences(message.references);
		switch (name) {
		case "/ask":
			if (!args) throw new Error("usage: /ask <question>");
			this.setActive(true);
			this.post({ type: "turn_start", mode: "chat" });
			await this.submitInput(args, role, "execute", { ask: true }, references);
			return;
		case "/plan":
			await this.bridgeCommand("configure", { role, mode: "plan" }, role, "plan");
			if (!args) {
				this.post({ type: "notice", text: "switched to plan mode (read-only exploration)" });
				return;
			}
			this.setActive(true);
			this.post({ type: "turn_start", mode: "plan" });
			await this.bridgeCommand("input", { input: promptWithReferences(args, references), authored: true, plan_mode: true });
			return;
		case "/execute": {
			const plan = args || (typeof message.plan === "string" ? message.plan : "");
			if (!plan) throw new Error("no plan to execute — use /plan <goal> first");
			this.setActive(true);
			this.post({ type: "turn_start", mode: "chat" });
			await this.submitInput(`Execute the following approved plan. Create and maintain a todowrite checklist while implementing it.\n\n${plan}`, "fast", "execute", {}, references);
			return;
		}
		case "/review":
			if (!args) throw new Error("usage: /review <target or instructions>");
			this.setActive(true);
			this.post({ type: "turn_start", mode: "chat" });
			await this.submitInput(args, role, "execute", { review: true }, references);
			return;
		case "/export":
		case "/export-result":
		case "/export-chat":
		case "/export-log": {
			let kind = name === "/export-chat" || name === "/export-log" ? "chat" : "";
			let destination = "";
			let format = "";
			let force = false;
			for (let i = 1; i < fields.length; i++) {
				const field = fields[i];
				if (field === "--force" || field === "-f") {
					force = true;
					continue;
				}
				if (field === "--format" || field === "-format") {
					format = fields[++i] || "";
					continue;
				}
				if (field.startsWith("--format=")) {
					format = field.slice("--format=".length);
					continue;
				}
				if (field === "--output" || field === "-o") {
					destination = fields[++i] || "";
					continue;
				}
				const normalized = normalizeExportKind(field);
				if (normalized && !kind) kind = normalized;
				else if (!destination) destination = field;
				else throw new Error("usage: /export [chat|plan|review|last] [path] [--format json|markdown] [--force]");
			}
			return this.exportResult(kind, destination, format, force);
		}
		case "/continue": {
			if (this.active) throw new Error("A ghg turn is already running.");
			const continueMode = message.mode === "plan" ? "plan" : "execute";
			this.setActive(true);
			this.post({ type: "turn_start", mode: continueMode });
			await this.bridgeCommand("input", {
				input: promptWithReferences(args || "continue", cleanReferences(message.references)),
				authored: true,
				continue: true,
				plan_mode: continueMode === "plan",
				review_mode: message.mode === "review",
			});
			return;
		}
		case "/compact":
			if (args === "retry") return this.bridgeCommand("compact_retry");
			if (args === "log") throw new Error("/compact log is not available in the extension yet");
			if (args) throw new Error("usage: /compact [retry]");
			this.post({ type: "notice", text: "Compacting context…" });
			return this.bridgeCommand("compact");
		case "/approval":
			if (args !== "ask" && args !== "auto" && args !== "never") throw new Error("usage: /approval <ask|auto|never>");
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
		case "/search-providers": {
			const [action = "list", name, baseURL, apiKey] = fields.slice(1);
			if (action === "add") {
				if (!name || !baseURL) throw new Error("usage: /search-providers add <name> <base-url> [api-key]");
				return this.bridgeCommand("search_provider", { action, name, base_url: baseURL, api_key: apiKey || "" });
			}
			if (action === "use" || action === "remove") {
				if (!name) throw new Error(`usage: /search-providers ${action} <name|brave>`);
				return this.bridgeCommand("search_provider", { action, name });
			}
			if (action !== "list") throw new Error("usage: /search-providers [list|add|use|remove]");
			return this.bridgeCommand("search_provider", { action: "list" });
		}
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
			if (!args) throw new Error("usage: /effort <off|model-supported effort>");
			{
				const effort = normalizeEffort(args);
				if (effort === undefined) throw new Error("usage: /effort <off|model-supported effort>");
				const mode = message.mode === "plan" ? "plan" : "execute";
				return this.bridgeCommand("configure", { role, mode, effort, update_effort: true }, role, mode);
			}
		case "/dynamic-reasoning": {
			if (args !== "on" && args !== "off") throw new Error("usage: /dynamic-reasoning <on|off>");
			const mode = message.mode === "plan" ? "plan" : "execute";
			return this.bridgeCommand("configure", { role, mode, dynamic_reasoning: args === "on", persist_dynamic_reasoning: true }, role, mode);
		}
		case "/model": {
			if (args === "refresh") return this.refreshModelCatalogs();
			if (!args) return this.pickModel(message.mode === "plan" ? "plan" : "chat");
			const [model, provider] = fields.slice(1);
			const mode = message.mode === "plan" ? "plan" : "execute";
			await this.bridgeCommand("configure", { model, provider, role, mode, persist_role_model: true }, role, mode);
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
		case "/help": {
			const supported = new Set(extensionCommands);
			const entries = this.commands.filter((entry) => supported.has(entry.name));
			const lines = entries.length > 0
				? entries.sort((a, b) => (a.name < b.name ? -1 : 1)).map((entry) => `${entry.name} ${entry.hint}`)
				: extensionCommands;
			this.post({ type: "notice", text: lines.join("\n") });
			return;
		}
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
				this.post({ type: "busy", value: this.active });
				this.post({ type: "extensionSettings", settings: this.extensionSettings() });
				break;
			case "refreshModels":
				await this.refreshModels();
				break;
			case "exportChat": {
				await this.exportChat();
				break;
			}
			case "exportResult": {
				if (typeof message.kind !== "string" || !normalizeExportKind(message.kind)) break;
				try {
					await this.exportResult(normalizeExportKind(message.kind));
				} catch (error) {
					this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
				}
				break;
			}
			case "openSettings":
				await this.openSettings();
				break;
			case "openAuth":
				await this.openAuth();
				break;
			case "configureSearchProvider":
				await this.configureSearchProvider();
				break;
			case "configureModel": {
				if (this.active) break;
				const role = oneOf(roles, message.role) ? message.role : undefined;
				if (!role) break;
				try {
					await this.pickModel(message.mode === "plan" ? "plan" : "chat", role);
				} catch (error) {
					this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
				}
				break;
			}
			case "setExecutionSetting": {
				if (typeof message.name !== "string" || typeof message.value !== "string") break;
				try {
					await this.setExecutionSetting(message.name, message.value);
				} catch (error) {
					this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
				}
				break;
			}
			case "setDefaultSetting": {
				if (typeof message.name !== "string" || typeof message.value !== "string") break;
				try {
					await this.setDefaultSetting(message.name, message.value);
				} catch (error) {
					this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
				}
				break;
			}
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
				const role = oneOf(roles, message.role) ? message.role : "fast";
				const mode = message.mode === "plan" ? "plan" : "execute";
				try {
					const updateEffort = message.updateEffort === true || typeof message.effort === "string";
					const normalizedEffort = normalizeEffort(message.effort ?? "");
					if (updateEffort && normalizedEffort === undefined) {
						throw new Error("unsupported thinking effort for this model");
					}
					await this.bridgeCommand("configure", { role, mode, effort: normalizedEffort, update_effort: updateEffort }, role, mode);
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
				if (!(["http", "https"] as string[]).includes(uri.scheme)) {
					this.post({ type: "error", error: "Only http and https links can be opened." });
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
		const mode = message.mode === "plan" ? "plan" : message.mode === "review" ? "review" : message.mode === "ask" ? "ask" : message.mode === "execute" || message.mode === "chat" ? "execute" : undefined;
		if (!mode) {
			this.post({ type: "error", error: "Choose Execute, Plan, Review, or Ask before sending." });
			return;
		}
		const role: Role = oneOf(roles, message.role) ? message.role : "fast";
		const references = cleanReferences(message.references);
		if (activeBefore && !prompt.startsWith("/") && !prompt.startsWith("!")) {
			this.pendingSteers.push({ prompt, role, mode, references });
			if (!this.steerCancelRequested) {
				this.steerCancelRequested = true;
				try {
					await this.bridgeCommand("cancel");
				} catch (error) {
					this.pendingSteers.pop();
					this.steerCancelRequested = false;
					this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
				}
			}
			return;
		}
		if (activeBefore && !commandMayRunDuringTurn(prompt)) {
			this.post({ type: "error", error: "A ghg turn is already running." });
			return;
		}
		try {
			if (prompt.startsWith("/") || prompt.startsWith("!")) {
				await this.sendCommand(prompt, message);
				return;
			}
			this.setActive(true);
			this.post({ type: "turn_start", mode });
			if (mode === "review") {
				await this.submitInput(prompt, role, "execute", { review: true }, cleanReferences(message.references));
			} else if (mode === "ask") {
				await this.submitInput(prompt, role, "execute", { ask: true }, cleanReferences(message.references));
			} else {
				await this.submitInput(prompt, role, mode === "plan" ? "plan" : "execute", {}, cleanReferences(message.references));
			}
		} catch (error) {
			this.post({ type: "error", error: error instanceof Error ? error.message : String(error) });
			if (!activeBefore) {
				this.setActive(false);
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
			sessions = parseSessions(await runCommand(binary, workspace?.uri.fsPath, ["sessions", "--format", "json"]));
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
		vscode.window.registerWebviewViewProvider("ghg.chatView", provider, {
			webviewOptions: { retainContextWhenHidden: true },
		}),
		vscode.commands.registerCommand("ghg.newSession", () => provider.newSession()),
		vscode.commands.registerCommand("ghg.resumeSession", () => provider.resumeSession()),
		vscode.commands.registerCommand("ghg.exportChat", () => provider.exportChat()),
		vscode.commands.registerCommand("ghg.refreshModels", () => provider.refreshModels()),
		vscode.commands.registerCommand("ghg.openSettings", () => provider.showSettings()),
		vscode.workspace.onDidChangeConfiguration((event) => {
			if (event.affectsConfiguration("ghg")) {
				provider.post({ type: "extensionSettings", settings: provider.extensionSettings() });
				if (["binaryPath", "sandbox", "network", "approval"].some((name) => event.affectsConfiguration(`ghg.${name}`))) {
					void provider.reloadBridge();
				}
			}
		}),
		{ dispose: () => provider.dispose() },
	);
}

export function deactivate(): void {}
