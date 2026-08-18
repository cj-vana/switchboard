package lsp

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDecodeInitializeResultNormalizesCapabilities(t *testing.T) {
	raw := json.RawMessage(`{
  "serverInfo": {"name": "testls", "version": "1.2.3"},
  "capabilities": {
    "positionEncoding": "utf-16",
    "textDocumentSync": {
      "openClose": true,
      "change": 2,
      "save": {"includeText": true}
    },
    "definitionProvider": true,
    "referencesProvider": {},
    "documentSymbolProvider": {"label": "outline"},
    "workspaceSymbolProvider": {"resolveProvider": true},
    "hoverProvider": {},
    "diagnosticProvider": {"workspaceDiagnostics": true}
  }
}`)

	got, err := decodeInitializeResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := Capabilities{
		ServerName:             "testls",
		ServerVersion:          "1.2.3",
		PositionEncoding:       PositionEncodingUTF16,
		Sync:                   SyncOptions{OpenClose: true, Change: SyncIncremental, Save: true, SaveIncludeText: true},
		Definition:             true,
		References:             true,
		DocumentSymbols:        true,
		WorkspaceSymbols:       true,
		Hover:                  true,
		WorkspaceSymbolResolve: true,
		PullDiagnostics:        true,
		WorkspaceDiagnostics:   true,
	}
	if got != want {
		t.Fatalf("capabilities = %#v, want %#v", got, want)
	}
}

func TestDecodeInitializeResultDefaultsAndUnsupportedProviders(t *testing.T) {
	got, err := decodeInitializeResult(json.RawMessage(`{
  "serverInfo": {"name": "quiet"},
  "capabilities": {
    "definitionProvider": false,
    "referencesProvider": null,
    "documentSymbolProvider": false,
    "workspaceSymbolProvider": null,
    "hoverProvider": false,
    "diagnosticProvider": null
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.PositionEncoding != PositionEncodingUTF16 {
		t.Fatalf("default position encoding = %q, want %q", got.PositionEncoding, PositionEncodingUTF16)
	}
	if got.Sync != (SyncOptions{}) {
		t.Fatalf("missing sync = %#v, want zero options", got.Sync)
	}
	if got.Definition || got.References || got.DocumentSymbols || got.WorkspaceSymbols || got.Hover || got.PullDiagnostics || got.WorkspaceSymbolResolve || got.WorkspaceDiagnostics {
		t.Fatalf("false, null, and missing providers must remain unsupported: %#v", got)
	}
}

func TestDecodeInitializeResultNumericSyncKinds(t *testing.T) {
	tests := []struct {
		name string
		kind int
		want SyncOptions
	}{
		{name: "none", kind: 0, want: SyncOptions{Change: SyncNone}},
		{name: "full", kind: 1, want: SyncOptions{OpenClose: true, Change: SyncFull}},
		{name: "incremental", kind: 2, want: SyncOptions{OpenClose: true, Change: SyncIncremental}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := json.RawMessage(`{"capabilities":{"textDocumentSync":` + string(rune('0'+tt.kind)) + `}}`)
			got, err := decodeInitializeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Sync != tt.want {
				t.Fatalf("sync = %#v, want %#v", got.Sync, tt.want)
			}
		})
	}
}

func TestDecodeInitializeResultSaveForms(t *testing.T) {
	tests := []struct {
		name string
		save string
		want SyncOptions
	}{
		{name: "false", save: `false`, want: SyncOptions{OpenClose: true, Change: SyncFull}},
		{name: "true", save: `true`, want: SyncOptions{OpenClose: true, Change: SyncFull, Save: true}},
		{name: "empty options", save: `{}`, want: SyncOptions{OpenClose: true, Change: SyncFull, Save: true}},
		{name: "include text", save: `{"includeText":true}`, want: SyncOptions{OpenClose: true, Change: SyncFull, Save: true, SaveIncludeText: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := json.RawMessage(`{"capabilities":{"textDocumentSync":{"openClose":true,"change":1,"save":` + tt.save + `}}}`)
			got, err := decodeInitializeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Sync != tt.want {
				t.Fatalf("sync = %#v, want %#v", got.Sync, tt.want)
			}
		})
	}
}

func TestDecodeInitializeResultRejectsMalformedOrUnsupportedValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		part string
	}{
		{name: "not object", raw: `[]`, part: "initialize result"},
		{name: "missing capabilities", raw: `{}`, part: "no capabilities"},
		{name: "bad server name", raw: `{"serverInfo":{"name":4},"capabilities":{}}`, part: "serverInfo name"},
		{name: "unsupported encoding", raw: `{"capabilities":{"positionEncoding":"utf-8"}}`, part: "unsupported positionEncoding"},
		{name: "null encoding", raw: `{"capabilities":{"positionEncoding":null}}`, part: "positionEncoding"},
		{name: "invalid sync kind", raw: `{"capabilities":{"textDocumentSync":3}}`, part: "invalid change kind"},
		{name: "fractional sync kind", raw: `{"capabilities":{"textDocumentSync":1.5}}`, part: "change must be"},
		{name: "change without open close", raw: `{"capabilities":{"textDocumentSync":{"change":1}}}`, part: "requires openClose"},
		{name: "bad save", raw: `{"capabilities":{"textDocumentSync":{"save":"yes"}}}`, part: "save must be"},
		{name: "bad provider", raw: `{"capabilities":{"definitionProvider":"yes"}}`, part: "definitionProvider"},
		{name: "bad resolve", raw: `{"capabilities":{"workspaceSymbolProvider":{"resolveProvider":{}}}}`, part: "resolveProvider"},
		{name: "bad workspace diagnostics", raw: `{"capabilities":{"diagnosticProvider":{"workspaceDiagnostics":"yes"}}}`, part: "workspaceDiagnostics"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeInitializeResult(json.RawMessage(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.part) {
				t.Fatalf("error = %v, want one containing %q", err, tt.part)
			}
		})
	}
}

func TestFileURIEscapesPathDelimitersAndRoundTrips(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "tmp", "space # 100%", "雪.go")
	uri := fileURI(path)
	for _, escaped := range []string{"%20", "%23", "%25", "%E9%9B%AA"} {
		if !strings.Contains(uri, escaped) {
			t.Errorf("fileURI(%q) = %q, want escape %q", path, uri, escaped)
		}
	}
	if strings.Contains(uri, "#") {
		t.Errorf("file URI contains an unescaped fragment delimiter: %q", uri)
	}
	got, err := filePath(uri)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("round trip = %q, want %q", got, path)
	}
}

func TestFileURIHandlesWindowsDriveAndUNCPaths(t *testing.T) {
	driveURI := fileURI(`C:\Users\A B\雪#100%.go`)
	if driveURI != "file:///C:/Users/A%20B/%E9%9B%AA%23100%25.go" {
		t.Fatalf("Windows drive URI = %q", driveURI)
	}
	drivePath, err := filePath(driveURI)
	if err != nil {
		t.Fatal(err)
	}
	wantDrive := "C:/Users/A B/雪#100%.go"
	if runtime.GOOS == "windows" {
		wantDrive = filepath.FromSlash(wantDrive)
	}
	if drivePath != wantDrive {
		t.Fatalf("Windows drive path = %q, want %q", drivePath, wantDrive)
	}

	uncURI := fileURI(`\\server\share\A B.go`)
	if uncURI != "file://server/share/A%20B.go" {
		t.Fatalf("UNC URI = %q", uncURI)
	}
	uncPath, err := filePath(uncURI)
	if err != nil {
		t.Fatal(err)
	}
	wantUNC := "//server/share/A B.go"
	if runtime.GOOS == "windows" {
		wantUNC = filepath.FromSlash(wantUNC)
	}
	if uncPath != wantUNC {
		t.Fatalf("UNC path = %q, want %q", uncPath, wantUNC)
	}
}

func TestFilePathAcceptsLocalhostAndRejectsNonFileParts(t *testing.T) {
	got, err := filePath("file://localhost/tmp/a%20b.go")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.FromSlash("/tmp/a b.go") {
		t.Fatalf("localhost path = %q", got)
	}

	for _, uri := range []string{
		"https://example.com/a.go",
		"file:///tmp/a.go?query=yes",
		"file:///tmp/a.go#fragment",
		"file://user@example.com/tmp/a.go",
		"file:///tmp/a%00b.go",
		"file:/%zz",
	} {
		if _, err := filePath(uri); err == nil {
			t.Errorf("filePath(%q) unexpectedly succeeded", uri)
		}
	}
}

func TestSymbolPositionUsesUTF16AndSelectsOccurrences(t *testing.T) {
	text := []byte("header\r\nalpha😀 β target target\r\nend\n")
	tests := []struct {
		name       string
		occurrence int
		want       Position
	}{
		{name: "default first", occurrence: 0, want: Position{Line: 1, Character: 10}},
		{name: "negative first", occurrence: -1, want: Position{Line: 1, Character: 10}},
		{name: "explicit first", occurrence: 1, want: Position{Line: 1, Character: 10}},
		{name: "explicit second", occurrence: 2, want: Position{Line: 1, Character: 17}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := symbolPosition(text, 2, "target", tt.occurrence)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("position = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSymbolPositionRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name       string
		text       []byte
		line       int
		symbol     string
		occurrence int
		part       string
	}{
		{name: "zero line", text: []byte("a"), line: 0, symbol: "a", occurrence: 1, part: "line must"},
		{name: "past end", text: []byte("a\n"), line: 3, symbol: "a", occurrence: 1, part: "past the end"},
		{name: "empty symbol", text: []byte("a"), line: 1, occurrence: 1, part: "must not be empty"},
		{name: "missing", text: []byte("a"), line: 1, symbol: "b", occurrence: 1, part: "does not appear"},
		{name: "nth missing", text: []byte("a a"), line: 1, symbol: "a", occurrence: 3, part: "fewer than 3"},
		{name: "invalid document utf8", text: []byte{0xff, 'a'}, line: 1, symbol: "a", occurrence: 1, part: "document is not valid UTF-8"},
		{name: "invalid symbol utf8", text: []byte("a"), line: 1, symbol: string([]byte{0xff}), occurrence: 1, part: "symbol is not valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := symbolPosition(tt.text, tt.line, tt.symbol, tt.occurrence)
			if err == nil || !strings.Contains(err.Error(), tt.part) {
				t.Fatalf("error = %v, want one containing %q", err, tt.part)
			}
		})
	}
}

func TestSymbolPositionAcceptsTrailingEmptyLine(t *testing.T) {
	got, err := symbolPosition([]byte("first\nsecond\n"), 2, "second", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Position{Line: 1, Character: 0}) {
		t.Fatalf("position = %#v", got)
	}
	if _, err := symbolPosition([]byte("first\nsecond\n"), 3, "anything", 1); err == nil || !strings.Contains(err.Error(), "does not appear") {
		t.Fatalf("the trailing empty third line must exist; got %v", err)
	}
}

func TestDocumentEndUsesUTF16ForLFAndCRLF(t *testing.T) {
	tests := []struct {
		text string
		want Position
	}{
		{text: "", want: Position{}},
		{text: "abc", want: Position{Character: 3}},
		{text: "a😀", want: Position{Character: 3}},
		{text: "a\n", want: Position{Line: 1}},
		{text: "a\r\n", want: Position{Line: 1}},
		{text: "a\r\n😀", want: Position{Line: 1, Character: 2}},
		{text: "one\ntwo\r\n三😀", want: Position{Line: 2, Character: 3}},
	}
	for _, tt := range tests {
		if got := documentEnd([]byte(tt.text)); got != tt.want {
			t.Errorf("documentEnd(%q) = %#v, want %#v", tt.text, got, tt.want)
		}
	}
}
