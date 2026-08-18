# Head-to-head: three tools, one corpus, one verifier

Run on 2026-08-16, one machine (Apple Silicon macOS, this repository's
development host), by the procedure in
[comparison.md](comparison.md#reproduce-it). The raw journal is
[head-to-head-2026-08-16.jsonl](head-to-head-2026-08-16.jsonl), one line
per attempt.

## What ran

The deterministic one-task-per-package cut of the tier-1 corpus contains
eleven seeded single-defect tasks in this repository, fixed in corpus
order before any tool ran (`internal/eval/bench_test.go`). Each tool ran
headless, one attempt per task, hard-killed at 540 seconds, in its own
fresh materialisation, in the non-interactive posture its documentation
recommends for edit-and-run-tests work:

- **switchboard**, this tree's HEAD: `sb -p "<prompt>" -mode bypass
  -output json`. Exec was confined by the verified Seatbelt sandbox. That build
  activated verified confinement automatically. Current builds start with
  confinement off, and both production profiles retain host IPC authority that
  keeps bypass approval with the human. The historical prompt-free posture is
  therefore not directly replayable. The ladder used this machine's
  configuration (t1 qwen3.5:9b local, t2
  qwen3.8:27b local, t3 kimi-for-coding, t4 gpt-5.6-sol on the codex
  subscription), every session starting at t1 with escalation live.
- **claude code**, installed release on claude-fable-5: `claude -p
  --permission-mode acceptEdits --allowedTools "Bash(go test:*)"
  "Bash(go build:*)" "Bash(go vet:*)" "Bash(gofmt:*)"`.
- **codex cli**, installed release on gpt-5.6-sol: `codex exec --sandbox
  workspace-write --skip-git-repo-check`.

The judge is the corpus's own verifier. It runs the package test suite plus
the mustContain checks that keep "delete the failing test" from counting.
The same code judges every lane.

## Verdicts

Every tool solved every task: **11/11 for all three.** On completion,
over this corpus, the three tools are indistinguishable. One asterisk:
claude code's cache-read-subset fix landed before the watchdog killed the
still-running process at 540s, so its verdict is a solve and its wall
time is a cap, not a measurement.

## What the tie cost

Identical outcomes, three different bills.

| | solved | wall clock | what it consumed |
|---|---|---|---|
| switchboard | 11/11 | 25.7 min | 4 tasks entirely on local rungs (nothing billed), 3 ended on t3 and 4 on t4 (plan quota); **zero API dollars** |
| claude code | 11/11 | 26.8 min | **$22.35** metered across the ten reporting runs, claude-fable-5 throughout |
| codex cli | 11/11 | 21.8 min | 641,576 tokens of gpt-5.6-sol (plan quota) |

The ladder's decisions are the interesting column. Switchboard put
cache-read-subset, prefix-seal, breakpoint-minimum, and
router-feasibility-order entirely on local models. Those 9B and 27B targets
billed nothing. Switchboard climbed to a frontier rung only where its own
escalation evidence said the cheap rung was stuck, finishing three tasks
on kimi and four on gpt-5.6-sol. Claude code bought every task at
frontier price; on this corpus, eleven times out of eleven, that bought
nothing the verifier could see. On this task family, the result supports the
[routing premise](routing.md): allocate a stronger target only when the work
provides evidence that it is needed.

## What this does and does not establish

Eleven tasks, one attempt each, one machine, one day. The family is
narrow: seeded single-defect Go fixes with test verifiers, in a
repository none of the three had seen but whose corpus this project
authored. Claude code ran the model its install defaults to, which is
also the most expensive model in this table; a user who configures it
cheaper moves its number, exactly as a user who pins switchboard to t4
moves its. Wall times overlap too much to rank on. What the run
establishes is precisely this: on this corpus, under each tool's own
recommended headless posture, completion tied at the ceiling, and only
one of the three tools had machinery whose job was to notice when the
ceiling was reachable from a free rung. It worked here. Anything broader
needs a broader corpus, and the instrument for that is committed.
