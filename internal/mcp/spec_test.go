package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSpecsParsesAndSorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), SpecFileName)
	const file = `
[mcp.zeta]
url = "https://example.invalid/mcp"

[mcp.alpha]
command = "alpha-server"
args = ["stdio", "--fast"]
allow = ["safe_tool"]

[mcp.alpha.env]
ALPHA_HOME = "/tmp/alpha"
`
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := LoadSpecs(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "alpha" || specs[1].Name != "zeta" {
		t.Fatalf("specs = %+v, want alpha then zeta", specs)
	}
	a := specs[0]
	if a.Command != "alpha-server" || len(a.Args) != 2 || a.Env["ALPHA_HOME"] != "/tmp/alpha" || len(a.Allow) != 1 {
		t.Errorf("alpha = %+v", a)
	}
	if specs[1].URL == "" {
		t.Errorf("zeta lost its url: %+v", specs[1])
	}
}

func TestLoadSpecsMissingFileIsEmpty(t *testing.T) {
	specs, err := LoadSpecs(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil || specs != nil {
		t.Fatalf("missing file: specs=%v err=%v, want nil and nil", specs, err)
	}
}

func TestLoadSpecsRejectsAmbiguousTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), SpecFileName)
	const file = `
[mcp.both]
command = "x"
url = "https://example.invalid"
`
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSpecs(path); err == nil {
		t.Fatal("a server with both command and url must be refused")
	}
}
