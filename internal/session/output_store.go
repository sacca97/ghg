package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sacca97/ghg/internal/models"
	"github.com/sacca97/ghg/internal/tempdir"
)

const (
	DefaultMaxBytes  int64 = 10 << 20
	DefaultReadLimit int64 = 64 << 10
	MaxReadLimit     int64 = 1 << 20
)

type OutputStore struct {
	root    string
	maxSize int64
	temp    bool
}

func NewOutputStore(root string, limits ...int64) (*OutputStore, error) {
	limit := DefaultMaxBytes
	if len(limits) > 0 {
		limit = limits[0]
	}
	return NewOutputStoreWithLimit(root, limit)
}

func NewOutputStoreWithLimit(root string, maxBytes int64) (*OutputStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("output store root is required")
	}
	if maxBytes <= 0 {
		return nil, errors.New("output store max size must be positive")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve output store root: %w", err)
	}
	if err := privateDir(abs); err != nil {
		return nil, fmt.Errorf("prepare output store root: %w", err)
	}
	return &OutputStore{root: abs, maxSize: maxBytes}, nil
}

func NewTempOutputStore(limits ...int64) (*OutputStore, error) {
	limit := DefaultMaxBytes
	if len(limits) > 0 {
		limit = limits[0]
	}
	return NewTempOutputStoreWithLimit(limit)
}

func NewTempOutputStoreWithLimit(maxBytes int64) (*OutputStore, error) {
	root, err := os.MkdirTemp(tempdir.Base(), "ghg-outputs-")
	if err != nil {
		return nil, fmt.Errorf("create temporary output store: %w", err)
	}
	store, err := NewOutputStoreWithLimit(root, maxBytes)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	store.temp = true
	return store, nil
}

func (s *OutputStore) Cleanup() error {
	if s == nil || s.root == "" || !s.temp {
		return nil
	}
	return os.RemoveAll(s.root)
}

func (s *OutputStore) Put(ctx context.Context, data []byte, originalBytes int64, complete bool, mediaType string) (models.OutputRef, error) {
	if s == nil || s.root == "" {
		return models.OutputRef{}, errors.New("output store is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return models.OutputRef{}, err
	}
	if originalBytes < int64(len(data)) {
		originalBytes = int64(len(data))
	}
	if originalBytes <= 0 {
		originalBytes = int64(len(data))
	}
	if int64(len(data)) > s.maxSize {
		data = retainHeadTail(data, s.maxSize)
		complete = false
	}
	hash := sha256.Sum256(data)
	hexHash := hex.EncodeToString(hash[:])
	ref := models.OutputRef{ID: "sha256:" + hexHash, Hash: hexHash, OriginalBytes: originalBytes, StoredBytes: int64(len(data)), Complete: complete && originalBytes == int64(len(data)), MediaType: mediaType}
	dir := filepath.Join(s.root, "sha256", hexHash[:2])
	if err := privateDir(dir); err != nil {
		return models.OutputRef{}, err
	}
	path := filepath.Join(dir, hexHash)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
			return models.OutputRef{}, fmt.Errorf("output payload %q is invalid", hexHash)
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return models.OutputRef{}, fmt.Errorf("check output payload: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".output-")
	if err != nil {
		return models.OutputRef{}, fmt.Errorf("create output payload: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return models.OutputRef{}, fmt.Errorf("protect output payload: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return models.OutputRef{}, fmt.Errorf("write output payload: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return models.OutputRef{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return models.OutputRef{}, fmt.Errorf("sync output payload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return models.OutputRef{}, fmt.Errorf("close output payload: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Size() == int64(len(data)) {
			return ref, nil
		}
		return models.OutputRef{}, fmt.Errorf("publish output payload: %w", err)
	}
	return ref, nil
}

func (s *OutputStore) Read(ctx context.Context, ref models.OutputRef, offset, limit int64) ([]byte, error) {
	if s == nil || s.root == "" {
		return nil, errors.New("output store is not initialized")
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, errors.New("output offset must be non-negative")
	}
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	if limit > MaxReadLimit {
		return nil, fmt.Errorf("output read limit exceeds %d bytes", MaxReadLimit)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hash := outputHash(ref)
	path := filepath.Join(s.root, "sha256", hash[:2], hash)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat output %s: %w", ref.ID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("output %s is not a regular file", ref.ID)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open output %s: %w", ref.ID, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek output %s: %w", ref.ID, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, fmt.Errorf("read output %s: %w", ref.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

type outputCandidate struct {
	hash string
	path string
	size int64
	when time.Time
}

func (s *OutputStore) GarbageCollect(ctx context.Context, referenced map[string]bool, maxAge time.Duration, maxBytes int64) (int, error) {
	if s == nil || s.root == "" {
		return 0, errors.New("output store is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	root := filepath.Join(s.root, "sha256")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat output payload root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, errors.New("output payload root is not a directory")
	}
	var candidates []outputCandidate
	var total int64
	entries := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > 100000 {
			return errors.New("output garbage-collection scan exceeds 100000 entries")
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		hash, ok := outputHashAt(root, path)
		if !ok {
			return nil
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() {
			return nil
		}
		total += stat.Size()
		if !referencedHash(referenced, hash) {
			candidates = append(candidates, outputCandidate{hash: hash, path: path, size: stat.Size(), when: stat.ModTime()})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].when.Equal(candidates[j].when) {
			return candidates[i].when.Before(candidates[j].when)
		}
		return candidates[i].hash < candidates[j].hash
	})
	cutoff := time.Time{}
	if maxAge > 0 {
		cutoff = time.Now().Add(-maxAge)
	}
	removed := 0
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if (maxAge <= 0 || !candidate.when.Before(cutoff)) && (maxBytes <= 0 || total <= maxBytes) {
			continue
		}
		if err := os.Remove(candidate.path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		total -= candidate.size
		removed++
	}
	return removed, nil
}

func referencedHash(referenced map[string]bool, hash string) bool {
	return referenced[hash] || referenced["sha256:"+hash]
}

func outputHash(ref models.OutputRef) string {
	if ref.Hash != "" {
		return ref.Hash
	}
	return strings.TrimPrefix(ref.ID, "sha256:")
}

func outputHashAt(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != sha256.Size*2 || !strings.EqualFold(parts[0], parts[1][:2]) {
		return "", false
	}
	if _, err := hex.DecodeString(parts[0] + parts[1]); err != nil {
		return "", false
	}
	return parts[1], true
}

func RelativePath(ref models.OutputRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	hash := outputHash(ref)
	return filepath.ToSlash(filepath.Join("sha256", hash[:2], hash)), nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	return os.Chmod(path, 0o700)
}

func retainHeadTail(data []byte, limit int64) []byte {
	if limit <= 0 || int64(len(data)) <= limit {
		return append([]byte(nil), data...)
	}
	head := int(limit / 2)
	tail := int(limit) - head
	out := make([]byte, 0, int(limit))
	return append(append(out, data[:head]...), data[len(data)-tail:]...)
}
