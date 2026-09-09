# ghg for VS Code

This extension adds a GHG view in the VS Code Activity Bar, backed by a
persistent `ghg bridge` worker. It does not duplicate ghg's model, tool, or
sandbox configuration.

The view supports worker-backed streaming chat, plans, reviews, slash
commands, basic Markdown with fenced code, session resume, stopping a turn,
and adding workspace files or folders with `@`.

Install dependencies and compile during development:

```sh
npm install
npm run compile
```

Set `ghg.binaryPath` if `ghg` is not on the extension host's `PATH`. Use
`ghg: New Session` or `ghg: Resume Session` from the Command Palette to manage
the workspace's stored session.
