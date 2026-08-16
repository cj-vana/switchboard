package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const minimal = "---\ndescription: does a thing\n---\n\nThe instructions.\n"

func TestLoadReadsBothShapes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	skillsDir := filepath.Join(ws, ".switchboard", "skills")
	writeSkill(t, skillsDir, "flat.md", minimal)
	writeSkill(t, filepath.Join(skillsDir, "packed"), "SKILL.md", minimal)

	list, notes := Load(ws)
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	if len(list) != 2 || list[0].Name != "flat" || list[1].Name != "packed" {
		t.Fatalf("loaded %+v, want flat and packed, sorted", list)
	}
	if list[1].Dir != filepath.Join(skillsDir, "packed") {
		t.Errorf("a packed skill's dir must be its own folder, got %s", list[1].Dir)
	}
	if list[0].Dir != skillsDir {
		t.Errorf("a flat skill's dir is the skills directory, got %s", list[0].Dir)
	}
}

func TestLoadProjectBeatsHomeOnANameClash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, ".switchboard", "skills"), "review.md",
		"---\ndescription: the project one\n---\nproject body")
	writeSkill(t, filepath.Join(home, ".switchboard", "skills"), "review.md",
		"---\ndescription: the home one\n---\nhome body")

	list, _ := Load(ws)
	if len(list) != 1 || list[0].Description != "the project one" || list[0].FromHome {
		t.Fatalf("project must win the clash: %+v", list)
	}
}

func TestLoadSkipsWhatItCannotUse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	dir := filepath.Join(ws, ".switchboard", "skills")
	writeSkill(t, dir, "nobody.md", "---\ndescription: fine\n---\n\n  \n")
	writeSkill(t, dir, "nodesc.md", "just a body")
	writeSkill(t, dir, "good.md", minimal)

	list, notes := Load(ws)
	if len(list) != 1 || list[0].Name != "good" {
		t.Fatalf("loaded %+v, want only the usable one", list)
	}
	if len(notes) != 2 {
		t.Fatalf("a skipped skill must say why, got %v", notes)
	}
}

func TestParseIgnoresForeignFrontmatterKeys(t *testing.T) {
	sk, err := parse("port", "---\nname: ported\ndescription: travels\nallowed-tools: Bash(git:*)\nmodel: something\n---\nbody")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "ported" || sk.Description != "travels" || sk.Body != "body" {
		t.Errorf("a pack written for a neighboring tool must load as written: %+v", sk)
	}
}

func TestToolDescriptionEnumeratesTheSet(t *testing.T) {
	tool := NewTool([]Skill{
		{Name: "a", Description: "first"},
		{Name: "b", Description: "second"},
	})
	desc := tool.Description()
	for _, want := range []string{"- a: first", "- b: second"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q:\n%s", want, desc)
		}
	}
}

func TestToolServesTheBodyWithItsDirectory(t *testing.T) {
	tool := NewTool([]Skill{{Name: "s", Description: "d", Body: "the body", Dir: "/somewhere/s"}})
	plan, err := tool.Plan(json.RawMessage(`{"name":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "the body") || !strings.Contains(res.Content, "/somewhere/s") {
		t.Errorf("serving must carry the body and where it lives:\n%s", res.Content)
	}
}

func TestToolRefusesAnUnknownName(t *testing.T) {
	tool := NewTool([]Skill{{Name: "s", Description: "d", Body: "b"}})
	if _, err := tool.Plan(json.RawMessage(`{"name":"ghost"}`)); err == nil {
		t.Fatal("an unknown skill must be a malformed call the model can correct")
	}
}

func TestToolServesSupportingFilesAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "style.md"), []byte("the reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewTool([]Skill{{Name: "s", Description: "d", Body: "b", Dir: dir}})
	serve := func(file string) string {
		t.Helper()
		in, _ := json.Marshal(skillInput{Name: "s", File: file})
		plan, err := tool.Plan(in)
		if err != nil {
			t.Fatal(err)
		}
		res, err := plan.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return res.Content
	}

	if got := serve("references/style.md"); got != "the reference" {
		t.Errorf("a named reference must be served as written, got %q", got)
	}
	if got := serve("../secret"); !strings.Contains(got, "leaves skill") && !strings.Contains(got, "does not exist") {
		t.Errorf("a traversal must be refused, got %q", got)
	}
	if got := serve(outside); !strings.Contains(got, "absolute") {
		t.Errorf("an absolute path must be refused, got %q", got)
	}
	if got := serve("references/missing.md"); !strings.Contains(got, "does not exist") {
		t.Errorf("a missing file must be named, got %q", got)
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if got := serve("link"); !strings.Contains(got, "leaves skill") {
			t.Errorf("a symlink out of the directory must be refused, got %q", got)
		}
	}
}
