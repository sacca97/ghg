package main

import (
	"context"
	"path/filepath"

	"github.com/sacca97/ghg/internal/agent"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/lsp"
	"github.com/sacca97/ghg/internal/memory"
	"github.com/sacca97/ghg/internal/session"
	"github.com/sacca97/ghg/internal/skills"
	"github.com/sacca97/ghg/internal/tools"
)

func newConfiguredRuntime(cfg *config.Config, trusted bool) (*tools.ToolRuntime, *lsp.Manager, func(), error) {
	runtime, cleanup, err := tools.NewConfiguredRuntime(".", cfg.Execution, trusted, cfg.PostEdit)
	if err != nil {
		return nil, nil, nil, err
	}
	lspMgr := lsp.NewManager(lsp.FromConfigMap(cfg.LSPServers))
	lspMgr.SetRuntime(runtime)
	return runtime, lspMgr, cleanup, nil
}

func systemPromptAdditions(sessionID, mcpInstructions string) []string {
	return []string{
		skills.PromptBlock(skills.Scan(skills.DefaultDirs()...)),
		memory.PromptBlock(memory.Installation(), memory.Session(sessionID)),
		mcpInstructions,
	}
}

func outputStoreLimit(cfg *config.Config) (int64, bool) {
	outputConfig := cfg.Outputs
	if outputConfig != nil && outputConfig.Enabled != nil && !*outputConfig.Enabled {
		return 0, false
	}
	maxBytes := int64(session.DefaultMaxBytes)
	if outputConfig != nil && outputConfig.MaxBytes > 0 {
		maxBytes = outputConfig.MaxBytes
	}
	return maxBytes, true
}

func openOutputStore(root string, maxBytes int64) (*session.OutputStore, error) {
	return session.NewOutputStoreWithLimit(filepath.Join(root, "outputs"), maxBytes)
}

func bindAgentSubsystems(ctx context.Context, ag *agent.Agent, runtime *tools.ToolRuntime, outputs *session.OutputStore, store *session.Store, sessionID string, cfg *config.Config) error {
	ag.Runtime = runtime
	ag.Outputs = outputs
	ag.SubagentsDisabled = !config.SubagentsEnabled(cfg)
	if store != nil {
		store.Outputs = outputs
		ag.OutputCatalog = store
		ag.HistoryCatalog = store
		ag.SetObservationStore(store)
		ag.SetSearchStore(store)
	}
	ag.SetSessionID(sessionID)
	return ag.BindState(ctx)
}
