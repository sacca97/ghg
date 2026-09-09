#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
editor="$root/editors/vscode"

need() {
	command -v "$1" >/dev/null 2>&1 || {
		printf 'install-vscode: missing required command: %s\n' "$1" >&2
		exit 1
	}
}

need go
need npm

if [ -n "${GHG_BIN_PATH:-}" ]; then
	bin=$GHG_BIN_PATH
elif command -v ghg >/dev/null 2>&1; then
	bin=$(command -v ghg)
elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
	bin=/usr/local/bin/ghg
elif [ -d /opt/homebrew/bin ] && [ -w /opt/homebrew/bin ]; then
	bin=/opt/homebrew/bin/ghg
else
	bin="$HOME/.local/bin/ghg"
	mkdir -p "$(dirname "$bin")"
fi

printf 'building ghg -> %s\n' "$bin"
go build -o "$bin" "$root/cmd/ghg"
chmod 755 "$bin"

printf 'installing extension dependencies\n'
npm install --no-package-lock --ignore-scripts --prefix "$editor"
npm run --prefix "$editor" compile

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ghg-vscode.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
(cd "$editor" && npx --yes @vscode/vsce package \
	--no-dependencies --allow-missing-repository --out "$tmp/ghg.vsix")

if [ -n "${GHG_VSCODE_CLI:-}" ]; then
	code_cli=$GHG_VSCODE_CLI
elif command -v code >/dev/null 2>&1; then
	code_cli=$(command -v code)
elif command -v code-insiders >/dev/null 2>&1; then
	code_cli=$(command -v code-insiders)
else
	printf 'install-vscode: neither code nor code-insiders is on PATH\n' >&2
	exit 1
fi

printf 'installing extension with %s\n' "$code_cli"
cli_stderr="$tmp/code.stderr"
if "$code_cli" --install-extension "$tmp/ghg.vsix" --force 2>"$cli_stderr"; then
	grep -v -E '^\(node:[0-9]+\) \[DEP0169\] DeprecationWarning:|^\(Use `Code --trace-deprecation' "$cli_stderr" >&2 || true
else
	cat "$cli_stderr" >&2
	exit 1
fi
printf 'installed ghg extension and binary at %s\n' "$bin"
