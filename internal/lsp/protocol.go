package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// PositionEncoding is the unit used for LSP position characters. Switchboard
// currently supports only the protocol default, UTF-16.
type PositionEncoding string

const PositionEncodingUTF16 PositionEncoding = "utf-16"

// SyncKind describes how a server wants open document changes delivered.
type SyncKind int

const (
	SyncNone SyncKind = iota
	SyncFull
	SyncIncremental
)

// SyncOptions is the normalized textDocumentSync capability.
type SyncOptions struct {
	OpenClose       bool
	Change          SyncKind
	Save            bool
	SaveIncludeText bool
}

// Capabilities is the portion of InitializeResult that Switchboard can use.
// Union-shaped wire capabilities are normalized to booleans here so callers
// never need to guess whether a bool or an options object advertised support.
type Capabilities struct {
	ServerName             string
	ServerVersion          string
	PositionEncoding       PositionEncoding
	Sync                   SyncOptions
	Definition             bool
	References             bool
	DocumentSymbols        bool
	WorkspaceSymbols       bool
	Hover                  bool
	WorkspaceSymbolResolve bool
	PullDiagnostics        bool
	WorkspaceDiagnostics   bool
}

// Position is a zero-based LSP wire position. Character is measured in the
// negotiated position encoding, which is UTF-16 for this client.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

func decodeInitializeResult(raw json.RawMessage) (Capabilities, error) {
	var out Capabilities
	out.PositionEncoding = PositionEncodingUTF16

	result, err := rawObject(raw, "initialize result")
	if err != nil {
		return Capabilities{}, err
	}
	capRaw, ok := result["capabilities"]
	if !ok || isNull(capRaw) {
		return Capabilities{}, fmt.Errorf("initialize result has no capabilities object")
	}
	wire, err := rawObject(capRaw, "initialize result capabilities")
	if err != nil {
		return Capabilities{}, err
	}

	if serverRaw, ok := result["serverInfo"]; ok && !isNull(serverRaw) {
		server, err := rawObject(serverRaw, "initialize result serverInfo")
		if err != nil {
			return Capabilities{}, err
		}
		nameRaw, ok := server["name"]
		if !ok || isNull(nameRaw) {
			return Capabilities{}, fmt.Errorf("initialize result serverInfo has no name")
		}
		if err := json.Unmarshal(nameRaw, &out.ServerName); err != nil || out.ServerName == "" {
			return Capabilities{}, fmt.Errorf("initialize result serverInfo name must be a non-empty string")
		}
		if versionRaw, ok := server["version"]; ok && !isNull(versionRaw) {
			if err := json.Unmarshal(versionRaw, &out.ServerVersion); err != nil {
				return Capabilities{}, fmt.Errorf("initialize result serverInfo version must be a string: %w", err)
			}
		}
	}

	if encodingRaw, ok := wire["positionEncoding"]; ok {
		if isNull(encodingRaw) {
			return Capabilities{}, fmt.Errorf("positionEncoding must be %q", PositionEncodingUTF16)
		}
		var encoding PositionEncoding
		if err := json.Unmarshal(encodingRaw, &encoding); err != nil {
			return Capabilities{}, fmt.Errorf("positionEncoding must be %q: %w", PositionEncodingUTF16, err)
		}
		if encoding != PositionEncodingUTF16 {
			return Capabilities{}, fmt.Errorf("unsupported positionEncoding %q; only %q is supported", encoding, PositionEncodingUTF16)
		}
		out.PositionEncoding = encoding
	}

	if syncRaw, ok := wire["textDocumentSync"]; ok {
		out.Sync, err = decodeSyncOptions(syncRaw)
		if err != nil {
			return Capabilities{}, fmt.Errorf("textDocumentSync: %w", err)
		}
	}

	if out.Definition, _, err = decodeProvider(wire["definitionProvider"], "definitionProvider"); err != nil {
		return Capabilities{}, err
	}
	if out.References, _, err = decodeProvider(wire["referencesProvider"], "referencesProvider"); err != nil {
		return Capabilities{}, err
	}
	if out.DocumentSymbols, _, err = decodeProvider(wire["documentSymbolProvider"], "documentSymbolProvider"); err != nil {
		return Capabilities{}, err
	}
	var workspaceSymbolOptions map[string]json.RawMessage
	if out.WorkspaceSymbols, workspaceSymbolOptions, err = decodeProvider(wire["workspaceSymbolProvider"], "workspaceSymbolProvider"); err != nil {
		return Capabilities{}, err
	}
	if out.WorkspaceSymbols && workspaceSymbolOptions != nil {
		out.WorkspaceSymbolResolve, err = optionalBool(workspaceSymbolOptions["resolveProvider"], "workspaceSymbolProvider.resolveProvider")
		if err != nil {
			return Capabilities{}, err
		}
	}
	if out.Hover, _, err = decodeProvider(wire["hoverProvider"], "hoverProvider"); err != nil {
		return Capabilities{}, err
	}
	var diagnosticOptions map[string]json.RawMessage
	if out.PullDiagnostics, diagnosticOptions, err = decodeProvider(wire["diagnosticProvider"], "diagnosticProvider"); err != nil {
		return Capabilities{}, err
	}
	if out.PullDiagnostics && diagnosticOptions != nil {
		out.WorkspaceDiagnostics, err = optionalBool(diagnosticOptions["workspaceDiagnostics"], "diagnosticProvider.workspaceDiagnostics")
		if err != nil {
			return Capabilities{}, err
		}
	}

	return out, nil
}

func decodeSyncOptions(raw json.RawMessage) (SyncOptions, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return SyncOptions{}, nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		wire, err := rawObject(raw, "textDocumentSync options")
		if err != nil {
			return SyncOptions{}, err
		}
		var out SyncOptions
		if out.OpenClose, err = optionalBool(wire["openClose"], "textDocumentSync.openClose"); err != nil {
			return SyncOptions{}, err
		}
		if changeRaw, ok := wire["change"]; ok && !isNull(changeRaw) {
			out.Change, err = decodeSyncKind(changeRaw)
			if err != nil {
				return SyncOptions{}, err
			}
		}
		if saveRaw, ok := wire["save"]; ok && !isNull(saveRaw) {
			switch string(bytes.TrimSpace(saveRaw)) {
			case "true":
				out.Save = true
			case "false":
			case "":
			default:
				saveOptions, err := rawObject(saveRaw, "textDocumentSync.save")
				if err != nil {
					return SyncOptions{}, fmt.Errorf("textDocumentSync.save must be a boolean or options object: %w", err)
				}
				out.Save = true
				if out.SaveIncludeText, err = optionalBool(saveOptions["includeText"], "textDocumentSync.save.includeText"); err != nil {
					return SyncOptions{}, err
				}
			}
		}
		if out.Change != SyncNone && !out.OpenClose {
			return SyncOptions{}, fmt.Errorf("change kind %d requires openClose support", out.Change)
		}
		return out, nil
	}

	kind, err := decodeSyncKind(raw)
	if err != nil {
		return SyncOptions{}, err
	}
	return SyncOptions{OpenClose: kind != SyncNone, Change: kind}, nil
}

func decodeSyncKind(raw json.RawMessage) (SyncKind, error) {
	var kind int
	if err := json.Unmarshal(raw, &kind); err != nil {
		return SyncNone, fmt.Errorf("change must be 0 (none), 1 (full), or 2 (incremental): %w", err)
	}
	if kind < int(SyncNone) || kind > int(SyncIncremental) {
		return SyncNone, fmt.Errorf("invalid change kind %d", kind)
	}
	return SyncKind(kind), nil
}

func decodeProvider(raw json.RawMessage, name string) (bool, map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return false, nil, nil
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil, nil
	case "false":
		return false, nil, nil
	}
	options, err := rawObject(raw, name)
	if err != nil {
		return false, nil, fmt.Errorf("%s must be a boolean or options object: %w", name, err)
	}
	return true, options, nil
}

func optionalBool(raw json.RawMessage, name string) (bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

func rawObject(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%s must be an object: %w", name, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return object, nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// fileURI encodes an absolute local path as a file URI without letting URI
// delimiters in a filename become a query, fragment, or authority.
func fileURI(path string) string {
	if host, share, ok := windowsUNC(path); ok {
		return (&url.URL{Scheme: "file", Host: host, Path: share}).String()
	}
	path = slashWindowsDrive(path)
	if isWindowsDrive(path) && !strings.HasPrefix(path, "/") {
		path = "/" + path
	} else {
		path = filepath.ToSlash(path)
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// filePath decodes a file URI into a local path. A remote authority is kept
// as a UNC path rather than silently discarded.
func filePath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("parse file URI: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "file") || u.Opaque != "" {
		return "", fmt.Errorf("%q is not a hierarchical file URI", uri)
	}
	if u.User != nil || u.Port() != "" {
		return "", fmt.Errorf("file URI must not contain user information or a port")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("file URI must not contain a query or fragment")
	}
	if strings.IndexByte(u.Path, 0) >= 0 {
		return "", fmt.Errorf("file URI path contains a NUL byte")
	}

	host := u.Hostname()
	if host != "" && !strings.EqualFold(host, "localhost") {
		path := "//" + host + "/" + strings.TrimPrefix(u.Path, "/")
		if runtime.GOOS == "windows" {
			return filepath.FromSlash(path), nil
		}
		return path, nil
	}

	path := u.Path
	if len(path) >= 3 && path[0] == '/' && isWindowsDrive(path[1:]) {
		path = path[1:]
		if runtime.GOOS != "windows" {
			return path, nil
		}
	}
	return filepath.FromSlash(path), nil
}

func windowsUNC(path string) (string, string, bool) {
	if !strings.HasPrefix(path, `\\`) {
		return "", "", false
	}
	normalized := strings.ReplaceAll(strings.TrimPrefix(path, `\\`), `\`, "/")
	host, rest, ok := strings.Cut(normalized, "/")
	if !ok || host == "" || rest == "" {
		return "", "", false
	}
	return host, "/" + rest, true
}

func slashWindowsDrive(path string) string {
	if isWindowsDrive(path) {
		return strings.ReplaceAll(path, `\`, "/")
	}
	return path
}

func isWindowsDrive(path string) bool {
	return len(path) >= 2 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':'
}

// symbolPosition finds the requested occurrence of symbol on a 1-based line.
// occurrence is itself 1-based; zero or a negative value selects the first.
func symbolPosition(text []byte, line int, symbol string, occurrence int) (Position, error) {
	if line < 1 {
		return Position{}, fmt.Errorf("line must be 1 or greater")
	}
	if symbol == "" {
		return Position{}, fmt.Errorf("symbol must not be empty")
	}
	if !utf8.Valid(text) {
		return Position{}, fmt.Errorf("document is not valid UTF-8")
	}
	if !utf8.ValidString(symbol) {
		return Position{}, fmt.Errorf("symbol is not valid UTF-8")
	}
	lineText, lineCount, ok := documentLine(text, line)
	if !ok {
		return Position{}, fmt.Errorf("document has %d lines; line %d is past the end", lineCount, line)
	}
	if occurrence <= 0 {
		occurrence = 1
	}

	symbolBytes := []byte(symbol)
	start := 0
	column := -1
	for n := 1; n <= occurrence; n++ {
		found := bytes.Index(lineText[start:], symbolBytes)
		if found < 0 {
			if n == 1 {
				return Position{}, fmt.Errorf("%q does not appear on line %d", symbol, line)
			}
			return Position{}, fmt.Errorf("%q has fewer than %d occurrences on line %d", symbol, occurrence, line)
		}
		column = start + found
		start = column + len(symbolBytes)
	}
	return Position{Line: line - 1, Character: utf16Units(lineText[:column])}, nil
}

func documentLine(text []byte, wanted int) ([]byte, int, bool) {
	start := 0
	line := 1
	for line < wanted {
		next := bytes.IndexByte(text[start:], '\n')
		if next < 0 {
			return nil, line, false
		}
		start += next + 1
		line++
	}
	end := len(text)
	if next := bytes.IndexByte(text[start:], '\n'); next >= 0 {
		end = start + next
	}
	if end > start && text[end-1] == '\r' {
		end--
	}
	return text[start:end], bytes.Count(text, []byte{'\n'}) + 1, true
}

// documentEnd returns the position immediately after the final character.
// LF and CRLF endings both put the end at character zero of the next line.
func documentEnd(text []byte) Position {
	line := bytes.Count(text, []byte{'\n'})
	start := bytes.LastIndexByte(text, '\n') + 1
	return Position{Line: line, Character: utf16Units(text[start:])}
}

func utf16Units(text []byte) int {
	units := 0
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
		text = text[size:]
	}
	return units
}
