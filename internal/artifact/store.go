// Package artifact stores bounded tool output outside the model context.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultMaxBytes is the maximum payload retained for one artifact.
	DefaultMaxBytes int64 = 10 << 20
	// DefaultReadLimit keeps one artifact_read response bounded.
	DefaultReadLimit int64 = 64 << 10
	// MaxReadLimit prevents a caller from turning artifact_read into an
	// unbounded file reader.
	MaxReadLimit int64 = 1 << 20
)

// Ref identifies an immutable retained tool result. Hash is the SHA-256 of
// the stored payload; the original size and Complete distinguish a complete
// result from a deterministic head/tail retention.
type Ref struct {
	ID            string            `json:"id"`
	Hash          string            `json:"hash"`
	OriginalBytes int64             `json:"original_bytes"`
	StoredBytes   int64             `json:"stored_bytes"`
	Complete      bool              `json:"complete"`
	MediaType     string            `json:"media_type,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Metadata is the session-owned index entry for one artifact reference. The
// payload itself remains owned by Store and is addressed only by Ref.Hash;
// session code never trusts Path when reading it.
type Metadata struct {
	Ref
	SessionID  string
	MessageSeq int
	ToolCallID string
	ToolName   string
	Path       string
	CreatedAt  time.Time
}

// Filter selects artifact metadata for one session. Empty fields do not
// filter. Query matches tool name, call id, and optional metadata values.
type Filter struct {
	ToolName   string
	ToolCallID string
	Query      string
	Since      time.Time
	Until      time.Time
}

// PutRequest describes one retained result. Data is the bytes available to
// retain; OriginalBytes may be larger when the producer already applied its
// own bounded head/tail capture.
type PutRequest struct {
	Data          []byte
	OriginalBytes int64
	Complete      bool
	MediaType     string
	Metadata      map[string]string
}

// Writer is the small dependency injected into the agent. Keeping the
// filesystem implementation behind this interface lets tests and no-session
// runs choose their own storage policy without global state.
type Writer interface {
	Put(context.Context, PutRequest) (Ref, error)
}

// Store writes content-addressed payloads below root. The zero value is not
// usable; construct it with New so the root is validated and created with
// private permissions.
type Store struct {
	root    string
	maxSize int64
	temp    bool
}

// New creates a persistent artifact store rooted at root. Existing roots are
// reused but must be directories; all created directories are private.
func New(root string) (*Store, error) {
	return NewWithLimit(root, DefaultMaxBytes)
}

// NewWithLimit creates a store with a caller-supplied per-artifact ceiling.
// A non-positive limit is rejected rather than silently disabling the bound.
func NewWithLimit(root string, maxBytes int64) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("artifact store root is required")
	}
	if maxBytes <= 0 {
		return nil, errors.New("artifact store max size must be positive")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact store root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact store root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat artifact store root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("artifact store root %q is a symlink", abs)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifact store root %q is not a directory", abs)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("protect artifact store root: %w", err)
	}
	return &Store{root: abs, maxSize: maxBytes}, nil
}

// NewTemp creates a private store for a no-session run. The caller owns the
// returned directory and should call Cleanup when the run exits.
func NewTemp() (*Store, error) {
	return NewTempWithLimit(DefaultMaxBytes)
}

// NewTempWithLimit is NewTemp with an explicit per-artifact ceiling. It is
// used by --no-session so one-off runs obey the same retention policy as
// persistent sessions.
func NewTempWithLimit(maxBytes int64) (*Store, error) {
	root, err := os.MkdirTemp("", "ghg-artifacts-")
	if err != nil {
		return nil, fmt.Errorf("create temporary artifact store: %w", err)
	}
	store, err := NewWithLimit(root, maxBytes)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	store.temp = true
	return store, nil
}

// Cleanup removes a temporary store created by NewTemp. Persistent stores
// intentionally have no delete-all operation.
func (s *Store) Cleanup() error {
	if s == nil || s.root == "" || !s.temp {
		return nil
	}
	return os.RemoveAll(s.root)
}

// Put stores one retained payload and returns its content-addressed reference.
// Duplicate payloads reuse the existing file. The input is copied into the
// file, so callers may reuse their buffer after Put returns.
func (s *Store) Put(ctx context.Context, req PutRequest) (Ref, error) {
	if s == nil || s.root == "" {
		return Ref{}, errors.New("artifact store is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	original := req.OriginalBytes
	if original <= 0 {
		original = int64(len(req.Data))
	}
	if original < int64(len(req.Data)) {
		original = int64(len(req.Data))
	}
	data := req.Data
	complete := req.Complete && original == int64(len(data))
	if int64(len(data)) > s.maxSize {
		data = retainHeadTail(data, s.maxSize)
		complete = false
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	ref := Ref{
		ID:            "sha256:" + hash,
		Hash:          hash,
		OriginalBytes: original,
		StoredBytes:   int64(len(data)),
		Complete:      complete,
		MediaType:     req.MediaType,
		Metadata:      cloneMetadata(req.Metadata),
	}
	shaRoot := filepath.Join(s.root, "sha256")
	if err := ensurePrivateDir(shaRoot); err != nil {
		return Ref{}, err
	}
	dir := filepath.Join(s.root, "sha256", hash[:2])
	if err := ensurePrivateDir(dir); err != nil {
		return Ref{}, err
	}
	path := filepath.Join(dir, hash)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Ref{}, fmt.Errorf("artifact payload %q is a symlink", hash)
		}
		if !info.Mode().IsRegular() {
			return Ref{}, fmt.Errorf("artifact payload %q is not a regular file", hash)
		}
		if info.Size() != int64(len(data)) {
			return Ref{}, fmt.Errorf("artifact payload %q has unexpected size", hash)
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Ref{}, fmt.Errorf("check artifact payload: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".artifact-")
	if err != nil {
		return Ref{}, fmt.Errorf("create artifact payload: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Ref{}, fmt.Errorf("protect artifact payload: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return Ref{}, fmt.Errorf("write artifact payload: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return Ref{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Ref{}, fmt.Errorf("sync artifact payload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Ref{}, fmt.Errorf("close artifact payload: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Size() == int64(len(data)) {
			return ref, nil
		}
		return Ref{}, fmt.Errorf("publish artifact payload: %w", err)
	}
	return ref, nil
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat artifact directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact directory %q is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("artifact directory %q is not a directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect artifact directory: %w", err)
	}
	return nil
}

// Read returns a bounded byte range from ref. The path is derived from the
// validated hash; callers never supply a filesystem path.
func (s *Store) Read(ctx context.Context, ref Ref, offset, limit int64) ([]byte, error) {
	if s == nil || s.root == "" {
		return nil, errors.New("artifact store is not initialized")
	}
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, errors.New("artifact offset must be non-negative")
	}
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	if limit > MaxReadLimit {
		return nil, fmt.Errorf("artifact read limit exceeds %d bytes", MaxReadLimit)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, "sha256", ref.Hash[:2], ref.Hash)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat artifact %s: %w", ref.ID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact %s is not a regular file", ref.ID)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open artifact %s: %w", ref.ID, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek artifact %s: %w", ref.ID, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, fmt.Errorf("read artifact %s: %w", ref.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

type gcCandidate struct {
	hash    string
	path    string
	size    int64
	modTime time.Time
}

// GarbageCollect removes only unreferenced payloads. maxAge removes
// candidates older than that duration; maxBytes then removes oldest remaining
// candidates until the whole store is at or below the requested size. A zero
// age or size disables that policy. Referenced hashes are never removed.
func (s *Store) GarbageCollect(ctx context.Context, referenced map[string]bool, maxAge time.Duration, maxBytes int64) (int, error) {
	if s == nil || s.root == "" {
		return 0, errors.New("artifact store is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	root := filepath.Join(s.root, "sha256")
	rootInfo, statErr := os.Lstat(root)
	if errors.Is(statErr, os.ErrNotExist) {
		return 0, nil
	}
	if statErr != nil {
		return 0, fmt.Errorf("stat artifact payload root: %w", statErr)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return 0, fmt.Errorf("artifact payload root is not a directory")
	}
	prefixes, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("list artifact payloads: %w", err)
	}
	const maxEntries = 100000
	var candidates []gcCandidate
	var total int64
	entries := 0
	for _, prefix := range prefixes {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		prefixPath := filepath.Join(root, prefix.Name())
		prefixInfo, statErr := os.Lstat(prefixPath)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("stat artifact prefix %q: %w", prefix.Name(), statErr)
		}
		if prefixInfo.Mode()&os.ModeSymlink != 0 || !prefixInfo.IsDir() || len(prefix.Name()) != 2 {
			continue
		}
		if _, err := hex.DecodeString(prefix.Name()); err != nil {
			continue
		}
		files, err := os.ReadDir(prefixPath)
		if err != nil {
			return 0, fmt.Errorf("list artifact prefix %q: %w", prefix.Name(), err)
		}
		for _, file := range files {
			entries++
			if entries > maxEntries {
				return 0, fmt.Errorf("artifact garbage-collection scan exceeds %d entries", maxEntries)
			}
			if file.IsDir() || len(file.Name()) != sha256.Size*2 {
				continue
			}
			if _, err := hex.DecodeString(file.Name()); err != nil || !strings.EqualFold(prefix.Name(), file.Name()[:2]) {
				continue
			}
			if referencedHash(referenced, file.Name()) {
				info, statErr := os.Lstat(filepath.Join(root, prefix.Name(), file.Name()))
				if statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
					total += info.Size()
				}
				continue
			}
			path := filepath.Join(root, prefix.Name(), file.Name())
			info, err := os.Lstat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return 0, fmt.Errorf("stat artifact payload: %w", err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				continue
			}
			total += info.Size()
			candidates = append(candidates, gcCandidate{hash: file.Name(), path: path, size: info.Size(), modTime: info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].modTime.Before(candidates[j].modTime)
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
		old := !cutoff.IsZero() && candidate.modTime.Before(cutoff)
		oversize := maxBytes > 0 && total > maxBytes
		if !old && !oversize {
			continue
		}
		if err := os.Remove(candidate.path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("remove artifact payload %s: %w", candidate.hash, err)
		}
		total -= candidate.size
		removed++
	}
	return removed, nil
}

func referencedHash(referenced map[string]bool, hash string) bool {
	return referenced[hash] || referenced["sha256:"+hash]
}

// RelativePath returns the stable path stored in session metadata. It is not
// accepted as read input; Read always reconstructs the path from Hash.
func RelativePath(ref Ref) (string, error) {
	if err := validateRef(ref); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("sha256", ref.Hash[:2], ref.Hash)), nil
}

func validateRef(ref Ref) error {
	hash := ref.Hash
	if hash == "" && strings.HasPrefix(ref.ID, "sha256:") {
		hash = strings.TrimPrefix(ref.ID, "sha256:")
	}
	if len(hash) != sha256.Size*2 {
		return errors.New("invalid artifact hash")
	}
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("invalid artifact hash")
	}
	if ref.ID != "" && ref.ID != "sha256:"+hash {
		return errors.New("artifact id does not match hash")
	}
	return nil
}

func retainHeadTail(data []byte, limit int64) []byte {
	if limit <= 0 || int64(len(data)) <= limit {
		return append([]byte(nil), data...)
	}
	head := int(limit / 2)
	tail := int(limit) - head
	out := make([]byte, 0, int(limit))
	out = append(out, data[:head]...)
	out = append(out, data[len(data)-tail:]...)
	return out
}
