BIN ?= ghg
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)

VSCODE_DIR ?= editors/vscode
VSIX_PATH ?= $(VSCODE_DIR)/ghg.vsix

.PHONY: all build build-vscode install install-vscode clean

all: build

build:
	@printf 'building %s\n' "$(BIN)"
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/ghg
	@chmod 755 $(BIN)

install:
	@set -eu; \
	need() { command -v "$$1" >/dev/null 2>&1 || { printf 'install: missing required command: %s\n' "$$1" >&2; exit 1; }; }; \
	need go; \
	if [ -n "$${GHG_BIN_PATH:-}" ]; then \
		dest="$$GHG_BIN_PATH"; \
	elif [ -n "$${BINDIR:-}" ]; then \
		dest="$$BINDIR/ghg"; \
	elif command -v ghg >/dev/null 2>&1 && [ -w "$$(command -v ghg)" ]; then \
		dest="$$(command -v ghg)"; \
	elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then \
		dest=/usr/local/bin/ghg; \
	elif [ -d /opt/homebrew/bin ] && [ -w /opt/homebrew/bin ]; then \
		dest=/opt/homebrew/bin/ghg; \
	else \
		dest="$$HOME/.local/bin/ghg"; \
	fi; \
	mkdir -p "$$(dirname "$$dest")"; \
	printf 'installing ghg -> %s\n' "$$dest"; \
	go build -trimpath -ldflags "$(LDFLAGS)" -o "$$dest" ./cmd/ghg; \
	chmod 755 "$$dest"; \
	printf 'installed ghg at %s\n' "$$dest"

build-vscode:
	@set -eu; \
	command -v npm >/dev/null 2>&1 || { printf 'build-vscode: missing required command: npm\n' >&2; exit 1; }; \
	printf 'installing extension dependencies\n'; \
	npm install --no-package-lock --ignore-scripts --prefix $(VSCODE_DIR); \
	printf 'compiling extension\n'; \
	npm run --prefix $(VSCODE_DIR) compile; \
	printf 'packaging extension -> %s\n' "$(VSIX_PATH)"; \
	cd $(VSCODE_DIR) && npx --yes @vscode/vsce package \
		--no-dependencies --allow-missing-repository --out ghg.vsix

install-vscode: install build-vscode
	@set -eu; \
	if [ -n "$${GHG_VSCODE_CLI:-}" ]; then \
		code_cli="$$GHG_VSCODE_CLI"; \
	elif command -v code >/dev/null 2>&1; then \
		code_cli="$$(command -v code)"; \
	elif command -v code-insiders >/dev/null 2>&1; then \
		code_cli="$$(command -v code-insiders)"; \
	else \
		printf 'install-vscode: neither code nor code-insiders is on PATH (set GHG_VSCODE_CLI)\n' >&2; \
		exit 1; \
	fi; \
	printf 'installing extension with %s\n' "$$code_cli"; \
	cli_stderr="$$(mktemp)"; \
	if "$$code_cli" --install-extension "$(CURDIR)/$(VSIX_PATH)" --force 2>"$$cli_stderr"; then \
		grep -v -E '^\(node:[0-9]+\) \[DEP0169\] DeprecationWarning:|^\(Use `Code --trace-deprecation' "$$cli_stderr" >&2 || true; \
		rm -f "$$cli_stderr"; \
	else \
		cat "$$cli_stderr" >&2; \
		rm -f "$$cli_stderr"; \
		exit 1; \
	fi; \
	printf 'installed ghg extension into %s\n' "$$code_cli"

clean:
	rm -f $(BIN) $(VSIX_PATH)
	rm -rf $(VSCODE_DIR)/out
