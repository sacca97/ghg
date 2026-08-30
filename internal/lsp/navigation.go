package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sacca97/ghg/internal/sandbox"
	"github.com/sacca97/ghg/internal/tools"
)

const (
	maxDefinitions       = 20
	maxReferences        = 100
	maxSymbols           = 200
	maxHoverBytes        = 8 << 10
	maxPositionFileBytes = 16 << 20
)

type wirePosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type wireRange struct {
	Start wirePosition `json:"start"`
	End   wirePosition `json:"end"`
}

type wireLocation struct {
	URI   string    `json:"uri"`
	Range wireRange `json:"range"`
}

type wireLocationLink struct {
	TargetURI            string    `json:"targetUri"`
	TargetRange          wireRange `json:"targetRange"`
	TargetSelectionRange wireRange `json:"targetSelectionRange"`
}

type wireSymbolInformation struct {
	Name     string       `json:"name"`
	Kind     int          `json:"kind"`
	Location wireLocation `json:"location"`
}

type wireDocumentSymbol struct {
	Name           string               `json:"name"`
	Kind           int                  `json:"kind"`
	Range          wireRange            `json:"range"`
	SelectionRange wireRange            `json:"selectionRange"`
	Children       []wireDocumentSymbol `json:"children"`
}

type documentPosition struct {
	line  int
	start int
	end   int
}

// Navigate synchronizes the requested file, performs one read-only LSP
// request, and converts the response to bounded, policy-authorized values.
func (m *Manager) Navigate(ctx context.Context, request tools.NavigationRequest) (tools.NavigationResult, error) {
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	if operation != "definition" && operation != "references" && operation != "document_symbol" && operation != "hover" {
		return tools.NavigationResult{}, fmt.Errorf("unsupported lsp operation %q", request.Operation)
	}
	if strings.TrimSpace(request.Path) == "" {
		return tools.NavigationResult{}, errors.New("lsp path is required")
	}
	authorizedPath, err := m.authorizeReadPath(request.Path)
	if err != nil {
		return tools.NavigationResult{}, err
	}
	synced, err := m.syncDocument(ctx, authorizedPath)
	if err != nil {
		return tools.NavigationResult{}, err
	}
	if synced.client == nil {
		return tools.NavigationResult{}, fmt.Errorf("no language server covers %s", request.Path)
	}
	position, err := modelPositionToUTF16(synced.data, request.Line, request.Column)
	if err != nil && operation != "document_symbol" {
		return tools.NavigationResult{}, err
	}
	params := map[string]any{"textDocument": map[string]any{"uri": fileURI(synced.path)}}
	method := "textDocument/documentSymbol"
	switch operation {
	case "definition":
		method = "textDocument/definition"
		params["position"] = position
	case "references":
		method = "textDocument/references"
		params["position"] = position
		params["context"] = map[string]any{"includeDeclaration": request.IncludeDeclaration}
	case "hover":
		method = "textDocument/hover"
		params["position"] = position
	}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var raw json.RawMessage
	if err := synced.client.request(requestCtx, method, params, &raw); err != nil {
		return tools.NavigationResult{}, err
	}
	result := tools.NavigationResult{Operation: operation}
	switch operation {
	case "definition":
		locations, err := decodeDefinitionLocations(raw)
		if err != nil {
			return tools.NavigationResult{}, err
		}
		result.Locations, result.Omitted, err = m.normalizeLocations(locations, maxDefinitions)
		if err != nil {
			return tools.NavigationResult{}, err
		}
	case "references":
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil && string(raw) != "null" {
			return tools.NavigationResult{}, fmt.Errorf("decode references: %w", err)
		}
		locations := make([]wireLocation, 0, len(items))
		for _, item := range items {
			location, err := decodeWireLocation(item)
			if err != nil {
				return tools.NavigationResult{}, fmt.Errorf("decode reference: %w", err)
			}
			locations = append(locations, location)
		}
		result.Locations, result.Omitted, err = m.normalizeLocations(locations, maxReferences)
		if err != nil {
			return tools.NavigationResult{}, err
		}
	case "document_symbol":
		result.Symbols, result.Omitted, err = m.normalizeSymbols(raw, synced.path, synced.data, maxSymbols)
		if err != nil {
			return tools.NavigationResult{}, err
		}
	case "hover":
		result.Hover, result.HoverRange, err = normalizeHover(raw, synced.data)
		if err != nil {
			return tools.NavigationResult{}, err
		}
	}
	return result, nil
}

func (m *Manager) authorizeReadPath(path string) (string, error) {
	abs, err := documentPath(path)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	runtime := m.runtime
	m.mu.Unlock()
	if runtime != nil && runtime.Policy != nil {
		return runtime.Policy.Authorize(abs, sandbox.AccessRead, false)
	}
	return abs, nil
}

func decodeDefinitionLocations(raw json.RawMessage) ([]wireLocation, error) {
	raw = bytes.TrimSpace(raw)
	if string(raw) == "null" || len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("decode definition: %w", err)
		}
		locations := make([]wireLocation, 0, len(items))
		var links *bool
		for _, item := range items {
			var shape map[string]json.RawMessage
			if err := json.Unmarshal(item, &shape); err != nil {
				return nil, fmt.Errorf("decode definition item: %w", err)
			}
			_, isLink := shape["targetUri"]
			if links != nil && *links != isLink {
				return nil, errors.New("decode definition: mixed locations and location links")
			}
			links = &isLink
			if isLink {
				link, err := decodeWireLocationLink(item)
				if err != nil {
					return nil, err
				}
				locations = append(locations, link)
				continue
			}
			location, err := decodeWireLocation(item)
			if err != nil {
				return nil, err
			}
			locations = append(locations, location)
		}
		return locations, nil
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil, errors.New("decode definition: expected a location or location list")
	}
	if _, ok := shape["targetUri"]; ok {
		location, err := decodeWireLocationLink(raw)
		if err != nil {
			return nil, err
		}
		return []wireLocation{location}, nil
	}
	location, err := decodeWireLocation(raw)
	if err == nil {
		return []wireLocation{location}, nil
	}
	return nil, errors.New("decode definition: expected a location or location list")
}

func decodeWireLocation(raw json.RawMessage) (wireLocation, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		return wireLocation{}, fmt.Errorf("decode location: %w", err)
	}
	if _, ok := shape["uri"]; !ok {
		return wireLocation{}, errors.New("decode location: missing uri")
	}
	var location wireLocation
	if err := json.Unmarshal(raw, &location); err != nil || location.URI == "" {
		return wireLocation{}, errors.New("decode location: invalid uri")
	}
	rangeRaw, ok := shape["range"]
	if !ok {
		return wireLocation{}, errors.New("decode location: missing range")
	}
	rangeValue, err := decodeWireRange(rangeRaw)
	if err != nil {
		return wireLocation{}, fmt.Errorf("decode location range: %w", err)
	}
	location.Range = rangeValue
	return location, nil
}

func decodeWireLocationLink(raw json.RawMessage) (wireLocation, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		return wireLocation{}, fmt.Errorf("decode location link: %w", err)
	}
	if _, hasURI := shape["uri"]; hasURI {
		return wireLocation{}, errors.New("decode location link: contains a location uri")
	}
	var link wireLocationLink
	if err := json.Unmarshal(raw, &link); err != nil || link.TargetURI == "" {
		return wireLocation{}, errors.New("decode location link: invalid target uri")
	}
	targetRaw, ok := shape["targetRange"]
	if !ok {
		return wireLocation{}, errors.New("decode location link: missing target range")
	}
	targetRange, err := decodeWireRange(targetRaw)
	if err != nil {
		return wireLocation{}, fmt.Errorf("decode location link target range: %w", err)
	}
	rangeValue := targetRange
	if selectionRaw, ok := shape["targetSelectionRange"]; ok && string(bytes.TrimSpace(selectionRaw)) != "null" {
		rangeValue, err = decodeWireRange(selectionRaw)
		if err != nil {
			return wireLocation{}, fmt.Errorf("decode location link selection range: %w", err)
		}
	}
	return wireLocation{URI: link.TargetURI, Range: rangeValue}, nil
}

func decodeWireRange(raw json.RawMessage) (wireRange, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil || shape == nil {
		return wireRange{}, errors.New("range must be an object")
	}
	startRaw, ok := shape["start"]
	if !ok {
		return wireRange{}, errors.New("range is missing start")
	}
	endRaw, ok := shape["end"]
	if !ok {
		return wireRange{}, errors.New("range is missing end")
	}
	start, err := decodeWirePosition(startRaw)
	if err != nil {
		return wireRange{}, fmt.Errorf("range start: %w", err)
	}
	end, err := decodeWirePosition(endRaw)
	if err != nil {
		return wireRange{}, fmt.Errorf("range end: %w", err)
	}
	return wireRange{Start: start, End: end}, nil
}

func decodeWirePosition(raw json.RawMessage) (wirePosition, error) {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil || shape == nil {
		return wirePosition{}, errors.New("position must be an object")
	}
	lineRaw, ok := shape["line"]
	if !ok {
		return wirePosition{}, errors.New("position is missing line")
	}
	characterRaw, ok := shape["character"]
	if !ok {
		return wirePosition{}, errors.New("position is missing character")
	}
	var position wirePosition
	if err := json.Unmarshal(lineRaw, &position.Line); err != nil {
		return wirePosition{}, errors.New("position line is not an integer")
	}
	if err := json.Unmarshal(characterRaw, &position.Character); err != nil {
		return wirePosition{}, errors.New("position character is not an integer")
	}
	return position, nil
}

func (m *Manager) normalizeLocations(in []wireLocation, limit int) ([]tools.LSPLocation, int, error) {
	locations := make([]tools.LSPLocation, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, location := range in {
		path, data, err := m.authorizedLocation(location.URI)
		if err != nil {
			return nil, 0, err
		}
		rangeValue, err := normalizeRange(data, location.Range)
		if err != nil {
			return nil, 0, fmt.Errorf("normalize %s: %w", path, err)
		}
		item := tools.LSPLocation{Path: path, Range: rangeValue}
		key := locationKey(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		locations = append(locations, item)
	}
	sort.Slice(locations, func(i, j int) bool { return locationKey(locations[i]) < locationKey(locations[j]) })
	omitted := max(len(locations)-limit, 0)
	if len(locations) > limit {
		locations = locations[:limit]
	}
	return locations, omitted, nil
}

func (m *Manager) authorizedLocation(uri string) (string, []byte, error) {
	path := uriPath(uri)
	if path == "" {
		return "", nil, errors.New("language server returned a non-file URI")
	}
	canonical, err := canonicalDocumentPath(path)
	if err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	runtime := m.runtime
	m.mu.Unlock()
	if runtime != nil && runtime.Policy != nil {
		if _, err := runtime.Policy.Authorize(canonical, sandbox.AccessRead, false); err != nil {
			return "", nil, err
		}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxPositionFileBytes {
		return "", nil, errors.New("language server returned an unreadable or oversized file")
	}
	data, err := os.ReadFile(canonical)
	if err != nil {
		return "", nil, err
	}
	return canonical, data, nil
}

func normalizeRange(data []byte, value wireRange) (tools.LSPRange, error) {
	if !utf8.Valid(data) {
		return tools.LSPRange{}, errors.New("document is not valid UTF-8")
	}
	start, err := utf16PositionToModel(data, value.Start)
	if err != nil {
		return tools.LSPRange{}, err
	}
	end, err := utf16PositionToModel(data, value.End)
	if err != nil {
		return tools.LSPRange{}, err
	}
	if comparePosition(start, end) > 0 {
		return tools.LSPRange{}, errors.New("range ends before it starts")
	}
	return tools.LSPRange{Start: start, End: end}, nil
}

func comparePosition(a, b tools.LSPPosition) int {
	if a.Line != b.Line {
		if a.Line < b.Line {
			return -1
		}
		return 1
	}
	if a.Column < b.Column {
		return -1
	}
	if a.Column > b.Column {
		return 1
	}
	return 0
}

func locationKey(location tools.LSPLocation) string {
	return fmt.Sprintf("%s:%d:%d:%d:%d", location.Path, location.Range.Start.Line, location.Range.Start.Column, location.Range.End.Line, location.Range.End.Column)
}

func (m *Manager) normalizeSymbols(raw json.RawMessage, sourcePath string, sourceData []byte, limit int) ([]tools.LSPSymbol, int, error) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) || len(raw) == 0 {
		return nil, 0, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, 0, fmt.Errorf("decode document symbols: %w", err)
	}
	var symbols []tools.LSPSymbol
	for _, item := range items {
		var shape struct {
			Location json.RawMessage `json:"location"`
		}
		if err := json.Unmarshal(item, &shape); err != nil {
			return nil, 0, err
		}
		if len(shape.Location) > 0 && !bytes.Equal(bytes.TrimSpace(shape.Location), []byte("null")) {
			var old wireSymbolInformation
			if err := json.Unmarshal(item, &old); err != nil {
				return nil, 0, err
			}
			location, err := decodeWireLocation(shape.Location)
			if err != nil {
				return nil, 0, err
			}
			old.Location = location
			path, data, err := m.authorizedLocation(old.Location.URI)
			if err != nil {
				return nil, 0, err
			}
			rangeValue, err := normalizeRange(data, old.Location.Range)
			if err != nil {
				return nil, 0, err
			}
			symbols = append(symbols, tools.LSPSymbol{Name: old.Name, Kind: symbolKind(old.Kind), Path: path, Range: rangeValue, SelectionRange: rangeValue})
			continue
		}
		var document wireDocumentSymbol
		if err := json.Unmarshal(item, &document); err != nil {
			return nil, 0, err
		}
		if err := validateDocumentSymbol(item); err != nil {
			return nil, 0, err
		}
		if err := flattenDocumentSymbol(document, sourcePath, sourceData, &symbols); err != nil {
			return nil, 0, err
		}
	}
	sort.SliceStable(symbols, func(i, j int) bool {
		if symbols[i].Path != symbols[j].Path {
			return symbols[i].Path < symbols[j].Path
		}
		if comparePosition(symbols[i].Range.Start, symbols[j].Range.Start) != 0 {
			return comparePosition(symbols[i].Range.Start, symbols[j].Range.Start) < 0
		}
		if comparePosition(symbols[i].Range.End, symbols[j].Range.End) != 0 {
			return comparePosition(symbols[i].Range.End, symbols[j].Range.End) < 0
		}
		if comparePosition(symbols[i].SelectionRange.Start, symbols[j].SelectionRange.Start) != 0 {
			return comparePosition(symbols[i].SelectionRange.Start, symbols[j].SelectionRange.Start) < 0
		}
		if symbols[i].Name != symbols[j].Name {
			return symbols[i].Name < symbols[j].Name
		}
		return symbols[i].Kind < symbols[j].Kind
	})
	deduped := symbols[:0]
	seen := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		key := fmt.Sprintf("%s:%s:%d:%d:%d:%d", symbol.Path, symbol.Name, symbol.Range.Start.Line, symbol.Range.Start.Column, symbol.Range.End.Line, symbol.Range.End.Column)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, symbol)
	}
	omitted := max(len(deduped)-limit, 0)
	if len(deduped) > limit {
		deduped = deduped[:limit]
	}
	return deduped, omitted, nil
}

func validateDocumentSymbol(raw json.RawMessage) error {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil || shape == nil {
		return errors.New("document symbol must be an object")
	}
	for _, field := range []string{"range", "selectionRange"} {
		value, ok := shape[field]
		if !ok {
			return fmt.Errorf("document symbol is missing %s", field)
		}
		if _, err := decodeWireRange(value); err != nil {
			return fmt.Errorf("document symbol %s: %w", field, err)
		}
	}
	children, ok := shape["children"]
	if !ok || string(bytes.TrimSpace(children)) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(children, &items); err != nil {
		return fmt.Errorf("document symbol children: %w", err)
	}
	for _, item := range items {
		if err := validateDocumentSymbol(item); err != nil {
			return err
		}
	}
	return nil
}

func flattenDocumentSymbol(symbol wireDocumentSymbol, sourcePath string, sourceData []byte, out *[]tools.LSPSymbol) error {
	rangeValue, err := normalizeRange(sourceData, symbol.Range)
	if err != nil {
		return err
	}
	selection := rangeValue
	if symbol.SelectionRange != (wireRange{}) {
		selection, err = normalizeRange(sourceData, symbol.SelectionRange)
		if err != nil {
			return err
		}
	}
	*out = append(*out, tools.LSPSymbol{Name: symbol.Name, Kind: symbolKind(symbol.Kind), Path: sourcePath, Range: rangeValue, SelectionRange: selection})
	for _, child := range symbol.Children {
		if err := flattenDocumentSymbol(child, sourcePath, sourceData, out); err != nil {
			return err
		}
	}
	return nil
}

func symbolKind(kind int) string {
	names := map[int]string{1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class", 6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum", 11: "interface", 12: "function", 13: "variable", 14: "constant", 15: "string", 16: "number", 17: "boolean", 18: "array", 19: "object", 20: "key", 21: "null", 22: "enum_member", 23: "struct", 24: "event", 25: "operator", 26: "type_parameter"}
	return names[kind]
}

func normalizeHover(raw json.RawMessage, data []byte) (string, *tools.LSPRange, error) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) || len(raw) == 0 {
		return "", nil, nil
	}
	var hover struct {
		Contents json.RawMessage `json:"contents"`
		Range    *wireRange      `json:"range"`
	}
	if err := json.Unmarshal(raw, &hover); err != nil {
		return "", nil, fmt.Errorf("decode hover: %w", err)
	}
	var hoverShape struct {
		Range json.RawMessage `json:"range"`
	}
	if err := json.Unmarshal(raw, &hoverShape); err != nil {
		return "", nil, fmt.Errorf("decode hover: %w", err)
	}
	if len(hoverShape.Range) > 0 && string(bytes.TrimSpace(hoverShape.Range)) != "null" {
		if _, err := decodeWireRange(hoverShape.Range); err != nil {
			return "", nil, fmt.Errorf("decode hover range: %w", err)
		}
	}
	text := boundedString(renderMarkedString(hover.Contents), maxHoverBytes)
	var rangeValue *tools.LSPRange
	if hover.Range != nil {
		normalized, err := normalizeRange(data, *hover.Range)
		if err != nil {
			return "", nil, err
		}
		rangeValue = &normalized
	}
	return text, rangeValue, nil
}

func renderMarkedString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object struct {
		Language string `json:"language"`
		Value    string `json:"value"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Value != "" {
		if object.Language != "" {
			return "```" + object.Language + "\n" + object.Value + "\n```"
		}
		return object.Value
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		parts := make([]string, 0, len(list))
		for _, item := range list {
			if value := renderMarkedString(item); value != "" {
				parts = append(parts, value)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func boundedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + fmt.Sprintf("\n... [truncated %d bytes]", len(value)-cut)
}

func modelPositionToUTF16(data []byte, line, column int) (wirePosition, error) {
	if !utf8.Valid(data) {
		return wirePosition{}, errors.New("document is not valid UTF-8")
	}
	if line <= 0 || column <= 0 {
		return wirePosition{}, errors.New("position must use one-based line and Unicode-rune column")
	}
	spans := documentLines(data)
	if line > len(spans) {
		return wirePosition{}, errors.New("position line is outside the document")
	}
	span := spans[line-1]
	content := data[span.start:span.end]
	runes := []rune(string(content))
	if column > len(runes)+1 {
		return wirePosition{}, errors.New("position column is outside the document")
	}
	prefix := string(runes[:column-1])
	return wirePosition{Line: line - 1, Character: len(utf16.Encode([]rune(prefix)))}, nil
}

func utf16PositionToModel(data []byte, position wirePosition) (tools.LSPPosition, error) {
	if !utf8.Valid(data) {
		return tools.LSPPosition{}, errors.New("document is not valid UTF-8")
	}
	if position.Line < 0 || position.Character < 0 {
		return tools.LSPPosition{}, errors.New("language server returned a negative position")
	}
	spans := documentLines(data)
	if position.Line >= len(spans) {
		return tools.LSPPosition{}, errors.New("language server returned a position outside the document")
	}
	runes := []rune(string(data[spans[position.Line].start:spans[position.Line].end]))
	want := position.Character
	count := 0
	for i, r := range runes {
		if count == want {
			return tools.LSPPosition{Line: position.Line + 1, Column: i + 1}, nil
		}
		width := len(utf16.Encode([]rune{r}))
		if want < count+width {
			return tools.LSPPosition{}, errors.New("language server split a UTF-16 surrogate pair")
		}
		count += width
	}
	if count == want {
		return tools.LSPPosition{Line: position.Line + 1, Column: len(runes) + 1}, nil
	}
	return tools.LSPPosition{}, errors.New("language server returned a character outside the line")
}

func documentLines(data []byte) []documentPosition {
	lines := []documentPosition{{line: 0, start: 0}}
	for i, b := range data {
		if b != '\n' {
			continue
		}
		end := i
		if end > lines[len(lines)-1].start && data[end-1] == '\r' {
			end--
		}
		lines[len(lines)-1].end = end
		lines = append(lines, documentPosition{line: len(lines), start: i + 1})
	}
	last := &lines[len(lines)-1]
	last.end = len(data)
	if last.end > last.start && data[last.end-1] == '\r' {
		last.end--
	}
	return lines
}
