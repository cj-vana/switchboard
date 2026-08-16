package router

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
)

// Detector turns what happens inside a turn into the signals §8.3 escalates on.
//
// It is deliberately conservative. Every rule here fires on something the loop
// can actually see, and the triggers §8.3 lists that need state the loop does
// not keep are absent rather than approximated: an edit reverted twice needs
// edit history, and a diff crossing a threshold needs a running diff. Emitting
// a guess in their place would be worse than emitting nothing, because the
// policy would escalate on evidence that does not exist.
//
// It is safe for concurrent use, which is not optional: the agent loop runs a
// turn's tool calls in parallel goroutines, so every one of these methods is
// called concurrently. Assuming otherwise crashed a real run on a concurrent
// map write, and the assumption was written in a comment rather than tested.
type Detector struct {
	mu sync.Mutex

	// ErrorSpikeAt is how many failed tool calls in one turn count as a spike.
	// Tools fail routinely, so one is not news.
	ErrorSpikeAt int

	calls     map[string]int
	failures  map[string]bool
	errors    int
	spiked    bool
	repeated  map[string]bool
	uncertain bool
}

const DefaultErrorSpikeAt = 3

func NewDetector() *Detector {
	return &Detector{
		calls:    map[string]int{},
		failures: map[string]bool{},
		repeated: map[string]bool{},
	}
}

func (d *Detector) spikeAt() int {
	if d.ErrorSpikeAt > 0 {
		return d.ErrorSpikeAt
	}
	return DefaultErrorSpikeAt
}

// Reset clears per-turn state. Signatures do not survive a turn, because §8.3
// counts consecutive failures within one, and carrying them across would
// escalate a fresh turn for something already dealt with.
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = map[string]int{}
	d.failures = map[string]bool{}
	d.repeated = map[string]bool{}
	d.errors = 0
	d.spiked = false
	d.uncertain = false
}

// ToolCall reports a call about to run.
func (d *Detector) ToolCall(name string, input []byte) []Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := name + "\x00" + string(input)
	d.calls[key]++

	// Loop detection: the same call with the same arguments, which cannot be
	// making progress. Reported once, because after that every further
	// repetition would escalate again on the same evidence.
	if d.calls[key] > 1 && !d.repeated[key] {
		d.repeated[key] = true
		return []Signal{RepeatedToolCall}
	}
	return nil
}

// ToolResult reports what a call produced.
func (d *Detector) ToolResult(name, argv, output string, failed bool) []Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !failed {
		return nil
	}

	var signals []Signal

	d.errors++
	if d.errors >= d.spikeAt() && !d.spiked {
		d.spiked = true
		signals = append(signals, ToolErrorSpike)
	}

	// A failing test run is the case §8.3 singles out, and only a signature not
	// seen before counts. The same failure twice is one problem observed twice,
	// and counting it again escalates for persistence rather than difficulty.
	if looksLikeTests(argv) {
		if sig := failureSignature(output); sig != "" && !d.failures[sig] {
			d.failures[sig] = true
			signals = append(signals, NewTestFailure)
		}
	}
	return signals
}

// VerifierFailures reports a failing run of the user's declared verifier —
// the /watch command. §8.4 calls a task-specific verifier stronger evidence
// than the harness's own completion signal, and the declaration is what
// separates this from ToolResult: no command-shape check, because the user
// said this is the verifier, and no error-spike count, because the verifier
// failing means the task is broken, not that a tool call went wrong. The
// signature set is shared with ToolResult, so a failure the model's own test
// run already surfaced is one problem observed twice, not two problems.
//
// However many signatures one run produced, it contributes at most one
// signal: one run is one observation, the same weight ToolResult gives it.
func (d *Detector) VerifierFailures(sigs []string) []Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	fresh := false
	for _, sig := range sigs {
		if sig == "" || d.failures[sig] {
			continue
		}
		d.failures[sig] = true
		fresh = true
	}
	if fresh {
		return []Signal{NewTestFailure}
	}
	return nil
}

// AssistantText reports model output. Hedging is reported at most once per
// turn: §8.3 makes it a weak signal, and repeating it would let volume stand in
// for evidence.
func (d *Detector) AssistantText(text string) []Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.uncertain || !hedging(text) {
		return nil
	}
	d.uncertain = true
	return []Signal{UncertaintyLanguage}
}

// testCommand matches the shapes a test run takes across the ecosystems this
// is likely to meet. It is a heuristic and wrong at the edges; the cost of a
// false positive is half an escalation vote, which is why it is allowed to be.
var testCommand = regexp.MustCompile(`(?i)\b(go test|npm (run )?test|yarn test|pnpm test|pytest|cargo test|make test|ctest|rspec|jest|vitest|phpunit|dotnet test|gradle(w)? test|mvn test)\b`)

func looksLikeTests(argv string) bool { return testCommand.MatchString(argv) }

// failureLine matches the first line that names a failure, which is what makes
// one failure distinguishable from another.
var failureLine = regexp.MustCompile(`(?i)^\s*(---\s*FAIL|FAIL|PASS.*FAIL|E\s|ERROR|assert|panic:|\S+\.go:\d+:|\S+\.(py|rs|ts|js|java):\d+)`)

// Failure is one failing line reduced to something comparable: the line as
// printed, for a human or a model to read, and its signature, for telling
// whether two runs failed the same way.
type Failure struct {
	Signature string
	Line      string
}

// SignatureOf reduces one line to its comparable form. Digits are dropped so
// a line number shifting by one edit does not make the same failure look like
// a different one.
func SignatureOf(line string) string {
	normalized := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return -1
		}
		return r
	}, strings.TrimSpace(line))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

// ExtractFailures finds every line in the output that names a failure. It is
// lines rather than the whole output, because output carries timings, paths,
// and counts that differ between two runs of the same broken thing. Comparing
// whole outputs would make every failure look new, and every retry would
// escalate.
func ExtractFailures(output string) []Failure {
	var out []Failure
	for _, line := range strings.Split(output, "\n") {
		if !failureLine.MatchString(line) {
			continue
		}
		out = append(out, Failure{Signature: SignatureOf(line), Line: strings.TrimSpace(line)})
	}
	return out
}

// failureSignature is the first failing line's signature, which is what one
// tool result contributes as evidence: one run is one observation, however
// many assertions it took down with it.
func failureSignature(output string) string {
	if fs := ExtractFailures(output); len(fs) > 0 {
		return fs[0].Signature
	}
	return ""
}

// hedges are phrases that suggest the model is unsure. §8.3 is explicit that
// this is weak and never sufficient alone, which the policy enforces by
// capping its contribution below the threshold.
var hedges = []string{
	"i'm not sure", "i am not sure", "not entirely sure",
	"it's unclear", "it is unclear", "i can't tell",
	"i cannot tell", "hard to say", "might be wrong",
	"i may have", "not certain",
}

func hedging(text string) bool {
	lowered := strings.ToLower(text)
	for _, h := range hedges {
		if strings.Contains(lowered, h) {
			return true
		}
	}
	return false
}
