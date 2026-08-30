package lsp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/tools"
)

const (
	maxRenameFiles        = 256
	maxRenameEdits        = 10000
	maxRenameBytes        = 32 << 20
	maxRenameIDPerSession = 32
)

type wireTextEdit struct {
	Range   wireRange `json:"range"`
	NewText string    `json:"newText"`
}

type wireRenameDocument struct {
	URI     string
	Version *int
	Edits   []wireTextEdit
}

type renameStoredFile struct {
	file      tools.RenameFile
	clientKey string
}

type renameEntry struct {
	id        string
	sessionID string
	files     []renameStoredFile
}

func (m *Manager) PreviewRename(ctx context.Context, request tools.RenameRequest) (tools.RenamePreview, error) {
	if m == nil {
		return tools.RenamePreview{}, errors.New("LSP manager is unavailable")
	}
	if strings.TrimSpace(request.Path) == "" || request.Line <= 0 || request.Column <= 0 {
		return tools.RenamePreview{}, errors.New("rename requires path, one-based line, and column")
	}
	if request.NewName == "" || strings.IndexByte(request.NewName, 0) >= 0 || len(request.NewName) > 1024 {
		return tools.RenamePreview{}, errors.New("rename new_name is empty, contains NUL, or is too long")
	}
	authorizedPath, err := m.authorizeReadPath(request.Path)
	if err != nil {
		return tools.RenamePreview{}, err
	}
	source, err := m.syncDocument(ctx, authorizedPath)
	if err != nil {
		return tools.RenamePreview{}, err
	}
	if source.client == nil {
		return tools.RenamePreview{}, fmt.Errorf("no language server covers %s", request.Path)
	}
	position, err := modelPositionToUTF16(source.data, request.Line, request.Column)
	if err != nil {
		return tools.RenamePreview{}, err
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": fileURI(source.path)},
		"position":     position,
		"newName":      request.NewName,
	}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var raw json.RawMessage
	if err := source.client.request(requestCtx, "textDocument/rename", params, &raw); err != nil {
		return tools.RenamePreview{}, err
	}
	documents, err := decodeWorkspaceEdit(raw)
	if err != nil {
		return tools.RenamePreview{}, err
	}
	if len(documents) == 0 {
		return tools.RenamePreview{}, errors.New("language server returned no rename edits")
	}
	workspace, err := m.renameWorkspace(source.path, source.root)
	if err != nil {
		return tools.RenamePreview{}, err
	}
	stored := make([]renameStoredFile, 0, len(documents))
	if len(documents) > maxRenameFiles {
		return tools.RenamePreview{}, fmt.Errorf("rename affects more than %d files", maxRenameFiles)
	}
	seenPaths := make(map[string]struct{}, len(documents))
	totalEdits := 0
	totalBytes := 0
	for _, document := range documents {
		uriPathValue := uriPath(document.URI)
		if uriPathValue == "" {
			return tools.RenamePreview{}, errors.New("rename target is not a file URI")
		}
		path, err := canonicalDocumentPath(uriPathValue)
		if err != nil {
			return tools.RenamePreview{}, fmt.Errorf("rename target: %w", err)
		}
		if !withinPath(path, workspace) {
			return tools.RenamePreview{}, fmt.Errorf("rename target %s is outside the workspace", path)
		}
		if _, ok := seenPaths[path]; ok {
			return tools.RenamePreview{}, fmt.Errorf("rename contains duplicate document %s", path)
		}
		seenPaths[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil {
			return tools.RenamePreview{}, err
		}
		if !info.Mode().IsRegular() {
			return tools.RenamePreview{}, fmt.Errorf("rename target %s is not a regular file", path)
		}
		if _, err := m.authorizeRenameRead(path); err != nil {
			return tools.RenamePreview{}, err
		}
		synced, err := m.syncDocument(ctx, path)
		if err != nil {
			return tools.RenamePreview{}, err
		}
		if document.Version != nil && synced.client == nil {
			return tools.RenamePreview{}, fmt.Errorf("rename target %s has a document version but no language server", path)
		}
		if document.Version != nil && *document.Version < 0 {
			return tools.RenamePreview{}, fmt.Errorf("rename target %s has an invalid document version", path)
		}
		if document.Version != nil && synced.version != *document.Version {
			return tools.RenamePreview{}, fmt.Errorf("rename target %s changed before preview", path)
		}
		updated, err := applyRenameTextEdits(synced.data, document.Edits)
		if err != nil {
			return tools.RenamePreview{}, fmt.Errorf("rename %s: %w", path, err)
		}
		totalEdits += len(document.Edits)
		if totalEdits > maxRenameEdits {
			return tools.RenamePreview{}, fmt.Errorf("rename contains more than %d edits", maxRenameEdits)
		}
		totalBytes += len(synced.data) + len(updated)
		if totalBytes > maxRenameBytes {
			return tools.RenamePreview{}, fmt.Errorf("rename exceeds the %d-byte preview budget", maxRenameBytes)
		}
		if !utf8.Valid(updated) {
			return tools.RenamePreview{}, errors.New("rename produced invalid UTF-8")
		}
		if bytesEqual(synced.data, updated) {
			continue
		}
		stored = append(stored, renameStoredFile{
			file:      tools.RenameFile{Path: path, Original: append([]byte(nil), synced.data...), Updated: append([]byte(nil), updated...), Mode: info.Mode(), Version: synced.version},
			clientKey: synced.key,
		})
	}
	if len(stored) == 0 {
		return tools.RenamePreview{}, errors.New("language server returned no changes")
	}
	sort.Slice(stored, func(i, j int) bool { return stored[i].file.Path < stored[j].file.Path })
	id, err := newRenameID()
	if err != nil {
		return tools.RenamePreview{}, err
	}
	entry := &renameEntry{id: id, sessionID: request.SessionID, files: cloneStoredFiles(stored)}
	if err := m.storeRename(entry); err != nil {
		return tools.RenamePreview{}, err
	}
	preview := tools.RenamePreview{ID: id, Files: make([]tools.RenameFilePreview, 0, len(stored))}
	for _, file := range stored {
		preview.Files = append(preview.Files, tools.RenameFilePreview{Path: file.file.Path, Diff: tools.EditDiff(string(file.file.Original), string(file.file.Updated))})
	}
	return preview, nil
}

func decodeWorkspaceEdit(raw json.RawMessage) ([]wireRenameDocument, error) {
	if string(raw) == "null" || len(raw) == 0 {
		return nil, errors.New("language server returned an empty workspace edit")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("decode workspace edit: %w", err)
	}
	changesRaw, hasChanges := object["changes"]
	documentChangesRaw, hasDocumentChanges := object["documentChanges"]
	if hasChanges && hasDocumentChanges {
		return nil, errors.New("workspace edit contains both changes and documentChanges")
	}
	if hasChanges {
		var changes map[string][]json.RawMessage
		if err := json.Unmarshal(changesRaw, &changes); err != nil {
			return nil, fmt.Errorf("decode workspace edit changes: %w", err)
		}
		out := make([]wireRenameDocument, 0, len(changes))
		keys := make([]string, 0, len(changes))
		for uri := range changes {
			keys = append(keys, uri)
		}
		sort.Strings(keys)
		for _, uri := range keys {
			edits, err := decodeTextEdits(changes[uri])
			if err != nil {
				return nil, fmt.Errorf("decode workspace edit changes for %s: %w", uri, err)
			}
			out = append(out, wireRenameDocument{URI: uri, Edits: edits})
		}
		return out, nil
	}
	if !hasDocumentChanges {
		return nil, errors.New("workspace edit has no supported changes")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(documentChangesRaw, &items); err != nil {
		return nil, errors.New("documentChanges must contain only TextDocumentEdit entries")
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]wireRenameDocument, 0, len(items))
	for _, item := range items {
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(item, &kind); err != nil {
			return nil, err
		}
		if kind.Kind != "" {
			return nil, fmt.Errorf("documentChanges operation %q is not supported", kind.Kind)
		}
		var document struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version *int   `json:"version"`
			} `json:"textDocument"`
			Edits []json.RawMessage `json:"edits"`
		}
		if err := json.Unmarshal(item, &document); err != nil || document.TextDocument.URI == "" {
			return nil, errors.New("documentChanges contains a non-TextDocumentEdit entry")
		}
		if _, ok := seen[document.TextDocument.URI]; ok {
			return nil, fmt.Errorf("documentChanges contains duplicate document %s", document.TextDocument.URI)
		}
		seen[document.TextDocument.URI] = struct{}{}
		edits := make([]wireTextEdit, 0, len(document.Edits))
		decodedEdits, err := decodeTextEdits(document.Edits)
		if err != nil {
			return nil, err
		}
		edits = append(edits, decodedEdits...)
		out = append(out, wireRenameDocument{URI: document.TextDocument.URI, Version: document.TextDocument.Version, Edits: edits})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

func decodeTextEdits(rawEdits []json.RawMessage) ([]wireTextEdit, error) {
	edits := make([]wireTextEdit, 0, len(rawEdits))
	for _, rawEdit := range rawEdits {
		var shape map[string]json.RawMessage
		if err := json.Unmarshal(rawEdit, &shape); err != nil {
			return nil, err
		}
		if _, annotated := shape["annotationId"]; annotated {
			return nil, errors.New("annotated text edits are not supported")
		}
		if err := requireRenameField(shape, "range"); err != nil {
			return nil, err
		}
		if err := requireRenameField(shape, "newText"); err != nil {
			return nil, err
		}
		var edit wireTextEdit
		if err := json.Unmarshal(rawEdit, &edit); err != nil {
			return nil, err
		}
		rangeValue, err := decodeWireRange(shape["range"])
		if err != nil {
			return nil, fmt.Errorf("text edit range: %w", err)
		}
		edit.Range = rangeValue
		edits = append(edits, edit)
	}
	return edits, nil
}

func requireRenameField(shape map[string]json.RawMessage, name string) error {
	raw, ok := shape[name]
	if !ok || string(raw) == "null" {
		return fmt.Errorf("text edit is missing %s", name)
	}
	return nil
}

func applyRenameTextEdits(original []byte, edits []wireTextEdit) ([]byte, error) {
	if !utf8.Valid(original) {
		return nil, errors.New("file is not valid UTF-8")
	}
	type byteEdit struct {
		start, end int
		text       string
	}
	converted := make([]byteEdit, 0, len(edits))
	for _, edit := range edits {
		start, end, err := utf16RangeToBytes(original, edit.Range)
		if err != nil {
			return nil, err
		}
		if !utf8.ValidString(edit.NewText) {
			return nil, errors.New("edit replacement is not valid UTF-8")
		}
		converted = append(converted, byteEdit{start: start, end: end, text: edit.NewText})
	}
	sort.Slice(converted, func(i, j int) bool {
		if converted[i].start != converted[j].start {
			return converted[i].start < converted[j].start
		}
		return converted[i].end < converted[j].end
	})
	for i := 1; i < len(converted); i++ {
		previous, current := converted[i-1], converted[i]
		if rangesOverlap(previous.start, previous.end, current.start, current.end) {
			return nil, errors.New("workspace edit contains overlapping ranges")
		}
	}
	updated := append([]byte(nil), original...)
	for i := len(converted) - 1; i >= 0; i-- {
		edit := converted[i]
		next := make([]byte, 0, len(updated)-edit.end+edit.start+len(edit.text))
		next = append(next, updated[:edit.start]...)
		next = append(next, edit.text...)
		next = append(next, updated[edit.end:]...)
		updated = next
	}
	return updated, nil
}

func utf16RangeToBytes(data []byte, value wireRange) (int, int, error) {
	start, err := utf16Offset(data, value.Start)
	if err != nil {
		return 0, 0, err
	}
	end, err := utf16Offset(data, value.End)
	if err != nil {
		return 0, 0, err
	}
	if end < start {
		return 0, 0, errors.New("range ends before it starts")
	}
	return start, end, nil
}

func utf16Offset(data []byte, position wirePosition) (int, error) {
	if position.Line < 0 || position.Character < 0 {
		return 0, errors.New("range contains a negative position")
	}
	lines := documentLines(data)
	if position.Line >= len(lines) {
		return 0, errors.New("range line is outside the document")
	}
	line := lines[position.Line]
	text := data[line.start:line.end]
	count := 0
	for byteOffset, r := range string(text) {
		if count == position.Character {
			return line.start + byteOffset, nil
		}
		width := 1
		if r > 0xffff {
			width = 2
		}
		if position.Character < count+width {
			return 0, errors.New("range splits a UTF-16 surrogate pair")
		}
		count += width
	}
	if count == position.Character {
		return line.end, nil
	}
	return 0, errors.New("range character is outside the line")
}

func rangesOverlap(aStart, aEnd, bStart, bEnd int) bool {
	if aStart == aEnd || bStart == bEnd {
		return aStart == bStart || (aStart == aEnd && aStart > bStart && aStart < bEnd) || (bStart == bEnd && bStart > aStart && bStart < aEnd)
	}
	return aStart < bEnd && bStart < aEnd
}

func (m *Manager) authorizeRenameRead(path string) (string, error) {
	m.mu.Lock()
	runtime := m.runtime
	m.mu.Unlock()
	if runtime != nil && runtime.Policy != nil {
		return runtime.Policy.Authorize(path, sandbox.AccessRead, false)
	}
	return path, nil
}

func (m *Manager) renameWorkspace(source, serverRoot string) (string, error) {
	m.mu.Lock()
	workspace := m.workspace
	runtime := m.runtime
	m.mu.Unlock()
	if runtime != nil && runtime.Policy != nil {
		workspace = runtime.Policy.Workspace()
	}
	if workspace == "" && serverRoot != "" {
		if canonical, err := canonicalDocumentPath(serverRoot); err == nil {
			workspace = canonical
		}
	}
	if workspace == "" {
		workspace = filepath.Dir(source)
	}
	return filepath.Clean(workspace), nil
}

func withinPath(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newRenameID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("rename id: %w", err)
	}
	return "rn_" + hex.EncodeToString(bytes[:]), nil
}

func cloneStoredFiles(in []renameStoredFile) []renameStoredFile {
	out := make([]renameStoredFile, len(in))
	for i, file := range in {
		out[i] = file
		out[i].file.Original = append([]byte(nil), file.file.Original...)
		out[i].file.Updated = append([]byte(nil), file.file.Updated...)
	}
	return out
}

func (m *Manager) storeRename(entry *renameEntry) error {
	// Keep Close and storeRename in the same lock order. A preview can finish
	// its server request while shutdown is already clearing the registry; do
	// not let that late request recreate a capability after Close returns.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return context.Canceled
	}
	m.renamesMu.Lock()
	defer m.renamesMu.Unlock()
	if m.renames == nil {
		m.renames = make(map[string]*renameEntry)
	}
	if m.renameOrder == nil {
		m.renameOrder = make(map[string][]string)
	}
	order := m.renameOrder[entry.sessionID]
	if len(order) >= maxRenameIDPerSession {
		old := order[0]
		delete(m.renames, entry.sessionID+"\x00"+old)
		order = order[1:]
	}
	order = append(order, entry.id)
	m.renameOrder[entry.sessionID] = order
	m.renames[entry.sessionID+"\x00"+entry.id] = entry
	return nil
}

func (m *Manager) LookupRename(_ context.Context, sessionID, id string) (tools.RenamePlan, error) {
	m.renamesMu.Lock()
	defer m.renamesMu.Unlock()
	entry, ok := m.renames[sessionID+"\x00"+id]
	if !ok || entry.sessionID != sessionID {
		return tools.RenamePlan{}, errors.New("rename preview not found for this session")
	}
	files := make([]tools.RenameFile, len(entry.files))
	for i, stored := range entry.files {
		files[i] = stored.file
		files[i].Original = append([]byte(nil), stored.file.Original...)
		files[i].Updated = append([]byte(nil), stored.file.Updated...)
	}
	return tools.RenamePlan{ID: entry.id, SessionID: entry.sessionID, Files: files}, nil
}

func (m *Manager) ValidateRename(_ context.Context, plan tools.RenamePlan) error {
	// An empty session is the deliberate scope for a --no-session process. It
	// is still keyed by this manager instance and cannot cross a process.
	if plan.ID == "" {
		return errors.New("rename preview has no id")
	}
	key := plan.SessionID + "\x00" + plan.ID
	m.renamesMu.Lock()
	entry, ok := m.renames[key]
	m.renamesMu.Unlock()
	if !ok {
		return errors.New("rename preview not found for this session")
	}
	if len(entry.files) != len(plan.Files) {
		return errors.New("rename preview changed")
	}
	for i, stored := range entry.files {
		if stored.file.Path != plan.Files[i].Path || !bytesEqual(stored.file.Original, plan.Files[i].Original) || !bytesEqual(stored.file.Updated, plan.Files[i].Updated) {
			return errors.New("rename preview changed")
		}
	}
	m.docMu.Lock()
	defer m.docMu.Unlock()
	for _, stored := range entry.files {
		current, err := os.ReadFile(stored.file.Path)
		if err != nil || !bytesEqual(current, stored.file.Original) {
			m.dropRename(key)
			return fmt.Errorf("rename preview is stale; preview again")
		}
		if stored.file.Version > 0 {
			m.mu.Lock()
			cs := m.clients[stored.clientKey]
			version := 0
			if cs != nil {
				version = cs.docs[stored.file.Path]
			}
			m.mu.Unlock()
			if version != stored.file.Version {
				m.dropRename(key)
				return errors.New("rename preview is stale; document version changed")
			}
		}
	}
	return nil
}

func (m *Manager) ConsumeRename(_ context.Context, sessionID, id string) error {
	key := sessionID + "\x00" + id
	m.renamesMu.Lock()
	defer m.renamesMu.Unlock()
	if _, ok := m.renames[key]; !ok {
		return errors.New("rename preview not found for this session")
	}
	delete(m.renames, key)
	order := m.renameOrder[sessionID]
	for i, value := range order {
		if value == id {
			m.renameOrder[sessionID] = append(order[:i], order[i+1:]...)
			break
		}
	}
	return nil
}

func (m *Manager) dropRename(key string) {
	m.renamesMu.Lock()
	defer m.renamesMu.Unlock()
	entry, ok := m.renames[key]
	if !ok {
		return
	}
	delete(m.renames, key)
	order := m.renameOrder[entry.sessionID]
	for i, value := range order {
		if value == entry.id {
			m.renameOrder[entry.sessionID] = append(order[:i], order[i+1:]...)
			break
		}
	}
}
