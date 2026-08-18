package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndexRefreshFilterAndSearch(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cmd/server/main.go", "package main\n// Needle here\n")
	write("internal/parser/parse.go", "package parser\n// needle there\n")
	write("vendor/hidden.go", "needle hidden\n")
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex(w, 0)
	snapshot, err := index.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 2 {
		t.Fatalf("files = %+v", snapshot.Files)
	}
	matches := snapshot.Filter("psgo", 10)
	if len(matches) == 0 || matches[0].File.Path != "internal/parser/parse.go" {
		t.Fatalf("fuzzy matches = %+v", matches)
	}
	text, truncated, err := index.Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(text) != 2 {
		t.Fatalf("search = %+v truncated=%v", text, truncated)
	}
	if text[0].Location.Path != "cmd/server/main.go" || text[0].Location.Range.Start.Line != 2 {
		t.Fatalf("first match = %+v", text[0])
	}
}

func TestIndexInvalidateObservesExternalChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.go"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	index := NewIndex(w, 0)
	first, err := index.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.go"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cached, _ := index.Ensure(context.Background()); len(cached.Files) != 1 {
		t.Fatalf("unexpected implicit refresh: %+v", cached.Files)
	}
	index.Invalidate()
	second, err := index.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Files) != 2 || second.Generation <= first.Generation {
		t.Fatalf("refreshed snapshot = %+v (first %+v)", second, first)
	}
}

func TestSearchUnicodeCaseFoldAndRuneColumn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unicode.txt"), []byte("🙂 Éclair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	index := NewIndex(w, 0)
	matches, _, err := index.Search(context.Background(), "éCLAIR", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Location.Range.Start.Column != 3 {
		t.Fatalf("unicode matches = %+v", matches)
	}
	if err := w.Verify(matches[0].Location); err != nil {
		t.Fatalf("search returned unverifiable location: %v", err)
	}
}

func TestSearchHonorsCancelledContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	index := NewIndex(w, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := index.Search(ctx, "needle", SearchOptions{}); err != context.Canceled {
		t.Fatalf("cancelled search = %v", err)
	}
}

func BenchmarkFilterHundredThousandFiles(b *testing.B) {
	files := make([]File, 100_000)
	for i := range files {
		files[i] = indexedFile(filepath.ToSlash(filepath.Join("internal", "package", string(rune('a'+i%26)), "file.go")), 0)
	}
	snapshot := Snapshot{Files: files}
	b.ResetTimer()
	for range b.N {
		_ = snapshot.Filter("ipfg", 50)
	}
}
