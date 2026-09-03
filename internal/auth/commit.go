package auth

import (
	"fmt"

	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/models"
)

// CatalogError reports a cache failure after the provider credentials were
// committed successfully.
type CatalogError struct {
	Err error
}

func (e *CatalogError) Error() string { return fmt.Sprintf("save catalog: %v", e.Err) }
func (e *CatalogError) Unwrap() error { return e.Err }

// CommitCredential stores a resolved provider credential and its optional
// validated model catalog.
func CommitCredential(cfg *config.Config, name string, resolved models.Resolved, key string, envMode bool, infos []models.ModelInfo) error {
	if err := cfg.UpsertProviderKey(name, resolved, key, envMode); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if len(infos) == 0 {
		return nil
	}
	if err := config.SaveCatalog(name, resolved.BaseURL, infos); err != nil {
		return &CatalogError{Err: err}
	}
	return nil
}
