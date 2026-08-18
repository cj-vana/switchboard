package session

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestRuntimeBindingAndUsageTargetsSurviveReplay(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "scripted/local/start", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendRuntimeBinding("t2", "scripted/local/landed", true); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"scripted/local/a", "scripted/local/b", "scripted/local/a"} {
		if err := sess.AppendUsage(Usage{Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	id := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state := reopened.State()
	if state.RuntimeBinding != (RuntimeBinding{Tier: "t2", Target: "scripted/local/landed", Pinned: true}) {
		t.Fatalf("runtime binding = %+v", state.RuntimeBinding)
	}
	if got := strings.Join(state.UsageTargets, ","); got != "scripted/local/a,scripted/local/b" {
		t.Fatalf("usage targets = %q", got)
	}
}

func TestSchemaTwoUpgradesBeforeRuntimeBindingAppend(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "scripted/local/start", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v2 := strings.Replace(string(data), fmt.Sprintf("%s %d", magic, SchemaVersion), magic+" 2", 1)
	if err := os.WriteFile(path, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("opening schema 2 for migration: %v", err)
	}
	if err := reopened.AppendRuntimeBinding("t3", "scripted/local/new", true); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	header, err := bufio.NewReader(f).ReadString('\n')
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var gotMagic string
	var version int
	if _, err := fmt.Sscanf(strings.TrimSpace(header), "%s %d", &gotMagic, &version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion || version <= 2 {
		t.Fatalf("runtime binding log header = %q; a schema-2 reader would not refuse it", strings.TrimSpace(header))
	}

	again, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.State().RuntimeBinding; got.Tier != "t3" || got.Target != "scripted/local/new" || !got.Pinned {
		t.Fatalf("binding after v2 upgrade/reopen = %+v", got)
	}
}

func TestExplicitOntoForksDoNotInheritSourceRuntimeBinding(t *testing.T) {
	destination := provider.RouteTargetID("scripted/local/destination")

	t.Run("live", func(t *testing.T) {
		store, source := forkFixture(t)
		if err := source.AppendRuntimeBinding("t1", "scripted/local/source", true); err != nil {
			t.Fatal(err)
		}
		child, err := store.ForkSessionOnto(source, len(source.State().Messages), destination)
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close()
		if got := child.State(); got.Target != string(destination) || got.RuntimeBinding.Target != "" {
			t.Fatalf("retargeted child state = target %q binding %+v", got.Target, got.RuntimeBinding)
		}
	})

	t.Run("non-live", func(t *testing.T) {
		store, source := forkFixture(t)
		if err := source.AppendRuntimeBinding("t1", "scripted/local/source", true); err != nil {
			t.Fatal(err)
		}
		id, messages := source.ID(), len(source.State().Messages)
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		child, err := store.ForkOnto(id, messages, destination)
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close()
		if got := child.State(); got.Target != string(destination) || got.RuntimeBinding.Target != "" {
			t.Fatalf("retargeted child state = target %q binding %+v", got.Target, got.RuntimeBinding)
		}
	})

	t.Run("accounting-empty", func(t *testing.T) {
		store, source := forkFixture(t)
		if err := source.AppendRuntimeBinding("t1", "scripted/local/source", true); err != nil {
			t.Fatal(err)
		}
		child, err := store.ForkSessionAccountingOnto(source, destination)
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close()
		if got := child.State(); got.Target != string(destination) || got.RuntimeBinding.Target != "" || len(got.Messages) != 0 {
			t.Fatalf("retargeted accounting child = target %q binding %+v messages %d", got.Target, got.RuntimeBinding, len(got.Messages))
		}
	})
}

func TestOrdinaryForkPreservesRuntimeBinding(t *testing.T) {
	store, source := forkFixture(t)
	want := RuntimeBinding{Tier: "t2", Target: "scripted/local/fallback", Pinned: true}
	if err := source.AppendRuntimeBinding(want.Tier, want.Target, want.Pinned); err != nil {
		t.Fatal(err)
	}
	child, err := store.ForkSession(source, len(source.State().Messages))
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if got := child.State().RuntimeBinding; got != want {
		t.Fatalf("ordinary fork binding = %+v, want %+v", got, want)
	}
}
