// Package session stores conversations as append-only event logs on disk.
//
// A session is a file, not a database row. Replay reconstructs the whole state,
// which keeps the canonical log the source of truth even when a provider offers
// a server-side continuation handle (§5.2, §12).
package session

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// ErrSessionLocked reports that another process is appending to this session.
// Two writers would interleave frames and corrupt the log.
var ErrSessionLocked = errors.New("session is open in another process")

// ErrSessionPoisoned means a durable append had an ambiguous or partial
// failure. No later record may be appended behind that point: replay stops at
// the first torn frame, so a later WAL record could otherwise appear synced
// while being unreachable after restart.
var ErrSessionPoisoned = errors.New("session log is poisoned by a failed append")

var ErrRaceBranchPending = errors.New("race branch is not resumable until its origin ledger is reconciled")

var ErrNoSessions = errors.New("no sessions recorded for this workspace")

type Store struct {
	root string
}

// DefaultStore places sessions under the user's config directory rather than in
// the workspace, so a session never lands in a repository or a build artifact.
func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return NewStore(filepath.Join(home, ".switchboard", "sessions"))
}

func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating session store: %w", err)
	}
	return &Store{root: root}, nil
}

// State is the result of replaying a log.
type State struct {
	ID        string
	Workspace string
	Target    string
	CreatedAt time.Time

	// RuntimeBinding is the latest committed tier/target/pin state. Its zero
	// value identifies a legacy log that only has SessionStart.Target.
	RuntimeBinding RuntimeBinding

	// Messages includes assistant messages marked Incomplete. They are kept for
	// diagnosis and display; adapters drop them when rendering a request so an
	// interrupted turn is never replayed as a finished one (§10.3).
	Messages []provider.Message

	Usage provider.Usage
	Calls int

	// UsageTargets is replay-derived from the existing per-call Usage records.
	// It contains each non-empty target identity once, in first-seen order, so
	// cost surfaces can distinguish mixed metering without another record type.
	UsageTargets []string

	// CostMicroUSD totals what the catalog priced this session at. It is an
	// estimate and a reconciliation aid, never a substitute for the provider's
	// invoice (§15).
	CostMicroUSD int64

	// RetryReserveMicroUSD totals conservative allowances for failed provider
	// attempts that may still be billed despite returning no usage, plus
	// write-ahead attempts whose settlement was never made durable. Keeping it
	// separate preserves the distinction between observed usage and a
	// pessimistic hard-ceiling reserve.
	RetryReserveMicroUSD int64

	// ExternalCostMicroUSD is actual priced work admitted by this session but
	// whose provider Usage lives in another log, such as a delegate or losing
	// race arm. It participates in the hard ceiling without fabricating tokens
	// or calls in this session's provider telemetry.
	ExternalCostMicroUSD int64

	// Pins are the named points /fork can cut back to, in the order set,
	// with a re-used name moving its pin rather than stacking a second.
	Pins []Pin

	// Continuity is the latest bounded task-state record, including a cleared
	// tombstone. ContinuityRef is the newest capsule an appended message says
	// was made model-visible; comparing their IDs prevents reinjection after a
	// process resume without parsing prompt prose.
	Continuity    *continuity.Capsule
	ContinuityRef string

	// CatalogRevision is the revision the session started against.
	CatalogRevision string

	pendingBudgetAttempts  map[string]int64
	appliedBudgetTransfers map[string]bool
	providerCallIDs        map[string]bool
	raceBranchOrigin       string
	raceBranchPending      bool
	raceBranchFinalized    bool
	raceBranchContinuation bool
}

// AccountedCostMicroUSD is the observed dollar cost attributable to this
// continuing session. RetryReserveMicroUSD is deliberately excluded: callers
// display or add that pessimistic allowance separately.
func (s State) AccountedCostMicroUSD() int64 {
	total, err := checkedMicroUSDAdd(s.CostMicroUSD, s.ExternalCostMicroUSD)
	if err != nil {
		return math.MaxInt64
	}
	return total
}

func checkedMicroUSDAdd(current, delta int64) (int64, error) {
	if current < 0 || delta < 0 || delta > math.MaxInt64-current {
		return 0, fmt.Errorf("micro-USD accounting overflow")
	}
	return current + delta, nil
}

func (s State) checkedObservedCost(localDelta, externalDelta int64) (local, external int64, err error) {
	local, err = checkedMicroUSDAdd(s.CostMicroUSD, localDelta)
	if err != nil {
		return 0, 0, err
	}
	external, err = checkedMicroUSDAdd(s.ExternalCostMicroUSD, externalDelta)
	if err != nil {
		return 0, 0, err
	}
	if _, err = checkedMicroUSDAdd(local, external); err != nil {
		return 0, 0, err
	}
	return local, external, nil
}

func (s State) checkedUsage(u Usage) (provider.Usage, error) {
	if err := u.Usage.Validate(); err != nil {
		return provider.Usage{}, fmt.Errorf("invalid provider usage: %w", err)
	}
	if u.CostMicroUSD < 0 {
		return provider.Usage{}, fmt.Errorf("usage cost cannot be negative")
	}
	if u.Attempts < 0 {
		return provider.Usage{}, fmt.Errorf("usage attempts cannot be negative")
	}
	if u.CallID != "" && s.providerCallIDs[u.CallID] {
		return provider.Usage{}, fmt.Errorf("provider call %q is already recorded", u.CallID)
	}
	total, err := s.Usage.CheckedAdd(u.Usage)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("provider usage accounting: %w", err)
	}
	if s.Calls == math.MaxInt {
		return provider.Usage{}, fmt.Errorf("provider call accounting overflow")
	}
	return total, nil
}

func (s *State) recordProviderCallID(id string) {
	if id == "" {
		return
	}
	if s.providerCallIDs == nil {
		s.providerCallIDs = make(map[string]bool)
	}
	s.providerCallIDs[id] = true
}

func (s *State) recordUsageTarget(target string) {
	if target == "" {
		return
	}
	for _, recorded := range s.UsageTargets {
		if recorded == target {
			return
		}
	}
	s.UsageTargets = append(s.UsageTargets, target)
}

type Session struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	seq      int
	poisoned error

	state State

	// liveUsages only holds calls appended since this Session handle was
	// opened. A UsageCursor can therefore name an exact live interval without
	// rereading the whole append-only log on every turn; a reopened handle
	// starts its first cursor after every replayed record.
	liveUsages []sequencedUsage
	// lastRouteUsageCursor prevents an accidentally reused window from
	// attributing the same durable provider call to two route records.
	lastRouteUsageCursor int

	// truncated counts bytes discarded by replay because the tail of the log was
	// unreadable. Non-zero means the user lost recorded work and must be told.
	truncated int64
}

type sequencedUsage struct {
	seq   int
	usage Usage
}

// UsageCursor is an opaque boundary in one live session handle. Route
// accounting uses it to correlate only provider calls durably appended after
// the turn began. Its fields stay private so callers cannot fabricate a
// sequence or reuse a cursor against another session.
type UsageCursor struct {
	sessionID string
	seq       int
}

// Create starts a session. catalogRevision pins the price and capability data
// in force, so a cost recorded in this log stays checkable against the data
// that produced it rather than whatever is current when it is read back.
func (s *Store) Create(workspace string, target provider.RouteTargetID, catalogRevision string) (*Session, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, workspaceKey(workspace))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".log")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating session file: %w", err)
	}
	if err := acquireLock(f); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(f, "%s %d\n", magic, SchemaVersion); err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}

	sess := &Session{
		f:    f,
		path: path,
		state: State{
			ID:              id,
			Workspace:       workspace,
			Target:          string(target),
			CatalogRevision: catalogRevision,
			CreatedAt:       time.Now().UTC(),
		},
	}
	err = sess.append(RecordSessionStart, SessionStart{
		ID:              id,
		Workspace:       workspace,
		Target:          string(target),
		Binary:          binaryVersion(),
		CatalogRevision: catalogRevision,
	})
	if err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}

// WorkspaceDir is the per-workspace directory the store keeps logs in,
// created if absent. Per-workspace state that is not a session log — the
// schedule ledger — lives beside the logs under the same key, so it follows
// the same machine-local placement rule DefaultStore states.
func (s *Store) WorkspaceDir(workspace string) (string, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, workspaceKey(workspace))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Open replays a session by ID and reopens it for appending.
func (s *Store) Open(id string) (*Session, error) {
	matches, err := filepath.Glob(filepath.Join(s.root, "*", id+".log"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return openPath(matches[0])
}

// Latest opens the most recently modified session for a workspace, which is
// what `sb --continue` resumes.
func (s *Store) Latest(workspace string) (*Session, error) {
	infos, err := s.List(workspace)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, ErrNoSessions
	}
	// A completed race keeps both fully accounted answers explicitly
	// resumable, but --continue must select the branch the user chose rather
	// than a later-touched alternative. If all records predate this marker,
	// fall back to the ordinary mtime ordering for compatibility.
	for _, info := range infos {
		state, stateErr := ReadState(info.Path)
		if stateErr == nil && !state.raceAlternative() {
			return openPath(info.Path)
		}
	}
	return openPath(infos[0].Path)
}

type Info struct {
	ID       string
	Path     string
	Modified time.Time
	Size     int64
}

// ListAll returns every workspace's sessions, keyed by the workspace path
// each log's own header records. The store's directories are content
// hashes, so the answer comes from the logs rather than from names that
// never held it; a directory whose logs are all unreadable is skipped,
// the same posture List takes per file.
func (s *Store) ListAll() (map[string][]Info, error) {
	dirs, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string][]Info{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
				continue
			}
			path := filepath.Join(s.root, d.Name(), e.Name())
			if !hasValidHeader(path) {
				continue
			}
			state, err := ReadState(path)
			if err != nil || state.RaceBranchPending() {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out[state.Workspace] = append(out[state.Workspace], Info{
				ID:       strings.TrimSuffix(e.Name(), ".log"),
				Path:     path,
				Modified: fi.ModTime(),
				Size:     fi.Size(),
			})
		}
	}
	return out, nil
}

// List returns a workspace's sessions, most recent first.
func (s *Store) List(workspace string) ([]Info, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, workspaceKey(workspace)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var infos []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		// A crash between creating the file and syncing its header leaves a stub
		// that cannot be replayed. Skipping it keeps `--continue` from resuming
		// into a parse failure on a session that never held anything.
		path := filepath.Join(s.root, workspaceKey(workspace), e.Name())
		if !hasValidHeader(path) {
			continue
		}
		state, err := ReadState(path)
		if err != nil || state.RaceBranchPending() {
			continue
		}
		infos = append(infos, Info{
			ID:       strings.TrimSuffix(e.Name(), ".log"),
			Path:     path,
			Modified: fi.ModTime(),
			Size:     fi.Size(),
		})
	}
	// Modification time first, then the id, because a filesystem can stamp two
	// files in the same tick and mtime alone would then leave `--continue`
	// resuming whichever one the directory happened to be read in. The id
	// tiebreak makes the answer stable; it does not make it right, because the
	// id only carries seconds, so two sessions started inside one second are
	// ordered by the random suffix rather than by which came first.
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].Modified.Equal(infos[j].Modified) {
			return infos[i].Modified.After(infos[j].Modified)
		}
		return infos[i].ID > infos[j].ID
	})
	return infos, nil
}

func hasValidHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	return err == nil && strings.HasPrefix(line, magic+" ")
}

func openPath(path string) (*Session, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := acquireLock(f); err != nil {
		f.Close()
		return nil, err
	}
	f, err = migrateForAppend(f, path)
	if err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}

	sess := &Session{f: f, path: path}
	if err := sess.replay(); err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	if sess.state.raceBranchPending {
		releaseLock(f)
		f.Close()
		return nil, fmt.Errorf("%w: origin session %s", ErrRaceBranchPending, sess.state.raceBranchOrigin)
	}
	return sess, nil
}

// migrateForAppend upgrades old readable logs before this process is allowed
// to append records whose execution semantics older binaries cannot preserve.
// Schemas 1 through 4 have same-width headers, so the version byte can be
// replaced in place while the file is exclusively locked. This works on
// Windows too, where replacing the path of an open, non-share-delete file is
// not permitted. Sync completes the upgrade
// before any schema-4 record may be appended.
func migrateForAppend(old *os.File, path string) (*os.File, error) {
	if _, err := old.Seek(0, io.SeekStart); err != nil {
		return old, err
	}
	r := bufio.NewReader(old)
	header, err := r.ReadString('\n')
	if err != nil {
		return old, fmt.Errorf("reading session header: %w", err)
	}
	var gotMagic string
	var version int
	if _, err := fmt.Sscanf(strings.TrimSpace(header), "%s %d", &gotMagic, &version); err != nil || gotMagic != magic {
		return old, fmt.Errorf("%s is not a switchboard session log", path)
	}
	if version > SchemaVersion {
		return old, fmt.Errorf("%w: log is schema %d, this binary understands %d", ErrSchemaTooNew, version, SchemaVersion)
	}
	if version == SchemaVersion {
		return old, nil
	}
	if version != 1 && version != 2 && version != 3 {
		return old, fmt.Errorf("cannot migrate session schema %d to %d", version, SchemaVersion)
	}
	oldHeader := []byte(header)
	newHeader := []byte(fmt.Sprintf("%s %d\n", magic, SchemaVersion))
	if len(oldHeader) != len(newHeader) {
		return old, fmt.Errorf("cannot migrate session schema header in place (%d bytes to %d)", len(oldHeader), len(newHeader))
	}
	n, err := old.WriteAt(newHeader, 0)
	if err == nil && n != len(newHeader) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return old, fmt.Errorf("writing session schema migration: %w", err)
	}
	if err := old.Sync(); err != nil {
		return old, fmt.Errorf("syncing session schema migration: %w", err)
	}
	if _, err := old.Seek(0, io.SeekStart); err != nil {
		return old, err
	}
	return old, nil
}

// replay folds the log into state, truncating at the first unreadable record.
func (s *Session) replay() error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(s.f)

	header, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading session header: %w", err)
	}
	var gotMagic string
	var version int
	if _, err := fmt.Sscanf(strings.TrimSpace(header), "%s %d", &gotMagic, &version); err != nil || gotMagic != magic {
		return fmt.Errorf("%s is not a switchboard session log", s.path)
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w: log is schema %d, this binary understands %d", ErrSchemaTooNew, version, SchemaVersion)
	}

	offset := int64(len(header))
	for {
		rec, consumed, err := decodeRecord(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, ErrCorruptRecord) {
			size, statErr := s.f.Seek(0, io.SeekEnd)
			if statErr != nil {
				return statErr
			}
			s.truncated = size - offset
			if err := s.f.Truncate(offset); err != nil {
				return fmt.Errorf("truncating corrupt log at %d: %w", offset, err)
			}
			break
		}
		if err != nil {
			return err
		}
		offset += int64(consumed)
		if err := s.apply(rec); err != nil {
			return err
		}
	}

	if _, err := s.f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	return nil
}

func (s *Session) apply(rec Record) error {
	s.seq = rec.Seq
	switch rec.Type {
	case RecordSessionStart:
		var p SessionStart
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		s.state.ID = p.ID
		s.state.Workspace = p.Workspace
		s.state.Target = p.Target
		s.state.CatalogRevision = p.CatalogRevision
		s.state.CreatedAt = rec.At
	case RecordMessage:
		var m provider.Message
		if err := json.Unmarshal(rec.Payload, &m); err != nil {
			return err
		}
		m = provider.CloneMessage(m)
		if m.ContinuityRef != "" {
			if err := validateContinuityDelivery(s.state, m); err != nil {
				return fmt.Errorf("message in record %d: %w", rec.Seq, err)
			}
			s.state.ContinuityRef = m.ContinuityRef
		}
		s.state.Messages = append(s.state.Messages, m)
	case RecordMessageContinuity:
		m, capsule, err := decodeMessageContinuity(rec.Payload)
		if err != nil {
			return fmt.Errorf("message-continuity in record %d: %w", rec.Seq, err)
		}
		if m.Role != provider.RoleTool || !hasSuccessfulTodoResult(m) {
			return fmt.Errorf("message-continuity in record %d does not hold a successful todo result", rec.Seq)
		}
		if m.ContinuityRef != "" {
			if err := validateContinuityDelivery(s.state, m); err != nil {
				return fmt.Errorf("message in record %d: %w", rec.Seq, err)
			}
		}
		if capsule.Source != continuity.SourceTodo || capsule.Cleared {
			return fmt.Errorf("message-continuity in record %d does not hold live todo state", rec.Seq)
		}
		if capsule.BasisMessages != len(s.state.Messages)+1 {
			return fmt.Errorf("continuity in record %d is based on %d messages, but the atomic result makes %d", rec.Seq, capsule.BasisMessages, len(s.state.Messages)+1)
		}
		// All validation precedes either state change: a malformed compound
		// frame cannot publish its message without its matching capsule.
		s.state.Messages = append(s.state.Messages, m)
		if m.ContinuityRef != "" {
			s.state.ContinuityRef = m.ContinuityRef
		}
		cloned := continuity.Clone(capsule)
		s.state.Continuity = &cloned
	case RecordUsage:
		var p Usage
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		usage, err := s.state.checkedUsage(p)
		if err != nil {
			return fmt.Errorf("usage in record %d: %w", rec.Seq, err)
		}
		local, external, err := s.state.checkedObservedCost(p.CostMicroUSD, 0)
		if err != nil {
			return fmt.Errorf("usage cost in record %d: %w", rec.Seq, err)
		}
		s.state.Usage = usage
		s.state.CostMicroUSD, s.state.ExternalCostMicroUSD = local, external
		s.state.Calls++
		s.state.recordProviderCallID(p.CallID)
		s.state.recordUsageTarget(p.Target)
	case RecordRetryReserve:
		var p RetryReserve
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		reserve, err := checkedMicroUSDAdd(s.state.RetryReserveMicroUSD, p.CostMicroUSD)
		if err != nil {
			return fmt.Errorf("retry reserve in record %d: %w", rec.Seq, err)
		}
		s.state.RetryReserveMicroUSD = reserve
	case RecordBudgetAttempt:
		var p BudgetAttempt
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if err := s.state.applyBudgetAttempt(p); err != nil {
			return fmt.Errorf("budget attempt in record %d: %w", rec.Seq, err)
		}
	case RecordBudgetSettle:
		var p BudgetSettlement
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if err := s.state.applyBudgetSettlement(p); err != nil {
			return fmt.Errorf("budget settlement in record %d: %w", rec.Seq, err)
		}
	case RecordBudgetTransfer:
		var p BudgetTransfer
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if err := s.state.applyBudgetTransfer(p); err != nil {
			return fmt.Errorf("budget transfer in record %d: %w", rec.Seq, err)
		}
	case RecordRaceBranch:
		var p RaceBranch
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if p.OriginID == "" {
			return fmt.Errorf("race branch in record %d has no origin", rec.Seq)
		}
		if s.state.raceBranchOrigin != "" && s.state.raceBranchOrigin != p.OriginID {
			return fmt.Errorf("race branch in record %d changes origin", rec.Seq)
		}
		s.state.raceBranchOrigin = p.OriginID
		s.state.raceBranchPending = !p.Finalized
		s.state.raceBranchFinalized = p.Finalized
		s.state.raceBranchContinuation = p.Finalized && p.Continuation
	case RecordRuntimeBinding:
		var p RuntimeBinding
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if p.Tier == "" || p.Target == "" {
			return fmt.Errorf("runtime binding in record %d has an empty tier or target", rec.Seq)
		}
		s.state.RuntimeBinding = p
	case RecordContinuity:
		p, err := continuity.DecodeStored(rec.Payload)
		if err != nil {
			return fmt.Errorf("continuity in record %d: %w", rec.Seq, err)
		}
		if p.BasisMessages != len(s.state.Messages) {
			return fmt.Errorf("continuity in record %d is based on %d messages, but the log held %d", rec.Seq, p.BasisMessages, len(s.state.Messages))
		}
		cloned := continuity.Clone(p)
		s.state.Continuity = &cloned
	case RecordPin:
		var p Pin
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		s.state.setPin(p)
	case RecordPermission, RecordNote, RecordRoute, RecordRace:
		// Recorded for audit and for §8.4's training signal; none of them carry
		// conversation state, so replay skips them without losing anything.
	default:
		// An unknown type from a same-schema log is forward-compatible padding,
		// not corruption. A newer schema is refused before replay reaches here.
	}
	return nil
}

func (s *Session) append(t RecordType, payload any) error {
	if s.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.seq++
	frame, err := encodeRecord(Record{
		Seq:     s.seq,
		At:      time.Now().UTC(),
		Type:    t,
		Payload: raw,
	})
	if err != nil {
		return err
	}
	if err := s.writeFrame(frame); err != nil {
		return err
	}
	// Records are few per turn, so paying for durability here is cheap and it is
	// what makes resume-after-interruption a guarantee rather than a hope.
	if err := s.f.Sync(); err != nil {
		s.poisoned = fmt.Errorf("syncing record %d: %w", s.seq, err)
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	return nil
}

func (s *Session) writeFrame(frame []byte) error {
	n, err := s.f.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.poisoned = fmt.Errorf("writing record %d (%d of %d bytes): %w", s.seq, n, len(frame), err)
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	return nil
}

func (s *Session) AppendMessage(m provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m = provider.CloneMessage(m)
	if m.ContinuityRef != "" {
		if err := validateContinuityDelivery(s.state, m); err != nil {
			return err
		}
	}
	if err := s.append(RecordMessage, m); err != nil {
		return err
	}
	s.state.Messages = append(s.state.Messages, m)
	if m.ContinuityRef != "" {
		s.state.ContinuityRef = m.ContinuityRef
	}
	return nil
}

// AppendToolResultsWithTasks commits one successful tool-result message and
// the exact todo continuity it produced in one checksummed, synced WAL frame.
// A torn frame replays neither half; a complete frame replays both.
func (s *Session) AppendToolResultsWithTasks(m provider.Message, tasks []continuity.Task) (continuity.Capsule, error) {
	return s.AppendToolResultsWithWorking(m, tasks, continuity.Working{})
}

// AppendToolResultsWithWorking is the same commit, carrying what the model said
// about the job alongside its list. The two are one WAL frame for the reason
// the tasks alone were: a crash between them would leave replay holding a
// successful tool result and an older belief about the work.
func (s *Session) AppendToolResultsWithWorking(m provider.Message, tasks []continuity.Task, working continuity.Working) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m = provider.CloneMessage(m)
	if m.Role != provider.RoleTool || !hasSuccessfulTodoResult(m) {
		return continuity.Capsule{}, fmt.Errorf("atomic todo result requires a tool message with a successful todo result")
	}
	if m.ContinuityRef != "" {
		return continuity.Capsule{}, fmt.Errorf("tool-result message cannot carry a continuity reference")
	}
	var current *continuity.Capsule
	if s.state.Continuity != nil {
		cloned := continuity.Clone(*s.state.Continuity)
		current = &cloned
	}
	next := continuity.WithWorking(current, tasks, working)
	next.BasisMessages = len(s.state.Messages) + 1
	prepared, err := continuity.Prepare(next)
	if err != nil {
		return continuity.Capsule{}, err
	}
	payload := messageContinuity{Message: m, Continuity: prepared}
	if err := s.append(RecordMessageContinuity, payload); err != nil {
		return continuity.Capsule{}, err
	}
	s.state.Messages = append(s.state.Messages, m)
	cloned := continuity.Clone(prepared)
	s.state.Continuity = &cloned
	return continuity.Clone(prepared), nil
}

func hasSuccessfulTodoResult(message provider.Message) bool {
	for _, block := range message.Content {
		result, ok := block.(provider.ToolResult)
		if ok && result.Name == "todo" && !result.IsError {
			return true
		}
	}
	return false
}

// StampContinuityOpening folds the one pending capsule into a complete user
// opening before routing or token estimation. The dedicated first text block
// and reference are an atomic delivery stamp: AppendMessage and replay accept
// the reference only while that exact capsule is current, undelivered, and
// rendered byte-for-byte in this message.
func (s *Session) StampContinuityOpening(opening provider.Message) (provider.Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	opening = provider.CloneMessage(opening)
	if !continuityOpening(opening) {
		return provider.Message{}, false, fmt.Errorf("continuity can only be stamped on a complete, non-injected user opening")
	}
	if opening.ContinuityRef != "" {
		if err := validateContinuityDelivery(s.state, opening); err != nil {
			return provider.Message{}, false, err
		}
		return opening, true, nil
	}
	current := s.state.Continuity
	if current == nil || current.Cleared || current.ID == s.state.ContinuityRef {
		return opening, false, nil
	}
	rendered, err := continuityDeliveryText(*current)
	if err != nil {
		return provider.Message{}, false, fmt.Errorf("render continuity opening: %w", err)
	}
	stamped := opening
	stamped.Content = make([]provider.Block, 0, len(opening.Content)+1)
	stamped.Content = append(stamped.Content, provider.Text{Text: rendered})
	stamped.Content = append(stamped.Content, opening.Content...)
	stamped.ContinuityRef = current.ID
	return stamped, true, nil
}

func validateContinuityDelivery(state State, message provider.Message) error {
	if !continuity.ValidID(message.ContinuityRef) {
		return fmt.Errorf("message has an invalid continuity reference")
	}
	if !continuityOpening(message) {
		return fmt.Errorf("continuity reference requires a complete, non-injected user opening")
	}
	if state.Continuity == nil || state.Continuity.Cleared || state.Continuity.ID != message.ContinuityRef {
		return fmt.Errorf("message refers to a continuity capsule that is not current")
	}
	if state.ContinuityRef == message.ContinuityRef {
		return fmt.Errorf("continuity capsule %s was already delivered", message.ContinuityRef)
	}
	rendered, err := continuityDeliveryText(*state.Continuity)
	if err != nil {
		return fmt.Errorf("render continuity delivery: %w", err)
	}
	if len(message.Content) == 0 {
		return fmt.Errorf("continuity reference has no rendered capsule block")
	}
	first, ok := message.Content[0].(provider.Text)
	if !ok || first.Text != rendered {
		return fmt.Errorf("continuity reference does not match the first rendered capsule block")
	}
	for _, block := range message.Content[1:] {
		if text, ok := block.(provider.Text); ok && text.Text == rendered {
			return fmt.Errorf("continuity reference duplicates the rendered capsule block")
		}
	}
	return nil
}

func continuityDeliveryText(c continuity.Capsule) (string, error) {
	rendered, err := continuity.Render(c)
	if err != nil {
		return "", err
	}
	// Ollama and chat-completions adapters flatten adjacent text blocks with
	// no delimiter. Carry the boundary inside the stamped block so both those
	// wires and block-preserving APIs keep capsule and prompt separated.
	return rendered + "\n\n", nil
}

func continuityOpening(message provider.Message) bool {
	return !message.Incomplete && OpensTurn(message)
}

// AppendContinuity redacts, bounds, identities, and durably appends the latest
// working-state capsule at the exact current conversation boundary. The caller
// receives the canonical stored value; it must not retain its pre-redaction
// input as though that were what replay will recover.
func (s *Session) AppendContinuity(c continuity.Capsule) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.BasisMessages = len(s.state.Messages)
	return s.appendContinuityLocked(c)
}

// AppendTasksContinuity atomically derives the todo-owned fields from the
// latest capsule and appends the result. Keeping the read/derive/write under
// one session lock prevents a concurrent clear or compaction capsule from
// being silently overwritten by a stale snapshot.
func (s *Session) AppendTasksContinuity(tasks []continuity.Task) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current *continuity.Capsule
	if s.state.Continuity != nil {
		cloned := continuity.Clone(*s.state.Continuity)
		current = &cloned
	}
	next := continuity.WithTasks(current, tasks)
	next.BasisMessages = len(s.state.Messages)
	return s.appendContinuityLocked(next)
}

func (s *Session) appendContinuityLocked(c continuity.Capsule) (continuity.Capsule, error) {
	prepared, err := continuity.Prepare(c)
	if err != nil {
		return continuity.Capsule{}, err
	}
	if err := s.append(RecordContinuity, prepared); err != nil {
		return continuity.Capsule{}, err
	}
	cloned := continuity.Clone(prepared)
	s.state.Continuity = &cloned
	return continuity.Clone(prepared), nil
}

// ClearContinuity appends a tombstone instead of deleting history. A fork
// before the tombstone can still recover the state that was current there;
// replay at or after it cannot accidentally revive that older capsule.
func (s *Session) ClearContinuity(source continuity.Source) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current *continuity.Capsule
	if s.state.Continuity != nil {
		copy := continuity.Clone(*s.state.Continuity)
		current = &copy
	}
	c := continuity.Tombstone(current, source)
	c.BasisMessages = len(s.state.Messages)
	if current != nil {
		c.ParentSession = s.state.ID
		c.ParentMessages = len(s.state.Messages)
		c.ParentCapsule = current.ID
	}
	return s.appendContinuityLocked(c)
}

func (s *Session) AppendUsage(u Usage) error {
	_, err := s.AppendUsageRecord(u)
	return err
}

// AppendUsageRecord appends one provider receipt and returns the exact stored
// value, including the Session-assigned durable CallID. Observers must receive
// this returned record rather than their pre-append copy or later telemetry
// cannot correlate the callback with the durable log.
func (s *Session) AppendUsageRecord(u Usage) (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.CallID == "" {
		u.CallID = fmt.Sprintf("call:%s:%d", s.state.ID, s.seq+1)
	}
	usage, err := s.state.checkedUsage(u)
	if err != nil {
		return Usage{}, err
	}
	local, external, err := s.state.checkedObservedCost(u.CostMicroUSD, 0)
	if err != nil {
		return Usage{}, err
	}
	if err := s.append(RecordUsage, u); err != nil {
		return Usage{}, err
	}
	s.state.Usage = usage
	s.state.CostMicroUSD, s.state.ExternalCostMicroUSD = local, external
	s.state.Calls++
	s.state.recordProviderCallID(u.CallID)
	s.state.recordUsageTarget(u.Target)
	s.liveUsages = append(s.liveUsages, sequencedUsage{seq: s.seq, usage: u})
	return u, nil
}

// AppendRetryReserve durably accounts for a failed provider attempt without
// pretending it returned successful usage.
func (s *Session) AppendRetryReserve(costMicroUSD int64) error {
	if costMicroUSD == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reserve, err := checkedMicroUSDAdd(s.state.RetryReserveMicroUSD, costMicroUSD)
	if err != nil {
		return err
	}
	if err := s.append(RecordRetryReserve, RetryReserve{CostMicroUSD: costMicroUSD}); err != nil {
		return err
	}
	s.state.RetryReserveMicroUSD = reserve
	return nil
}

// BeginBudgetAttempt writes and syncs the conservative bound before a provider
// request may be issued. The returned ID is the only token that can settle the
// attempt; losing it is safe because replay keeps the attempt pending.
func (s *Session) BeginBudgetAttempt(costMicroUSD int64) (string, error) {
	if costMicroUSD <= 0 {
		return "", fmt.Errorf("budget attempt cost must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("%s:%d", s.state.ID, s.seq+1)
	p := BudgetAttempt{ID: id, CostMicroUSD: costMicroUSD}
	if err := s.state.validateBudgetAttempt(p); err != nil {
		return "", err
	}
	if err := s.append(RecordBudgetAttempt, p); err != nil {
		return "", err
	}
	if err := s.state.applyBudgetAttempt(p); err != nil {
		return "", err
	}
	return id, nil
}

// SettleBudgetAttempt records the known outcome of a write-ahead attempt.
// externalCostMicroUSD is zero for a call whose Usage is in this log and the
// actual priced cost for a delegate/race call whose Usage lives elsewhere.
func (s *Session) SettleBudgetAttempt(attemptID, outcome string, externalCostMicroUSD int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := BudgetSettlement{AttemptID: attemptID, Outcome: outcome, ExternalCostMicroUSD: externalCostMicroUSD}
	if err := s.state.validateBudgetSettlement(p); err != nil {
		return err
	}
	if err := s.append(RecordBudgetSettle, p); err != nil {
		return err
	}
	return s.state.applyBudgetSettlement(p)
}

// AppendBudgetTransfer atomically attributes work from another authoritative
// ledger to this continuing session. Source must be stable for that transfer;
// duplicates are refused so a race verdict cannot silently double charge.
func (s *Session) AppendBudgetTransfer(source string, externalCostMicroUSD, retryReserveMicroUSD int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := BudgetTransfer{Source: source, ExternalCostMicroUSD: externalCostMicroUSD, RetryReserveMicroUSD: retryReserveMicroUSD}
	if err := s.state.validateBudgetTransfer(p); err != nil {
		return err
	}
	if err := s.append(RecordBudgetTransfer, p); err != nil {
		return err
	}
	return s.state.applyBudgetTransfer(p)
}

func (s *Session) MarkRaceBranchPending(originID string) error {
	if originID == "" {
		return fmt.Errorf("race branch origin is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.raceBranchOrigin != "" {
		return fmt.Errorf("session is already a race branch")
	}
	if err := s.append(RecordRaceBranch, RaceBranch{OriginID: originID}); err != nil {
		return err
	}
	s.state.raceBranchOrigin = originID
	s.state.raceBranchPending = true
	return nil
}

func (s *Session) FinalizeRaceBranch() error {
	return s.finalizeRaceBranch(true)
}

// FinalizeRaceBranchAlternative makes a fully reconciled branch explicitly
// resumable without letting --continue choose it over the user's verdict.
func (s *Session) FinalizeRaceBranchAlternative() error {
	return s.finalizeRaceBranch(false)
}

func (s *Session) finalizeRaceBranch(continuation bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.raceBranchOrigin == "" || !s.state.raceBranchPending {
		return fmt.Errorf("session is not a pending race branch")
	}
	if err := s.append(RecordRaceBranch, RaceBranch{OriginID: s.state.raceBranchOrigin, Finalized: true, Continuation: continuation}); err != nil {
		return err
	}
	s.state.raceBranchPending = false
	s.state.raceBranchFinalized = true
	s.state.raceBranchContinuation = continuation
	return nil
}

func (s State) RaceBranchPending() bool { return s.raceBranchPending }

func (s State) raceAlternative() bool {
	return s.raceBranchOrigin != "" && s.raceBranchFinalized && !s.raceBranchContinuation
}

func (s *State) validateBudgetAttempt(p BudgetAttempt) error {
	if p.ID == "" || p.CostMicroUSD <= 0 {
		return fmt.Errorf("invalid pending attempt")
	}
	if _, exists := s.pendingBudgetAttempts[p.ID]; exists {
		return fmt.Errorf("attempt %q is already pending", p.ID)
	}
	if _, err := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.CostMicroUSD); err != nil {
		return err
	}
	return nil
}

func (s *State) applyBudgetAttempt(p BudgetAttempt) error {
	if err := s.validateBudgetAttempt(p); err != nil {
		return err
	}
	if s.pendingBudgetAttempts == nil {
		s.pendingBudgetAttempts = make(map[string]int64)
	}
	s.pendingBudgetAttempts[p.ID] = p.CostMicroUSD
	reserve, _ := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.CostMicroUSD)
	s.RetryReserveMicroUSD = reserve
	return nil
}

func (s *State) validateBudgetSettlement(p BudgetSettlement) error {
	bound, exists := s.pendingBudgetAttempts[p.AttemptID]
	if p.AttemptID == "" || !exists || bound <= 0 {
		return fmt.Errorf("attempt %q is not pending", p.AttemptID)
	}
	if p.ExternalCostMicroUSD < 0 {
		return fmt.Errorf("external cost cannot be negative")
	}
	if _, _, err := s.checkedObservedCost(0, p.ExternalCostMicroUSD); err != nil {
		return err
	}
	switch p.Outcome {
	case BudgetOutcomeSucceeded:
		if s.RetryReserveMicroUSD < bound {
			return fmt.Errorf("pending attempt %q exceeds total retry reserve", p.AttemptID)
		}
		return nil
	case BudgetOutcomeFailed:
		if p.ExternalCostMicroUSD != 0 {
			return fmt.Errorf("a failed attempt cannot carry observed external cost")
		}
		return nil
	default:
		return fmt.Errorf("unknown budget outcome %q", p.Outcome)
	}
}

func (s *State) applyBudgetSettlement(p BudgetSettlement) error {
	if err := s.validateBudgetSettlement(p); err != nil {
		return err
	}
	bound := s.pendingBudgetAttempts[p.AttemptID]
	delete(s.pendingBudgetAttempts, p.AttemptID)
	if p.Outcome == BudgetOutcomeSucceeded {
		s.RetryReserveMicroUSD -= bound
	}
	local, external, _ := s.checkedObservedCost(0, p.ExternalCostMicroUSD)
	s.CostMicroUSD, s.ExternalCostMicroUSD = local, external
	return nil
}

func (s *State) validateBudgetTransfer(p BudgetTransfer) error {
	if p.Source == "" {
		return fmt.Errorf("budget transfer source is required")
	}
	if p.ExternalCostMicroUSD < 0 || p.RetryReserveMicroUSD < 0 {
		return fmt.Errorf("budget transfer amounts cannot be negative")
	}
	if _, _, err := s.checkedObservedCost(0, p.ExternalCostMicroUSD); err != nil {
		return err
	}
	if _, err := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.RetryReserveMicroUSD); err != nil {
		return err
	}
	if s.appliedBudgetTransfers[p.Source] {
		return fmt.Errorf("budget transfer %q was already applied", p.Source)
	}
	return nil
}

func (s *State) applyBudgetTransfer(p BudgetTransfer) error {
	if err := s.validateBudgetTransfer(p); err != nil {
		return err
	}
	if s.appliedBudgetTransfers == nil {
		s.appliedBudgetTransfers = make(map[string]bool)
	}
	s.appliedBudgetTransfers[p.Source] = true
	local, external, _ := s.checkedObservedCost(0, p.ExternalCostMicroUSD)
	reserve, _ := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.RetryReserveMicroUSD)
	s.CostMicroUSD, s.ExternalCostMicroUSD = local, external
	s.RetryReserveMicroUSD = reserve
	return nil
}

// BeginUsageWindow snapshots the durable sequence immediately before a routed
// turn. The returned cursor is bound to this session and can only be consumed
// by AppendRouteWithUsage.
func (s *Session) BeginUsageWindow() UsageCursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return UsageCursor{sessionID: s.state.ID, seq: s.seq}
}

// AppendRouteWithUsage fills a route's accounting from the exact durable
// purpose=turn calls appended since cursor, then appends the route atomically
// with respect to every other session writer. This avoids ledger subtraction:
// concurrent background usage and retry-reserve settlement cannot enter the
// route, and a decreasing or saturated aggregate can never yield a negative
// delta.
func (s *Session) AppendRouteWithUsage(cursor UsageCursor, r Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor.sessionID == "" || cursor.sessionID != s.state.ID || cursor.seq < 0 || cursor.seq > s.seq {
		return errors.New("route usage cursor does not belong to this session boundary")
	}
	if cursor.seq <= s.lastRouteUsageCursor {
		return errors.New("route usage cursor was already consumed")
	}
	var usage provider.Usage
	var cost int64
	var callIDs []string
	seen := make(map[string]bool)
	for _, record := range s.liveUsages {
		if record.seq <= cursor.seq || record.usage.EffectivePurpose() != UsagePurposeTurn {
			continue
		}
		if record.usage.CallID == "" || seen[record.usage.CallID] {
			return errors.New("route usage has no unique durable call identity")
		}
		seen[record.usage.CallID] = true
		var err error
		usage, err = usage.CheckedAdd(record.usage.Usage)
		if err != nil {
			return fmt.Errorf("route usage accounting: %w", err)
		}
		cost, err = checkedMicroUSDAdd(cost, record.usage.CostMicroUSD)
		if err != nil {
			return fmt.Errorf("route cost accounting: %w", err)
		}
		callIDs = append(callIDs, record.usage.CallID)
	}
	r.Usage = usage
	r.CostMicroUSD = cost
	r.UsageCallIDs = callIDs
	if err := s.appendRouteLocked(r); err != nil {
		return err
	}
	s.lastRouteUsageCursor = cursor.seq
	return nil
}

// AppendRoute records §8.4's training signal for one turn. Callers that own a
// live model turn should use AppendRouteWithUsage so its accounting is bound
// to durable provider-call identities.
func (s *Session) AppendRoute(r Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendRouteLocked(r)
}

func (s *Session) appendRouteLocked(r Route) error {
	if r.VerificationStatus == "" {
		switch {
		case r.Verified:
			// Older callers only had Verified; a verified result necessarily ran.
			r.VerificationRan = true
			r.VerificationStatus = RouteVerificationPassed
		case r.VerificationRan:
			r.VerificationStatus = RouteVerificationFailed
		default:
			r.VerificationStatus = RouteVerificationUnavailable
		}
	}
	if r.FailureKind != "" && !validRouteFailureKind(r.FailureKind) {
		return fmt.Errorf("unknown route failure kind %q", r.FailureKind)
	}
	return s.append(RecordRoute, r)
}

func validRouteFailureKind(kind string) bool {
	switch kind {
	case RouteFailureProvider, RouteFailureBudget, RouteFailureContext, RouteFailureRoundLimit,
		RouteFailureVerification, RouteFailureCancelled, RouteFailureInternal:
		return true
	default:
		return false
	}
}

// AppendRace records a paired trial's verdict. It lands on the session that
// continues — the picked branch, or the pre-race session when the race was
// abandoned — so the record travels with the history it judged.
func (s *Session) AppendRace(r Race) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(RecordRace, r)
}

func (s *Session) AppendPermission(p Permission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(RecordPermission, p)
}

func (s *Session) AppendNote(level, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(RecordNote, Note{Level: level, Text: text})
}

// AppendRuntimeBinding durably commits the tier, exact parameterized target,
// and pin posture that a reopened process must reconstruct. Callers append it
// only for permanent manual actions or committed automatic moves; temporary
// one-turn and process-only inference-parameter overrides deliberately do not.
func (s *Session) AppendRuntimeBinding(tier string, target provider.RouteTargetID, pinned bool) error {
	if tier == "" || target == "" {
		return fmt.Errorf("runtime binding requires a tier and target")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := RuntimeBinding{Tier: tier, Target: target, Pinned: pinned}
	if p == s.state.RuntimeBinding {
		return nil
	}
	if err := s.append(RecordRuntimeBinding, p); err != nil {
		return err
	}
	s.state.RuntimeBinding = p
	return nil
}

// AppendPin marks the current point in the conversation under a name. The
// count is taken here, under the lock, so the pin means "everything the log
// held when the user said so" whatever arrives next.
func (s *Session) AppendPin(name string) (Pin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := Pin{Name: name, Messages: len(s.state.Messages)}
	if err := s.append(RecordPin, p); err != nil {
		return Pin{}, err
	}
	s.state.setPin(p)
	return p, nil
}

// setPin keeps one pin per name: setting a name again moves it, because two
// cut points under one name would make /fork's answer depend on which the
// reader found first.
func (st *State) setPin(p Pin) {
	for i, have := range st.Pins {
		if have.Name == p.Name {
			st.Pins[i] = p
			return
		}
	}
	st.Pins = append(st.Pins, p)
}

// Pin resolves a name to its recorded point.
func (st State) Pin(name string) (Pin, bool) {
	for _, p := range st.Pins {
		if p.Name == name {
			return p, true
		}
	}
	return Pin{}, false
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state
	out.Messages = provider.CloneMessages(s.state.Messages)
	out.Pins = append([]Pin(nil), s.state.Pins...)
	out.UsageTargets = append([]string(nil), s.state.UsageTargets...)
	if s.state.Continuity != nil {
		copy := continuity.Clone(*s.state.Continuity)
		out.Continuity = &copy
	}
	if s.state.pendingBudgetAttempts != nil {
		out.pendingBudgetAttempts = make(map[string]int64, len(s.state.pendingBudgetAttempts))
		for id, amount := range s.state.pendingBudgetAttempts {
			out.pendingBudgetAttempts[id] = amount
		}
	}
	if s.state.appliedBudgetTransfers != nil {
		out.appliedBudgetTransfers = make(map[string]bool, len(s.state.appliedBudgetTransfers))
		for source, applied := range s.state.appliedBudgetTransfers {
			out.appliedBudgetTransfers[source] = applied
		}
	}
	if s.state.providerCallIDs != nil {
		out.providerCallIDs = make(map[string]bool, len(s.state.providerCallIDs))
		for id, recorded := range s.state.providerCallIDs {
			out.providerCallIDs[id] = recorded
		}
	}
	return out
}

// CurrentContinuity returns an isolated snapshot of the latest capsule without
// copying the entire conversation. Session binding and post-tool persistence
// use this narrow projection on potentially long-running sessions.
func (s *Session) CurrentContinuity() *continuity.Capsule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Continuity == nil {
		return nil
	}
	copy := continuity.Clone(*s.state.Continuity)
	return &copy
}

func (s *Session) ID() string   { return s.state.ID }
func (s *Session) Path() string { return s.path }

// TruncatedBytes is how much of the log replay could not read. Non-zero means
// recorded work was lost and the user is owed that fact.
func (s *Session) TruncatedBytes() int64 { return s.truncated }

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	releaseLock(s.f)
	return s.f.Close()
}

func workspaceKey(abs string) string {
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:8])
}

// newID sorts lexically by creation time, which makes a directory listing
// chronological without reading any file.
func newID() (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	// Microseconds, not seconds. The id is what orders a directory of sessions
	// when the filesystem stamps two of them in the same tick, and a
	// second-resolution id carries no ordering information at exactly the
	// moment ordering is needed. The random suffix keeps two sessions started
	// in the same microsecond from colliding; it is not what orders them.
	return time.Now().UTC().Format("20060102T150405.000000") + "-" + hex.EncodeToString(suffix[:]), nil
}

// binaryVersion records what produced a session so a historical decision can be
// reconstructed against the code that made it.
func binaryVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	switch {
	case revision == "":
		return info.Main.Version
	case modified == "true":
		return revision[:min(12, len(revision))] + "-dirty"
	default:
		return revision[:min(12, len(revision))]
	}
}
