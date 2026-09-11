// Small reusable buttons that appear in more than one place: the copy button
// owns two headers (a fenced code block and an assistant response) and the
// export button is added to plan/review blocks.
const COPY_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true"><rect x="5" y="5" width="8" height="8" rx="1"/><path d="M3 10V3h7"/></svg>';
const EXPORT_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 2v7M5 6.5l3 3 3-3M3 11.5v2h10v-2"/></svg>';
const FEEDBACK_MS = 1200;

export function copyButton(label, read) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "copy";
  button.innerHTML = COPY_ICON;
  button.setAttribute("aria-label", label);
  button.title = label;
  button.addEventListener("click", () => {
    void navigator.clipboard?.writeText(read() || "");
    button.textContent = "✓";
    button.title = "Copied";
    setTimeout(() => {
      button.innerHTML = COPY_ICON;
      button.title = label;
    }, FEEDBACK_MS);
  });
  return button;
}

export function exportButton(kind, post) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "copy export";
  button.innerHTML = EXPORT_ICON;
  button.setAttribute("aria-label", `Export ${kind}`);
  button.title = `Export ${kind}`;
  button.addEventListener("click", () => post({ type: "exportResult", kind }));
  return button;
}
