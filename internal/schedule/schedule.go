// Package schedule is the per-workspace ledger behind /every, /at, and
// /schedule: prompts that fire as ordinary user turns at a local clock time
// or on an interval.
//
// The ledger is deliberately not a daemon. Nothing fires while sb is not
// running, and an entry whose moment passed while the process was down fires
// once at the next startup: a recurring entry does not catch up the ticks it
// missed, it fires once and reschedules from now. One process at a time owns
// the file the way the session logs beside it are owned, and the entries
// persist per workspace, under the session store's per-workspace directory,
// so a reminder never follows the user into a different checkout.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// FileName is the ledger's name inside the per-workspace directory.
const FileName = "schedule.json"

// lockName is the sidecar the process holds an advisory lock on for its
// life. The lock cannot ride the ledger itself: saves are atomic renames,
// and a lock on the renamed-away inode would guard nothing.
const lockName = "schedule.lock"

// ErrLocked says another running sb process owns this workspace's ledger.
// One writer is what "fires once" rests on: two processes polling the same
// file would each fire the same entry.
var ErrLocked = errors.New("the schedule ledger is held by another sb process in this workspace")

// MaxEntries caps the ledger. A reminder list that grows without a bound is
// a todo file wearing the wrong command, and a bound the user can see is what
// keeps /schedule's listing a listing rather than a search problem.
const MaxEntries = 32

// MinEvery is the shortest interval a recurring entry takes. Anything tighter
// is a loop with a model in it, and the command surface is where that is
// refused rather than discovered as a bill.
const MinEvery = time.Minute

// Entry is one armed prompt. Every > 0 makes it recurring; At is a local
// wall-clock "15:04" and makes it one-shot. NextFire is the next instant the
// entry is due, recomputed at arm time and after each fire, so the ledger
// carries its own schedule and no clock math survives a restart.
type Entry struct {
	ID       string        `json:"id"`
	Every    time.Duration `json:"every,omitempty"`
	At       string        `json:"at,omitempty"`
	Prompt   string        `json:"prompt"`
	Created  time.Time     `json:"created"`
	NextFire time.Time     `json:"next_fire"`
}

// Recurring reports the kind the entry's fire behavior follows.
func (e Entry) Recurring() bool { return e.Every > 0 }

// Store is the persisted ledger. The zero value is unusable; open one with
// Open.
type Store struct {
	path string
	lock *os.File

	mu      sync.Mutex
	entries []Entry
}

// Open loads the ledger in dir and takes the workspace's advisory lock on
// it, held until Close or process exit; a second opener gets ErrLocked. A
// missing file is an empty ledger, because a workspace that never armed a
// reminder has no file. An unreadable or corrupt file is an error and is
// left exactly as found: wiping it would delete reminders on the strength of
// a parse failure, which is the wrong direction for data the user wrote a
// command to create.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName)}
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the schedule lock: %w", err)
	}
	if err := acquireLock(lock); err != nil {
		_ = lock.Close()
		return nil, err
	}
	s.lock = lock

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		s.Close()
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	if err := json.Unmarshal(raw, &s.entries); err != nil {
		s.Close()
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	return s, nil
}

// Close releases the ledger's lock. The kernel would do it at process exit;
// this is the tidy path.
func (s *Store) Close() {
	if s.lock == nil {
		return
	}
	_ = releaseLock(s.lock)
	_ = s.lock.Close()
	s.lock = nil
}

// save writes the ledger atomically: a crash mid-write must leave the old
// file whole, because the alternative is losing every armed reminder to a
// power cut at the wrong second.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), FileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Add arms an entry: it validates the shape, assigns the lowest free short
// id, computes the first fire from now, and persists. The validation lives
// here and not only in the command surface because the file can also be
// edited by hand, and a malformed entry that fired is worse than a refused
// one.
func (s *Store) Add(e Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= MaxEntries {
		return Entry{}, fmt.Errorf("the schedule holds at most %d entries; /schedule cancel one first", MaxEntries)
	}
	if e.Prompt == "" {
		return Entry{}, fmt.Errorf("a scheduled entry needs a prompt")
	}
	now := time.Now()
	e.Created = now.UTC()
	switch {
	case e.Every > 0 && e.At != "":
		return Entry{}, fmt.Errorf("an entry is recurring or one-shot, not both")
	case e.Every > 0:
		if e.Every < MinEvery {
			return Entry{}, fmt.Errorf("the shortest interval is %s", MinEvery)
		}
		e.NextFire = now.Add(e.Every)
	case e.At != "":
		next, err := nextAt(now, e.At)
		if err != nil {
			return Entry{}, err
		}
		// Normalize to the canonical clock form, so "7:05" and "07:05" are the
		// same entry rather than two spellings the listing renders differently.
		e.At = next.Format("15:04")
		e.NextFire = next
	default:
		return Entry{}, fmt.Errorf("an entry needs an interval or a clock time")
	}
	e.ID = s.nextID()
	s.entries = append(s.entries, e)
	if err := s.save(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		return Entry{}, err
	}
	return e, nil
}

// nextID assigns the lowest free short id. Cancelled ids are reused, so the
// listing's numbers stay small enough to type into /schedule cancel.
func (s *Store) nextID() string {
	used := make(map[string]bool, len(s.entries))
	for _, e := range s.entries {
		used[e.ID] = true
	}
	for n := 1; ; n++ {
		id := "s" + strconv.Itoa(n)
		if !used[id] {
			return id
		}
	}
}

// nextAt resolves a wall clock to an instant: today at that time when it is
// still in the future in local time, tomorrow otherwise. The wall clock is
// the promise — "14:30" means 14:30 local — so the date arithmetic is
// calendar arithmetic and a daylight-saving jump moves the instant, not the
// clock reading.
func nextAt(now time.Time, clock string) (time.Time, error) {
	parsed, err := time.Parse("15:04", clock)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a 24-hour clock time like 14:30", clock)
	}
	local := now.Local()
	next := time.Date(local.Year(), local.Month(), local.Day(), parsed.Hour(), parsed.Minute(), 0, 0, time.Local)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

// List returns the ledger, soonest fire first, as a copy the caller may hold.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Entry(nil), s.entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].NextFire.Before(out[j].NextFire) })
	return out
}

// Cancel removes an entry and reports whether it existed. The id names itself
// in the refusal rather than being corrected, because the user typed it.
func (s *Store) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if e.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			// A save failure here leaves the cancellation in memory only: the
			// entry returns on the next run. That is the safe direction — a
			// reminder that fires once more is recoverable, a silently lost
			// save error is not visible at all.
			_ = s.save()
			return true
		}
	}
	return false
}

// TakeDue returns entries due at now — at most max of them, soonest first,
// unless max is not positive — and, in the same locked step, advances the
// ledger past them: a recurring entry's next fire becomes now+Every and a
// one-shot is removed. Advancing before returning is what makes "fires once"
// a property rather than a hope — a crash after the caller fires but before
// the next save can repeat an entry, so the save happens here, before the
// caller has fired anything. Missed intervals are never made up: an entry
// five ticks overdue fires once, not five times. A save failure is returned
// with the entries: they are safe to fire, but the ledger on disk still
// holds them, and the caller is the one who can say so.
func (s *Store) TakeDue(now time.Time, max int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Soonest first, so a limited drain takes the most overdue entry rather
	// than the one that happens to be armed earliest.
	sort.SliceStable(s.entries, func(i, j int) bool { return s.entries[i].NextFire.Before(s.entries[j].NextFire) })
	var due []Entry
	kept := s.entries[:0]
	changed := false
	for _, e := range s.entries {
		if max > 0 && len(due) >= max {
			kept = append(kept, e)
			continue
		}
		if !e.NextFire.After(now) {
			due = append(due, e)
			changed = true
			if e.Recurring() {
				e.NextFire = now.Add(e.Every)
				kept = append(kept, e)
			}
			continue
		}
		kept = append(kept, e)
	}
	s.entries = kept
	if !changed {
		return nil, nil
	}
	if err := s.save(); err != nil {
		return due, err
	}
	return due, nil
}
