package gate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/provider/ollama"
	"github.com/cj-vana/switchboard/internal/provider/openaicompat"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// model is served by both adapters, which is what makes the comparison a
// property of the route rather than of the weights.
const model = "qwen3.5:9b-mlx"

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run the phase-1 exit gate against a local server")
	}
}

// task is one unit of the gate corpus. Each has an outcome that can be checked
// from the filesystem rather than by reading the model's prose, because a gate
// that grades an answer by eye is not a gate.
type task struct {
	name    string
	setup   func(t *testing.T, dir string)
	prompt  string
	verify  func(t *testing.T, dir string) error
	rounds  int
	timeout time.Duration
}

func corpus() []task {
	return []task{
		{
			name: "read-and-report",
			setup: func(t *testing.T, dir string) {
				write(t, dir, "config.txt", "timeout = 45\nretries = 3\n")
			},
			prompt: "Read config.txt and write the timeout value, digits only, into timeout.txt.",
			verify: func(t *testing.T, dir string) error {
				return fileContains(dir, "timeout.txt", "45")
			},
			rounds:  8,
			timeout: 4 * time.Minute,
		},
		{
			name: "edit-in-place",
			setup: func(t *testing.T, dir string) {
				write(t, dir, "greeting.py", "def greet():\n    print(\"hello\")\n")
			},
			prompt: "In greeting.py, change the word hello to goodbye. Change nothing else.",
			verify: func(t *testing.T, dir string) error {
				if err := fileContains(dir, "greeting.py", "goodbye"); err != nil {
					return err
				}
				body, err := os.ReadFile(filepath.Join(dir, "greeting.py"))
				if err != nil {
					return err
				}
				if strings.Contains(string(body), "hello") {
					return fmt.Errorf("the original word is still there: %s", body)
				}
				if !strings.Contains(string(body), "def greet():") {
					return fmt.Errorf("the surrounding code did not survive the edit: %s", body)
				}
				return nil
			},
			rounds:  8,
			timeout: 4 * time.Minute,
		},
		{
			name: "multi-file-search",
			setup: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "nothing here\n")
				write(t, dir, "b.txt", "the marker is XYZZY\n")
				write(t, dir, "c.txt", "nor here\n")
			},
			prompt: "One of a.txt, b.txt, or c.txt contains a marker word in capitals. " +
				"Find it and write just that word into found.txt.",
			verify: func(t *testing.T, dir string) error {
				return fileContains(dir, "found.txt", "XYZZY")
			},
			rounds:  12,
			timeout: 5 * time.Minute,
		},
	}
}

// TestExitGate is the phase-1 gate: identical tasks on two pinned targets, with
// the estimator measured against what the servers actually reported.
//
// It fails on the first half (a task that does not complete on both targets)
// and reports the second half rather than asserting a bound. That asymmetry is
// deliberate: no bound has been documented yet, so this run is what produces
// one. TestEstimatorStaysWithinTheDocumentedBound is where the number, once
// written down, is defended.
func TestExitGate(t *testing.T) {
	requireLive(t)

	targets := pinnedTargets(t)
	var all []Sample

	for _, tgt := range targets {
		for _, task := range corpus() {
			dir := t.TempDir()
			task.setup(t, dir)

			rec, err := runTask(t, tgt, task, dir)
			if err != nil {
				t.Errorf("%s on %s: %v", task.name, tgt.target.ID(), err)
				continue
			}
			if err := task.verify(t, dir); err != nil {
				t.Errorf("%s on %s produced the wrong result: %v", task.name, tgt.target.ID(), err)
			}
			all = append(all, rec.Samples()...)
		}
	}

	if len(all) == 0 {
		t.Fatal("no calls were recorded, so nothing was measured")
	}

	t.Logf("\n%s", Table(all))
	for _, tgt := range targets {
		t.Logf("%s", Summarize(tgt.target.ID(), all))
	}
}

// TestEstimatorStaysWithinTheDocumentedBound defends the number in
// docs/estimator.md.
//
// The bound is a claim about this estimator on these targets, measured rather
// than chosen. If a change to the prompt, the tool schemas, or the estimator
// moves it, this fails and the document is what has to be updated, so the
// number and the code cannot drift apart silently.
func TestEstimatorStaysWithinTheDocumentedBound(t *testing.T) {
	requireLive(t)

	var all []Sample
	for _, tgt := range pinnedTargets(t) {
		for _, task := range corpus() {
			dir := t.TempDir()
			task.setup(t, dir)
			rec, err := runTask(t, tgt, task, dir)
			if err != nil {
				t.Fatalf("%s on %s: %v", task.name, tgt.target.ID(), err)
			}
			all = append(all, rec.Samples()...)
		}
	}

	for _, s := range all {
		if s.Ratio() < MinRatio || s.Ratio() > MaxRatio {
			t.Errorf("%s on %s: estimate %d against actual %d is a ratio of %.2f, "+
				"outside the documented %.2f-%.2f band in docs/estimator.md",
				s.Task, s.Target, s.Estimate, s.Actual, s.Ratio(), MinRatio, MaxRatio)
		}
	}
}

// pinned is one target with the adapter that serves it.
type pinned struct {
	target provider.RouteTarget
	client provider.Provider
}

// pinnedTargets is the two route targets the gate compares: one model, two
// adapters, two wire formats. §19.3 allows the first two high-fidelity targets
// to be two models on one provider; two protocols to one model isolates the
// portability question more sharply, and costs nothing to run.
func pinnedTargets(t *testing.T) []pinned {
	t.Helper()

	native := ollama.New()
	compat, err := openaicompat.New("ollama")
	if err != nil {
		t.Fatal(err)
	}
	compatTarget, err := openaicompat.Target("ollama", model)
	if err != nil {
		t.Fatal(err)
	}

	targets := []pinned{
		{target: ollama.Target(model), client: native},
		{target: compatTarget, client: compat},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, p := range targets {
		res, err := p.client.Probe(ctx, p.target)
		if err != nil {
			t.Fatalf("probing %s: %v", p.target.ID(), err)
		}
		if !res.ModelPresent {
			t.Skipf("%s is not served: %s", p.target.ID(), res.Detail)
		}
	}
	return targets
}

func runTask(t *testing.T, p pinned, task task, dir string) (*Recorder, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), task.timeout)
	defer cancel()

	cat, err := catalog.LoadBundled()
	if err != nil {
		return nil, err
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		return nil, err
	}
	sess, err := store.Create(dir, p.target.ID(), cat.Revision)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	capability := execution.Detect()
	registry, err := tools.NewRegistry(dir, capability)
	if err != nil {
		return nil, err
	}

	rec := NewRecorder(p.client)
	rec.Task = task.name

	loop := &agent.Loop{
		Provider: rec,
		Target:   p.target,
		Tools:    registry,
		// The corpus writes files, and a gate that stops to ask cannot run
		// unattended. Reads and writes are approved; the sandbox still governs
		// commands, and the workspace is a directory the test created.
		Perms:         permission.NewEngine(permission.ModeAcceptEdits, capability),
		Asker:         refusingAsker{},
		Session:       sess,
		Observer:      agent.NopObserver{},
		Catalog:       cat,
		System:        agent.SystemPrompt(dir, permission.ModeAcceptEdits, capability),
		MaxToolRounds: task.rounds,
	}

	if err := loop.Turn(ctx, task.prompt); err != nil {
		return rec, err
	}
	return rec, nil
}

// refusingAsker fails rather than blocking. Anything that reaches it is a task
// the corpus should not have needed approval for, and a gate that silently
// approved it would be measuring a different run than the one described.
type refusingAsker struct{}

func (refusingAsker) Ask(context.Context, permission.Request, permission.Outcome) (permission.Response, error) {
	return permission.Response{}, errors.New("the gate corpus should not need an approval prompt")
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileContains(dir, name, want string) error {
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if !strings.Contains(string(body), want) {
		return fmt.Errorf("%s does not contain %q: %q", name, want, body)
	}
	return nil
}

// The summary arithmetic runs without a server, because a reporting bug in the
// gate would be indistinguishable from a bad estimator.
func TestSummarize(t *testing.T) {
	const target provider.RouteTargetID = "test/surface/model"
	samples := []Sample{
		{Target: target, Estimate: 90, Actual: 100},  // 0.90, undercount
		{Target: target, Estimate: 120, Actual: 100}, // 1.20
		{Target: target, Estimate: 100, Actual: 100}, // 1.00
		{Target: "other/surface/model", Estimate: 1, Actual: 1000},
	}

	got := Summarize(target, samples)
	if got.Calls != 3 {
		t.Errorf("calls = %d; samples for another target must not be folded in", got.Calls)
	}
	if math.Abs(got.MinRatio-0.9) > 1e-9 || math.Abs(got.MaxRatio-1.2) > 1e-9 {
		t.Errorf("range = %.2f-%.2f, want 0.90-1.20", got.MinRatio, got.MaxRatio)
	}
	if math.Abs(got.MedianRatio-1.0) > 1e-9 {
		t.Errorf("median = %.2f, want 1.00", got.MedianRatio)
	}
	if math.Abs(got.WorstUnderReport-0.10) > 1e-9 {
		t.Errorf("worst undercount = %.2f, want 0.10", got.WorstUnderReport)
	}
}

// The estimator predicts the whole prompt; the adapter reports the uncached
// remainder separately. Comparing against the remainder would credit the
// estimator for a cache it knows nothing about, and the error would appear to
// shrink as caching improved.
func TestCachedTokensCountTowardTheActual(t *testing.T) {
	rec := NewRecorder(nil)
	s := &recordingStream{
		EventStream: staticStream{provider.Event{
			Type:  provider.EventDone,
			Usage: provider.Usage{InputTokens: 200, CacheReadTokens: 800},
		}},
		recorder: rec,
		target:   "test/surface/model",
		estimate: 1000,
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}

	samples := rec.Samples()
	if len(samples) != 1 {
		t.Fatalf("recorded %d samples", len(samples))
	}
	if samples[0].Actual != 1000 {
		t.Errorf("actual = %d, want the whole prompt of 1000", samples[0].Actual)
	}
	if samples[0].Ratio() != 1.0 {
		t.Errorf("ratio = %.2f; a perfect estimate must not look wrong because the prompt was cached", samples[0].Ratio())
	}
}

type staticStream []provider.Event

func (s staticStream) Next() (provider.Event, error) { return s[0], nil }
func (s staticStream) Close() error                  { return nil }
