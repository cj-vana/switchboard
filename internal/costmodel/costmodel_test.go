package costmodel

import (
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/provider"
)

func infoFor(t *testing.T, p, surface, model string) (provider.RouteTarget, catalog.ModelInfo) {
	t.Helper()
	c, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: p, Surface: surface, ModelID: model}
	info, _, ok := c.Lookup(target)
	if !ok {
		t.Fatalf("%s has no catalog entry", target.ID())
	}
	return target, info
}

func inputs(t *testing.T, model string, prefix, fresh, output int) Inputs {
	t.Helper()
	target, info := infoFor(t, "anthropic", "first-party", model)
	return Inputs{
		Target: target, Info: info,
		PrefixTokens: prefix, FreshTokens: fresh, OutputTokens: output,
		Eligible: true, HitProbability: 0.95, TokensAreExact: true,
		TTL: 5 * time.Minute,
	}
}

// The inversion §6.4 exists to prevent, priced through the estimator rather
// than by hand: reading a warm prefix on the expensive model beats cold-writing
// the same prefix on the cheap one.
func TestTheCheaperModelCanCostMoreForTheTurn(t *testing.T) {
	const prefix = 80_000

	warmOpus := inputs(t, "claude-opus-5", prefix, 0, 0)
	warmOpus.HitProbability = 1

	coldHaiku := inputs(t, "claude-haiku-4-5", prefix, 0, 0)
	coldHaiku.HitProbability = 0

	opus := Estimator{}.Turn(warmOpus)
	haiku := Estimator{}.Turn(coldHaiku)

	if opus.Expected >= haiku.Expected {
		t.Errorf("warm Opus cost %s and cold Haiku cost %s; the inversion did not reproduce",
			opus.Expected, haiku.Expected)
	}
	// And the base input rate points the other way, which is the whole point:
	// a router comparing list prices would pick the more expensive turn.
	_, opusInfo := infoFor(t, "anthropic", "first-party", "claude-opus-5")
	_, haikuInfo := infoFor(t, "anthropic", "first-party", "claude-haiku-4-5")
	opusBand, _ := opusInfo.Band(prefix)
	haikuBand, _ := haikuInfo.Band(prefix)
	if opusBand.InputPerMTok <= haikuBand.InputPerMTok {
		t.Fatal("this test no longer demonstrates anything: Opus is not the more expensive model by list price")
	}
}

// A hit and a miss are the two things that actually happen, and the spread
// between them is what the cache is worth on a target.
func TestExpectedCostMovesWithTheHitProbability(t *testing.T) {
	in := inputs(t, "claude-haiku-4-5", 50_000, 500, 200)

	certainMiss := in
	certainMiss.HitProbability = 0
	certainHit := in
	certainHit.HitProbability = 1

	miss := Estimator{}.Turn(certainMiss)
	hit := Estimator{}.Turn(certainHit)
	even := in
	even.HitProbability = 0.5
	middle := Estimator{}.Turn(even)

	if !(hit.Expected < middle.Expected && middle.Expected < miss.Expected) {
		t.Errorf("expected cost did not track the probability: hit %s, even %s, miss %s",
			hit.Expected, middle.Expected, miss.Expected)
	}
	if hit.Spread() <= 0 {
		t.Error("a warm cache saved nothing, so there is nothing for a router to weigh")
	}
}

// A turn that cannot hit is priced as ordinary input, and the probability is
// not consulted. Otherwise a sub-minimum prefix would be priced as though it
// were cached.
func TestAnIneligibleTurnIsPricedAsInput(t *testing.T) {
	in := inputs(t, "claude-haiku-4-5", 2_000, 100, 50)
	in.Eligible = false
	in.HitProbability = 0.95 // a stale belief that must not be used

	got := Estimator{}.Turn(in)
	if got.HitProbability != 0 {
		t.Errorf("hit probability = %.2f on a turn that cannot hit", got.HitProbability)
	}
	if got.HitCost != got.MissCost {
		t.Error("an ineligible turn priced a hit differently from a miss")
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "cannot hit") {
		t.Errorf("notes did not explain why: %v", got.Notes)
	}
}

// The token estimate is 18 to 24 percent low on every target that cannot answer
// exactly, and never high. The bounds have to widen upward for that or a budget
// check inherits the bias.
func TestEstimatedTokensWidenTheBoundsUpward(t *testing.T) {
	exact := inputs(t, "claude-haiku-4-5", 50_000, 500, 200)
	estimated := exact
	estimated.TokensAreExact = false

	a := Estimator{}.Turn(exact)
	b := Estimator{}.Turn(estimated)

	if b.High <= a.High {
		t.Errorf("estimated tokens did not widen the upper bound: %s against %s", b.High, a.High)
	}
	if b.Low < a.Low {
		// Widening downward would let a budget check believe a turn is cheaper
		// than the measurement supports.
		t.Errorf("the lower bound moved down: %s against %s", b.Low, a.Low)
	}
	if !strings.Contains(strings.Join(b.Notes, " "), "estimator.md") {
		t.Errorf("the widening was not attributed to the measurement: %v", b.Notes)
	}
}

func TestBoundsBracketTheExpectation(t *testing.T) {
	in := inputs(t, "claude-haiku-4-5", 50_000, 500, 200)
	in.TokensAreExact = false
	in.HitProbability = 0.5

	got := Estimator{}.Turn(in)
	if !(got.Low <= got.Expected && got.Expected <= got.High) {
		t.Errorf("expectation %s is outside its bounds %s to %s", got.Expected, got.Low, got.High)
	}
}

// A switch does not evict the source cache. Charging the whole prior write as a
// residual would double-count a sunk cost; what is at risk is the future saving,
// weighted by the chance the entry is gone before the session returns.
func TestSwitchDoesNotChargeTheSunkWrite(t *testing.T) {
	from := inputs(t, "claude-opus-5", 80_000, 500, 200)
	to := inputs(t, "claude-haiku-4-5", 80_000, 500, 200)
	to.HitProbability = 0 // cold on the destination

	// Certain to return, and the source entry certainly still warm.
	stillWarm := Estimator{}.SwitchCost(from, to, 1, 1)
	if stillWarm.LostWarmValue != 0 {
		t.Errorf("charged %s for a prefix that is still there", stillWarm.LostWarmValue)
	}

	// Certain to return, and the source certainly gone.
	gone := Estimator{}.SwitchCost(from, to, 1, 0)
	if gone.LostWarmValue <= 0 {
		t.Error("charged nothing for a warm prefix that will be gone when the session returns")
	}

	// The charge is the lost saving, never the whole prior write.
	fromEstimate := Estimator{}.Turn(from)
	if gone.LostWarmValue > fromEstimate.Spread() {
		t.Errorf("lost value %s exceeds what a warm read was worth (%s), which double-counts",
			gone.LostWarmValue, fromEstimate.Spread())
	}

	// Never returning makes the source irrelevant.
	neverBack := Estimator{}.SwitchCost(from, to, 0, 0)
	if neverBack.LostWarmValue != 0 {
		t.Errorf("charged %s for a prefix the session will not come back to", neverBack.LostWarmValue)
	}
}

// Money and quota are not the same resource, and a difference of zero between
// them is not a reason to switch.
func TestSwitchAcrossMeteringSaysSo(t *testing.T) {
	from := inputs(t, "claude-haiku-4-5", 50_000, 500, 200)

	planTarget, planInfo := infoFor(t, "openai", "subscription", "gpt-5.4-mini")
	to := Inputs{
		Target: planTarget, Info: planInfo,
		PrefixTokens: 50_000, FreshTokens: 500, OutputTokens: 200,
		Eligible: true, HitProbability: 0.5, TokensAreExact: false,
	}

	got := Estimator{}.SwitchCost(from, to, 0.5, 0.5)
	if !strings.Contains(strings.Join(got.Notes, " "), "metered differently") {
		t.Errorf("a switch between money and quota did not say so: %v", got.Notes)
	}
}

// A plan target prices at zero, and the estimate has to say that this is not
// the same as costing nothing to use.
func TestPlanTargetSaysZeroIsNotFree(t *testing.T) {
	target, info := infoFor(t, "kimi", "coding", "k3-256k")
	got := Estimator{}.Turn(Inputs{
		Target: target, Info: info,
		PrefixTokens: 50_000, FreshTokens: 500, OutputTokens: 200,
		Eligible: true, HitProbability: 0.5,
	})

	if got.Expected != 0 {
		t.Errorf("expected %s on a plan target", got.Expected)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "quota") {
		t.Errorf("notes did not name what a plan actually consumes: %v", got.Notes)
	}
}

func TestLocalTargetSaysNothingIsMetered(t *testing.T) {
	target, info := infoFor(t, "ollama", "local", "qwen3.5:9b-mlx")
	got := Estimator{}.Turn(Inputs{
		Target: target, Info: info,
		PrefixTokens: 50_000, OutputTokens: 200,
	})

	if got.Expected != 0 {
		t.Errorf("expected %s on a local target", got.Expected)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "locally") {
		t.Errorf("notes = %v", got.Notes)
	}
}
