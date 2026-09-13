// Single source for every enum the webview shares between the composer, the
// settings panel, and the event validators. index.html ships empty <select>
// elements and main.js fills them from these tables, so the value list, its
// order, and its display label cannot drift apart inside the webview.
// extension.ts and package.json mirror these lists in TypeScript/JSON.
export const ROLES = ["default", "smart", "fast", "tiny"];
export const MODES = ["execute", "plan", "review"];
export const COMPOSER_MODES = ["execute", "plan", "review", "ask"];
export const EFFORT_LEVELS = ["", "low", "medium", "high"];
export const APPROVALS = ["", "ask", "auto", "never"];
export const SANDBOXES = ["", "read-only", "workspace-write", "danger-full-access"];
export const NETWORKS = ["", "deny", "host"];

export const ROLE_LABELS = { default: "Default", smart: "Smart", fast: "Fast", tiny: "Tiny" };
export const MODE_LABELS = { execute: "Execute", plan: "Plan", review: "Review" };
export const COMPOSER_MODE_LABELS = { ...MODE_LABELS, ask: "Ask" };
export const EFFORT_LABELS = { "": "Off", low: "Low", medium: "Medium", high: "High" };
export const APPROVAL_LABELS = { "": "Configured default", ask: "Ask", "auto": "Auto", never: "Never" };
export const SANDBOX_LABELS = { "": "Configured default", "read-only": "Read only", "workspace-write": "Workspace write", "danger-full-access": "Full access" };
export const NETWORK_LABELS = { "": "Configured default", deny: "Denied", host: "Host" };

export const BUSY_STATES = new Set(["running", "waiting_approval", "waiting_question", "stopping"]);

export const isRole = (value) => ROLES.includes(value);
export const isMode = (value) => MODES.includes(value);
export const isComposerMode = (value) => COMPOSER_MODES.includes(value);
export const isEffort = (value) => EFFORT_LEVELS.includes(value);
export const isApproval = (value) => APPROVALS.includes(value);
export const isSandbox = (value) => SANDBOXES.includes(value);
export const isNetwork = (value) => NETWORKS.includes(value);

// fillSelect rebuilds a <select> from a value list. label is either a map from
// value to text or a function that derives it (the composer labels its role
// options with the configured model names).
export function fillSelect(select, values, label) {
  select.replaceChildren();
  for (const value of values) {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = typeof label === "function" ? label(value) : label[value] ?? value;
    select.append(option);
  }
}
