package config

import (
	"strings"
	"testing"
)

func roleTestConfig() *Config {
	return &Config{
		Providers: map[string]Provider{
			"smart-provider": {BaseURL: "https://smart.example"},
			"fast-provider":  {BaseURL: "https://fast.example"},
		},
		Models: map[string]Model{
			"smart-model": {Providers: []string{"smart-provider"}},
			"fast-model":  {Providers: []string{"fast-provider"}},
		},
	}
}

func TestResolveRoleUsesConfiguredEntry(t *testing.T) {
	c := roleTestConfig()
	c.Roles = map[string]RoleConfig{
		RoleSmart: {Model: "smart-model", Provider: "smart-provider"},
		RoleFast:  {Model: "fast-model", Provider: "fast-provider"},
	}

	got, err := c.ResolveRole(RoleSmart)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "smart-model" || got.Provider != "smart-provider" {
		t.Fatalf("smart role = %+v", got)
	}
	got, err = c.ResolveRole(RoleFast)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "fast-model" || got.Provider != "fast-provider" {
		t.Fatalf("fast role = %+v", got)
	}
}

func TestResolveRoleFallsBackToConfiguredDefault(t *testing.T) {
	c := roleTestConfig()
	c.Roles = map[string]RoleConfig{
		RoleDefault: {Model: "smart-model", Provider: "smart-provider"},
	}
	for _, role := range []string{RoleSmart, RoleFast, RoleTiny} {
		got, err := c.ResolveRole(role)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if got.Model != "smart-model" || got.Provider != "smart-provider" {
			t.Fatalf("%s should fall back to default role, got %+v", role, got)
		}
	}

	c.Roles = nil
	got, err := c.ResolveRole(RoleSmart)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "" || got.Provider != "" {
		t.Fatalf("unconfigured default should remain empty: %+v", got)
	}
}

func TestResolveRoleDoesNotSilentlyFallbackInvalidConfiguredRole(t *testing.T) {
	c := roleTestConfig()
	c.Roles = map[string]RoleConfig{
		RoleFast: {Model: "does-not-exist"},
	}
	_, err := c.ResolveRole(RoleFast)
	if err == nil || !strings.Contains(err.Error(), `role "fast"`) || !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("invalid configured role error = %v", err)
	}

	c.Roles[RoleFast] = RoleConfig{}
	if _, err := c.ResolveRole(RoleFast); err == nil || !strings.Contains(err.Error(), "has no model") {
		t.Fatalf("empty configured role error = %v", err)
	}
}

func TestResolveRoleRejectsRemovedNames(t *testing.T) {
	c := roleTestConfig()
	for _, name := range []string{"plan", "execute", "smol", "vision", "unknown"} {
		if _, err := c.ResolveRole(name); err == nil {
			t.Fatalf("removed role %q was accepted", name)
		}
	}
	c.Roles = map[string]RoleConfig{"execute": {Model: "fast-model"}}
	if err := c.ValidateRoles(); err == nil {
		t.Fatal("unknown role key was accepted")
	}
}

func TestRoleForMode(t *testing.T) {
	if got := RoleForMode("plan"); got != RoleSmart {
		t.Fatalf("planning role = %q, want %q", got, RoleSmart)
	}
	if got := RoleForMode(ModeActing); got != RoleFast {
		t.Fatalf("acting role = %q, want %q", got, RoleFast)
	}
}
