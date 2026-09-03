package models

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// OutputRef identifies immutable tool output stored outside the model context.
type OutputRef struct {
	ID            string            `json:"id"`
	Hash          string            `json:"hash"`
	OriginalBytes int64             `json:"original_bytes"`
	StoredBytes   int64             `json:"stored_bytes"`
	Complete      bool              `json:"complete"`
	MediaType     string            `json:"media_type,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

func (r OutputRef) Validate() error {
	hash := r.Hash
	if hash == "" && strings.HasPrefix(r.ID, "sha256:") {
		hash = strings.TrimPrefix(r.ID, "sha256:")
	}
	if len(hash) != sha256.Size*2 {
		return errors.New("invalid output hash")
	}
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("invalid output hash")
	}
	if r.ID != "" && r.ID != "sha256:"+hash {
		return errors.New("output id does not match hash")
	}
	return nil
}
