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
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cj-vana/switchboard/internal/provider"
)

// ErrSessionLocked reports that another process is appending to this session.
// Two writers would interleave frames and corrupt the log.
var ErrSessionLocked = errors.New("session is open in another process")

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

	// Messages includes assistant messages marked Incomplete. They are kept for
	// diagnosis and display; adapters drop them when rendering a request so an
	// interrupted turn is never replayed as a finished one (§10.3).
	Messages []provider.Message

	Usage provider.Usage
	Calls int

	// CostMicroUSD totals what the catalog priced this session at. It is an
	// estimate and a reconciliation aid, never a substitute for the provider's
	// invoice (§15).
	CostMicroUSD int64

	// Pins are the named points /fork can cut back to, in the order set,
	// with a re-used name moving its pin rather than stacking a second.
	Pins []Pin

	// CatalogRevision is the revision the session started against.
	CatalogRevision string
}

type Session struct {
	mu   sync.Mutex
	f    *os.File
	path string
	seq  int

	state State

	// truncated counts bytes discarded by replay because the tail of the log was
	// unreadable. Non-zero means the user lost recorded work and must be told.
	truncated int64
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
			ws, err := ReadWorkspace(path)
			if err != nil {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out[ws] = append(out[ws], Info{
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
		if !hasValidHeader(filepath.Join(s.root, workspaceKey(workspace), e.Name())) {
			continue
		}
		infos = append(infos, Info{
			ID:       strings.TrimSuffix(e.Name(), ".log"),
			Path:     filepath.Join(s.root, workspaceKey(workspace), e.Name()),
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

	sess := &Session{f: f, path: path}
	if err := sess.replay(); err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	return sess, nil
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
		s.state.Messages = append(s.state.Messages, m)
	case RecordUsage:
		var p Usage
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		s.state.Usage = s.state.Usage.Add(p.Usage)
		s.state.CostMicroUSD += p.CostMicroUSD
		s.state.Calls++
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
	if _, err := s.f.Write(frame); err != nil {
		return fmt.Errorf("appending to session log: %w", err)
	}
	// Records are few per turn, so paying for durability here is cheap and it is
	// what makes resume-after-interruption a guarantee rather than a hope.
	return s.f.Sync()
}

func (s *Session) AppendMessage(m provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.append(RecordMessage, m); err != nil {
		return err
	}
	s.state.Messages = append(s.state.Messages, m)
	return nil
}

func (s *Session) AppendUsage(u Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.append(RecordUsage, u); err != nil {
		return err
	}
	s.state.Usage = s.state.Usage.Add(u.Usage)
	s.state.CostMicroUSD += u.CostMicroUSD
	s.state.Calls++
	return nil
}

// AppendRoute records §8.4's training signal for one turn.
func (s *Session) AppendRoute(r Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(RecordRoute, r)
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
	out.Messages = append([]provider.Message(nil), s.state.Messages...)
	out.Pins = append([]Pin(nil), s.state.Pins...)
	return out
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
