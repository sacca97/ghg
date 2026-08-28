package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/sacca97/ghg/internal/artifact"
	"github.com/sacca97/ghg/internal/config"
	"github.com/sacca97/ghg/internal/session"
)

// artifactsCLI owns maintenance of the payload directory. Session deletion
// removes database references first; this command then reclaims only payloads
// that no remaining session references, optionally applying age/size limits.
func artifactsCLI(args []string) error {
	if len(args) == 0 || args[0] != "gc" {
		return fmt.Errorf("usage: ghg artifacts gc [--max-age duration] [--max-bytes N]")
	}
	fs := flag.NewFlagSet("artifacts gc", flag.ContinueOnError)
	maxAge := fs.Duration("max-age", 0, "remove unreferenced payloads older than this duration")
	maxBytes := fs.Int64("max-bytes", 0, "remove oldest unreferenced payloads until the store is at most N bytes")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ghg artifacts gc [--max-age duration] [--max-bytes N]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *maxAge < 0 || *maxBytes < 0 {
		return fmt.Errorf("cleanup limits must be non-negative")
	}
	if *maxAge == 0 && *maxBytes == 0 {
		return fmt.Errorf("provide --max-age or --max-bytes")
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	st, err := session.Open(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	payloads, err := artifact.New(filepath.Join(dir, "artifacts"))
	if err != nil {
		return err
	}
	removed, err := st.GarbageCollectArtifacts(context.Background(), payloads, *maxAge, *maxBytes)
	if err != nil {
		return err
	}
	fmt.Printf("removed %d unreferenced artifact payload(s)\n", removed)
	return nil
}
