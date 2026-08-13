# Token estimator error

`Provider.CountTokens` returns a prediction, not a count. This records how wrong
it is, measured rather than guessed, so that a budget check built on it later
can widen its margin by a known amount instead of an imagined one.

## The measurement

`internal/gate` runs three tool-using tasks on two pinned route targets and
compares, for every request the agent loop actually sent, what the estimator
predicted against what the server reported.

```
SB_LIVE=1 go test ./internal/gate/ -run TestExitGate -v -timeout 40m
```

The two targets are `qwen3.5:9b-mlx` reached natively and the same model reached
through Ollama's OpenAI-compatible endpoint. One model, two adapters, two wire
formats: any difference between them is a property of the route rather than of
the weights.

The recorder wraps the provider instead of living inside the adapter or the
loop, so what gets measured is the real request in its real shape, with the
system prompt, the tool schemas, and the accumulated history included. An
estimator checked only against hand-built requests is checked against the easy
case.

Cached prompt tokens are added back before comparing. The estimator predicts the
whole prompt and the adapter reports the uncached remainder separately, so
comparing against the remainder would credit the estimator for a cache it knows
nothing about, and the apparent error would shrink as caching improved.

## Result, 2026-08-13

18 calls, 9 per target.

| Target | Ratio (estimate / actual) | Median | Worst undercount |
|---|---|---|---|
| `ollama/local/qwen3.5:9b-mlx` | 0.76 to 0.82 | 0.81 | 24% |
| `openaicompat/ollama/qwen3.5:9b-mlx` | 0.77 to 0.82 | 0.80 | 23% |

Three things in that table matter more than the range itself.

**The error is one-directional.** Not one of the 18 calls overcounted. The
estimator always claims a request is smaller than it is, which is the dangerous
direction: a budget check that believes a request is cheap approves spending
that has already happened by the time the server disagrees.

**The error grows within a conversation.** Each task starts near 0.82 and falls
toward 0.76 as rounds accumulate. The estimate is characters divided by four,
and characters are all it counts. Every message carries framing the model's chat
template adds and the estimator does not see, so the shortfall compounds with
each round rather than staying flat.

**The two adapters agree to within about a hundredth.** That places the error in
the estimator, not in either wire format. A fix in one place fixes both.

## What this does not cover

The dollar half of reconciliation is unexercised. Both reachable targets are
free, so the measured cost is $0 against $0, which proves nothing about the
pricing path. That half stays open until an adapter reaches a billing provider.

It is a weaker gap than it sounds. Cost is a multiplication over token counts
against a catalog rate, so an estimator with a bounded error yields a cost
estimate bounded by the same factor. The arithmetic is covered by
`internal/catalog` tests that reproduce the §6.4 worked example exactly. What is
genuinely untested is whether a real provider's invoice matches the tokens it
reported, and no local model can answer that.

The corpus is three tasks on one model. It is enough to establish a direction
and an order of magnitude, and not enough to claim a distribution.

## The bound this defends

`gate.MinRatio` and `gate.MaxRatio` hold 0.70 and 1.10, wide enough to absorb
run-to-run variation in what the model chooses to do and tight enough to catch a
real change.

```
SB_LIVE=1 go test ./internal/gate/ -run TestEstimatorStaysWithinTheDocumentedBound -timeout 40m
```

If the estimator, the system prompt, or the tool schemas change enough to move
the ratio, that test fails and this document is what has to be updated. The
number and the code cannot drift apart quietly.

## What to do about it

Nothing consumes `CountTokens` yet, which is why the answer for now is to write
the error down rather than to correct it. The cost estimator is phase 2a work
(§19.2), and it should start from a token estimate that accounts for per-message
framing rather than from characters over four. The measurement above says what
that correction has to be worth: roughly a fifth of the prompt at these
conversation lengths, more as they grow.

Until then, any consumer should treat the number as a floor, not an estimate.
`TokenEstimate.Exact` is false for exactly this reason.
