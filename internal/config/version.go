package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigVersion is the current on-disk config schema.
const ConfigVersion = 2

func decodeConfig(data []byte) (*Config, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := parseJSONC(data, &header); err != nil {
		return nil, err
	}
	if header.Version > ConfigVersion {
		return nil, fmt.Errorf("config version %d is newer than supported version %d", header.Version, ConfigVersion)
	}
	if header.Version != ConfigVersion {
		return nil, fmt.Errorf("unsupported config version %d (want %d)", header.Version, ConfigVersion)
	}

	var cfg Config
	if err := parseJSONC(data, &cfg); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := parseJSONC(data, &raw); err != nil {
		return nil, err
	}
	if err := rejectLegacyConfig(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func rejectLegacyConfig(raw map[string]json.RawMessage, cfg *Config) error {
	for _, key := range []string{"defaultModel", "defaultProvider"} {
		if _, ok := raw[key]; ok {
			return fmt.Errorf("config version %d contains removed field %q", ConfigVersion, key)
		}
	}
	var models map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw["models"], &models); err != nil && len(raw["models"]) > 0 {
		return fmt.Errorf("models: %w", err)
	}
	for name, fields := range models {
		if _, ok := fields["maxTokens"]; ok {
			return fmt.Errorf("config version %d contains removed field models.%s.maxTokens", ConfigVersion, name)
		}
	}
	return rejectLegacyProtocols(cfg)
}

func rejectLegacyProtocols(cfg *Config) error {
	for name, provider := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(provider.API), "openai-completions") {
			return fmt.Errorf("provider %q uses removed protocol %q; use %q", name, provider.API, "openai-chat-completions")
		}
	}
	for name, model := range cfg.Models {
		if strings.EqualFold(strings.TrimSpace(model.API), "openai-completions") {
			return fmt.Errorf("model %q uses removed protocol %q; use %q", name, model.API, "openai-chat-completions")
		}
	}
	return nil
}
