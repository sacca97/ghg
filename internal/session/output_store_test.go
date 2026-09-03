package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sacca97/ghg/internal/models"
)

func TestOutputPutReadAndDeduplicate(t *testing.T) {
	root := t.TempDir()
	store, err := NewOutputStore(root)
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("tool output")
	ref, err := store.Put(context.Background(), data, 0, true, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID == "" || ref.Hash == "" || !ref.Complete || ref.OriginalBytes != int64(len(data)) || ref.StoredBytes != int64(len(data)) {
		t.Fatalf("reference = %+v", ref)
	}
	rel, err := RelativePath(ref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, rel)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("payload mode = %o, want 600", info.Mode().Perm())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("payload = %q, err=%v", got, err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("payload directory mode = %o, want 700", dirInfo.Mode().Perm())
	}

	ref2, err := store.Put(context.Background(), append([]byte(nil), data...), 0, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref2.ID != ref.ID || ref2.StoredBytes != ref.StoredBytes {
		t.Fatalf("duplicate reference = %+v, first=%+v", ref2, ref)
	}

	got, err := store.Read(context.Background(), ref, 5, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outp" {
		t.Fatalf("range = %q, want outp", got)
	}
}

func TestOutputStoreRejectsSymlinkRootsAndPayloads(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "outputs")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOutputStore(link); err == nil {
		t.Fatal("symlink store root should be rejected")
	}

	store, err := NewOutputStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), []byte("payload"), 0, true, "")
	if err != nil {
		t.Fatal(err)
	}
	path, err := RelativePath(ref)
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(store.root, path)
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), ref, 0, 10); err == nil {
		t.Fatal("symlink payload should be rejected")
	}
}

func TestOutputPutRetainsHeadAndTailAtLimit(t *testing.T) {
	store, err := NewOutputStoreWithLimit(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), []byte("0123456789ABCDEFGHIJ"), 20, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Complete || ref.OriginalBytes != 20 || ref.StoredBytes != 10 {
		t.Fatalf("reference = %+v", ref)
	}
	data, err := store.Read(context.Background(), ref, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "01234FGHIJ" {
		t.Fatalf("retained payload = %q", data)
	}
}

func TestOutputReadRejectsUntrustedReferencesAndBounds(t *testing.T) {
	store, err := NewOutputStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), []byte(strings.Repeat("x", 8)), 0, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), ref, -1, 1); err == nil {
		t.Fatal("negative offset should fail")
	}
	if _, err := store.Read(context.Background(), ref, 0, MaxReadLimit+1); err == nil {
		t.Fatal("oversized read should fail")
	}
	bad := ref
	bad.ID = "sha256:" + strings.Repeat("0", 64)
	if _, err := store.Read(context.Background(), bad, 0, 1); err == nil {
		t.Fatal("mismatched id should fail")
	}
	if _, err := RelativePath(models.OutputRef{Hash: "../../outside"}); err == nil {
		t.Fatal("invalid hash should fail")
	}
}

func TestOutputPutHonorsCancellation(t *testing.T) {
	store, err := NewOutputStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(ctx, []byte("output"), 0, true, ""); err == nil {
		t.Fatal("canceled put should fail")
	}
}

func TestOutputGarbageCollectOnlyRemovesUnreferencedPayloads(t *testing.T) {
	store, err := NewOutputStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keep, err := store.Put(context.Background(), []byte("keep"), 0, true, "")
	if err != nil {
		t.Fatal(err)
	}
	drop, err := store.Put(context.Background(), []byte("drop"), 0, true, "")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.GarbageCollect(context.Background(), map[string]bool{keep.Hash: true}, 0, keep.StoredBytes)
	if err != nil || removed != 1 {
		t.Fatalf("garbage collection: removed=%d err=%v", removed, err)
	}
	if _, err := store.Read(context.Background(), keep, 0, 10); err != nil {
		t.Fatalf("referenced payload was removed: %v", err)
	}
	if _, err := store.Read(context.Background(), drop, 0, 10); err == nil {
		t.Fatal("unreferenced payload should be removed")
	}
}

func TestOutputGarbageCollectByAge(t *testing.T) {
	root := t.TempDir()
	store, err := NewOutputStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), []byte("old"), 0, true, "")
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := RelativePath(ref)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, rel), old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := store.GarbageCollect(context.Background(), nil, time.Hour, 0)
	if err != nil || removed != 1 {
		t.Fatalf("age cleanup: removed=%d err=%v", removed, err)
	}
}
