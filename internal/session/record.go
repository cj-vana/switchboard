package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// The log is a header line followed by framed records:
//
//	switchboard-session 3\n
//	<8-hex payload length> <8-hex crc32c> <compact json>\n
//
// Length and checksum together let replay tell a torn final write from a clean
// end of file, which is the whole point of the format: a session interrupted by
// a kill signal must resume, not refuse to load. Line framing is safe because
// encoding/json compacts and escapes payloads, so no record body contains a raw
// newline.
const (
	magic         = "switchboard-session"
	SchemaVersion = 3

	frameHeaderLen = 18 // 8 hex + space + 8 hex + space
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ErrCorruptRecord ends replay. It is not surfaced to the caller as a failure:
// the log truncates at that point and resumes from the last valid state, with
// the loss reported (§10.3).
var ErrCorruptRecord = errors.New("corrupt record")

// ErrSchemaTooNew refuses a log written by a newer binary. A best-effort parse
// would silently drop records it does not understand.
var ErrSchemaTooNew = errors.New("session was written by a newer version of switchboard")

type RecordType string

const (
	RecordSessionStart   RecordType = "session_start"
	RecordMessage        RecordType = "message"
	RecordUsage          RecordType = "usage"
	RecordRetryReserve   RecordType = "retry_reserve"
	RecordBudgetAttempt  RecordType = "budget_attempt"
	RecordBudgetSettle   RecordType = "budget_settle"
	RecordBudgetTransfer RecordType = "budget_transfer"
	RecordRaceBranch     RecordType = "race_branch"
	RecordRuntimeBinding RecordType = "runtime_binding"
	RecordPermission     RecordType = "permission"
	RecordNote           RecordType = "note"

	// RecordRoute carries §8.4's training signal: what a turn looked like, what
	// was chosen, why, and how it ended. It is written from ordinary sessions
	// rather than only from eval runs, because a corpus of deliberate
	// measurements is a corpus of tasks somebody thought to write down, and the
	// distribution that matters is the one people actually work in.
	RecordRoute RecordType = "route"

	// RecordPin is a named point in the conversation: how many messages the
	// session held when the user marked it. It exists for /fork by name —
	// the cut point survives in the log, so it survives resume and rides a
	// fork's copied prefix — and it is log-only by design: files are /undo's
	// job, and a pin that also promised file state would be promising what
	// the log cannot keep.
	RecordPin RecordType = "pin"

	// RecordRace is one /race verdict: the same prompt, from the same prefix,
	// run on two rungs at once and judged by the user. §8.4's complaint about
	// natural outcomes is that they are weak — a clean completion says nothing
	// about necessity — and a paired, human-judged comparison is the strongest
	// label class ordinary use can produce. This record is where the phase 2b
	// corpus that measurement needs comes from; nothing reads it back into
	// routing, because a learned router is gated on the eval that has not run.
	RecordRace RecordType = "race"
)

const (
	RouteVerificationUnavailable = "unavailable"
	RouteVerificationPassed      = "passed"
	RouteVerificationFailed      = "failed"
)

const (
	RouteFailureProvider     = "provider"
	RouteFailureBudget       = "budget"
	RouteFailureContext      = "context"
	RouteFailureRoundLimit   = "round_limit"
	RouteFailureVerification = "verification"
	RouteFailureCancelled    = "cancelled"
	RouteFailureInternal     = "internal"
)

// Route is one turn's routing decision and outcome.
//
// §8.4 is precise about what each field is worth as evidence, and the reader of
// this record has to be too. An escalation is not a negative label: provider
// failure, a planned phase change, and a bad escalation rule all produce one. A
// clean completion is weak evidence of sufficiency and none of necessity, which
// is the main way a naive router learns to over-provision. Nothing here is a
// label; it is what happened.
type Route struct {
	// The features the decision was made from, so a later reading can tell a
	// good decision from a lucky one.
	TurnDepth         int      `json:"turn_depth"`
	PromptTokens      int      `json:"prompt_tokens"`
	ContextTokens     int      `json:"context_tokens,omitempty"`
	PriorFailures     int      `json:"prior_failures"`
	TestFailures      int      `json:"test_failures,omitempty"`
	FilesInContext    int      `json:"files_in_context"`
	DiffSize          int      `json:"diff_size"`
	DiffSizeKnown     bool     `json:"diff_size_known"`
	TestsInvolved     bool     `json:"tests_involved"`
	PromptChars       int      `json:"prompt_chars"`
	Languages         []string `json:"languages,omitempty"`
	LastTurnEscalated bool     `json:"last_turn_escalated"`

	Tier           string                 `json:"tier"`
	Target         provider.RouteTargetID `json:"target"`
	Source         string                 `json:"source"`
	Rationale      string                 `json:"rationale"`
	PolicyRevision string                 `json:"policy_revision,omitempty"`

	// Escalations is how many times the primary moved during the turn, and
	// EndedOn is where it finished. §8.3 says the mid-task adjustments are
	// worth more than the opening choice, so recording only the opening one
	// would keep the less useful half.
	Escalations int                    `json:"escalations"`
	EndedOn     provider.RouteTargetID `json:"ended_on,omitempty"`
	// EndedTier disambiguates ladder movement when two rungs intentionally
	// share one concrete target. EndedOn remains the machine target identity;
	// neither field alone can recover both facts.
	EndedTier string `json:"ended_tier,omitempty"`

	// Outcome is the raw §8.4 result, including failed when no usable completion
	// or actual move occurred. Turning it into a label is a decision for whoever
	// trains on this, made with the caveats above in front of them.
	Outcome string `json:"outcome"`

	// FailureKind explains why a failed, abandoned, or interrupted routed turn
	// has no clean completion signal. It is deliberately a small stable machine
	// vocabulary: provider availability, a hard budget or context limit, a
	// round limit, an explicit verifier, cancellation, or an internal failure. A route that
	// actually moved can retain Outcome=escalated while still recording the
	// failure that ended it.
	FailureKind string `json:"failure_kind,omitempty"`

	// Verified is whether a task-specific check confirmed the result, which
	// §8.4 calls stronger evidence than the harness's own completion signal.
	// VerificationStatus is always populated by AppendRoute. "unavailable"
	// means no verifier result was correlated with this exact turn at the
	// route-record boundary; consumers must not mistake the two legacy false
	// booleans for a verifier that ran and failed.
	Verified           bool   `json:"verified"`
	VerificationRan    bool   `json:"verification_ran"`
	VerificationStatus string `json:"verification_status"`

	// UsageCallIDs are the exact durable purpose=turn provider calls summed in
	// Usage and CostMicroUSD. Background advisor/learn/compact calls and retry
	// reserves are intentionally outside this correlation.
	UsageCallIDs []string       `json:"usage_call_ids,omitempty"`
	Usage        provider.Usage `json:"usage"`
	CostMicroUSD int64          `json:"cost_micro_usd"`
	WallTimeMS   int64          `json:"wall_time_ms"`
}

// Race is one paired trial. The outcome vocabulary is deliberate, §8.4
// applied to a comparison instead of a turn: "a" and "b" are judged
// preferences; "tie" means both sufficed, which is evidence of necessity for
// the cheaper rung — exactly what a clean completion alone never
// establishes; "abandoned" is censored, not negative; and "incomparable"
// records that an arm failed to finish, because a provider error is not a
// preference and must not be stored as one.
type Race struct {
	Prompt string  `json:"prompt"`
	A      RaceArm `json:"a"`
	B      RaceArm `json:"b"`

	Outcome string `json:"outcome"`
	// Kept names the tier whose branch the session continued on, empty when
	// the race was abandoned and the pre-race session carried on instead.
	Kept string `json:"kept,omitempty"`
}

// RaceArm is what one branch of the trial did. Usage and cost are the
// branch's own — the forked prefix's spend is subtracted — and SessionID
// names the branch log, which survives the verdict: the road not taken
// stays resumable.
type RaceArm struct {
	Tier      string                 `json:"tier"`
	Target    provider.RouteTargetID `json:"target"`
	SessionID string                 `json:"session_id"`

	// Status is "completed", or why the arm has no answer: "error",
	// "cancelled", "round_limit". Only two completed arms can be compared.
	Status string `json:"status"`

	Usage        provider.Usage `json:"usage"`
	CostMicroUSD int64          `json:"cost_micro_usd"`
	WallTimeMS   int64          `json:"wall_time_ms"`
}

// Pin is a named point in the conversation. Messages is the count when the
// pin was set, which is the fork cut that returns there: set between turns,
// it always lands on the boundary /fork requires.
type Pin struct {
	Name     string `json:"name"`
	Messages int    `json:"messages"`
}

type Record struct {
	Seq     int             `json:"seq"`
	At      time.Time       `json:"at"`
	Type    RecordType      `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type SessionStart struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Target    string `json:"target"`
	Binary    string `json:"binary"`

	// CatalogRevision pins the price and capability data this session started
	// against, so a cost recorded here can be checked later against the data
	// that actually produced it rather than whatever is current (§4).
	CatalogRevision string `json:"catalog_revision,omitempty"`
}

// RuntimeBinding is the latest durable routing state for a live session.
// SessionStart.Target remains immutable provenance for where the log began;
// this record is the write-ahead state needed to resume where the runtime
// actually landed. Pinned distinguishes a deliberate user choice from an
// automatically selected primary.
type RuntimeBinding struct {
	Tier   string                 `json:"tier"`
	Target provider.RouteTargetID `json:"target"`
	Pinned bool                   `json:"pinned"`
}

// Usage records one model call. Attempts counts requests actually issued: a
// retry after partial output is billed by most providers, so recording only the
// successful attempt would understate spend (§10.3).
type Usage struct {
	// CallID is the durable identity of the provider call. Forks preserve it,
	// so workspace-wide receipts can deduplicate inherited accounting without
	// weakening the per-session total a hard ceiling relies on. Older logs omit
	// it and readers derive a legacy identity from the record timestamp.
	CallID string `json:"call_id,omitempty"`

	Target   string         `json:"target"`
	Usage    provider.Usage `json:"usage"`
	Duration time.Duration  `json:"duration_ns"`
	Attempts int            `json:"attempts"`

	// Purpose separates user-turn inference from model work Switchboard runs
	// around the loop. Empty is the legacy spelling of "turn".
	Purpose string `json:"purpose,omitempty"`

	// CostMicroUSD is what the catalog priced this call at, in millionths of
	// a dollar. It is stored as a plain integer so the session log stays
	// independent of the catalog's types, and micro-USD rather than cents
	// because a single turn routinely costs a fraction of a cent.
	CostMicroUSD int64 `json:"cost_micro_usd,omitempty"`

	// CatalogRevision and PriceConfidence record what produced the number. A
	// cost with neither is not reproducible, and one priced from a surface
	// default is shape rather than fact.
	CatalogRevision string `json:"catalog_revision,omitempty"`
	PriceConfidence string `json:"price_confidence,omitempty"`

	// At is the record's own timestamp, set by ReadUsages and never
	// serialized — the payload does not carry what the record framing
	// already holds. A fork copies usage records with their At intact, so
	// the timestamp is how an aggregate reader tells a copy from a second
	// real call.
	At time.Time `json:"-"`
}

const (
	UsagePurposeTurn     = "turn"
	UsagePurposeCompact  = "compact"
	UsagePurposeLearn    = "learn"
	UsagePurposeAdvisor  = "advisor"
	UsagePurposeApproval = "approval"
	UsagePurposeUnknown  = "unattributed"
)

func (u Usage) EffectivePurpose() string {
	if u.Purpose == "" {
		return UsagePurposeTurn
	}
	return u.Purpose
}

// RetryReserve is the conservative dollar allowance retained for a provider
// attempt that failed without durable usage. Such requests may still be
// billed, so a hard session ceiling must replay this amount instead of
// forgetting it on the next model call or process restart. It is deliberately
// separate from Usage: no successful response was received.
type RetryReserve struct {
	CostMicroUSD int64 `json:"cost_micro_usd"`
}

// BudgetAttempt is the write-ahead reservation for one provider request. It
// is appended and synced before the request is sent. Until a matching
// BudgetSettlement is durable, replay keeps the whole bound as retry reserve:
// the process may have died after the provider accepted the request.
type BudgetAttempt struct {
	ID           string `json:"id"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
}

const (
	BudgetOutcomeSucceeded = "succeeded"
	BudgetOutcomeFailed    = "failed"
)

// BudgetSettlement closes a write-ahead attempt after its outcome is known.
// A success releases the pessimistic bound. A failure converts the pending
// bound into permanent retry reserve because a request which returned no
// usable response may still appear on the provider's invoice.
//
// ExternalCostMicroUSD is used when the request's Usage lives in a different
// log (a delegate or race arm). It attributes the actual cost to the session
// whose hard ceiling admitted the request without inventing provider usage.
type BudgetSettlement struct {
	AttemptID            string `json:"attempt_id"`
	Outcome              string `json:"outcome"`
	ExternalCostMicroUSD int64  `json:"external_cost_micro_usd,omitempty"`
}

// BudgetTransfer moves already-accounted work onto a new continuing branch.
// Source is stable and unique within the destination log, so an accidental
// duplicate is rejected instead of charging the work twice.
type BudgetTransfer struct {
	Source               string `json:"source"`
	ExternalCostMicroUSD int64  `json:"external_cost_micro_usd,omitempty"`
	RetryReserveMicroUSD int64  `json:"retry_reserve_micro_usd,omitempty"`
}

// RaceBranch gates resumability while a race is in flight. An arm has only
// its own local Usage until the origin ledger is reconciled; exposing it to
// --continue or /resume before finalization would recover budget headroom.
type RaceBranch struct {
	OriginID     string `json:"origin_id"`
	Finalized    bool   `json:"finalized"`
	Continuation bool   `json:"continuation,omitempty"`
}

type Permission struct {
	Tool       string `json:"tool"`
	Mode       string `json:"mode"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason"`
	Approved   bool   `json:"approved"`
	ResolvedBy string `json:"resolved_by,omitempty"`

	Reviewer       string `json:"reviewer,omitempty"`
	ReviewDecision string `json:"review_decision,omitempty"`
	ReviewReason   string `json:"review_reason,omitempty"`
	ReviewError    string `json:"review_error,omitempty"`
}

type Note struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

func encodeRecord(rec Record) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encoding %s record: %w", rec.Type, err)
	}
	frame := make([]byte, 0, frameHeaderLen+len(payload)+1)
	frame = fmt.Appendf(frame, "%08x %08x ", len(payload), crc32.Checksum(payload, crcTable))
	frame = append(frame, payload...)
	return append(frame, '\n'), nil
}

// decodeRecord reads one framed record. It returns ErrCorruptRecord for a torn
// or altered frame and io.EOF at a clean end of log.
func decodeRecord(r *bufio.Reader) (Record, int, error) {
	line, err := r.ReadBytes('\n')
	if len(line) == 0 {
		if err == nil {
			err = io.EOF
		}
		return Record{}, 0, err
	}
	consumed := len(line)

	// A line with no terminator is the tail of a write that did not finish.
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Record{}, consumed, ErrCorruptRecord
		}
		return Record{}, consumed, err
	}
	if len(line) < frameHeaderLen+1 || line[8] != ' ' || line[17] != ' ' {
		return Record{}, consumed, ErrCorruptRecord
	}

	var wantLen, wantCRC uint32
	if _, err := fmt.Sscanf(string(line[:frameHeaderLen-1]), "%08x %08x", &wantLen, &wantCRC); err != nil {
		return Record{}, consumed, ErrCorruptRecord
	}

	payload := line[frameHeaderLen : len(line)-1]
	if uint32(len(payload)) != wantLen || crc32.Checksum(payload, crcTable) != wantCRC {
		return Record{}, consumed, ErrCorruptRecord
	}

	var rec Record
	if err := json.Unmarshal(payload, &rec); err != nil {
		return Record{}, consumed, ErrCorruptRecord
	}
	return rec, consumed, nil
}
