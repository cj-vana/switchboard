// Package prefix lays out a request so that the part a provider can cache stays
// byte-identical from one turn to the next.
//
// §6.1 describes four ordered zones and calls the layout "not clever, but
// discipline, and worth more than any of the optimizations below". Discipline
// that lives in a document gets violated by the next person in a hurry, so the
// zones are types here and the rules are the only operations they offer: the
// frozen zone has no setter, the stable zone refuses writes once a session is
// under way, history appends and nothing else, and only the tail can be
// rewritten.
//
// The failure this prevents is specific. Inserting a block anywhere above the
// tail shifts every block after it, so the provider's cached prefix stops
// matching at the insertion point and the whole remainder is re-read at full
// price. Most harnesses do this to themselves by appending retrieved context
// into the middle of the conversation.
package prefix

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cjvana/switchboard/internal/provider"
)

// Zone names a region of the request. The order is the order they are sent in,
// which is also increasing order of how often they change.
type Zone int

const (
	// Frozen holds the system prompt, the tool definitions, and project
	// instructions. It is fixed for the life of a session.
	Frozen Zone = iota

	// Stable holds file contents and retrieved documents confirmed unchanged by
	// content hash. It is populated at session start or at a scheduled rebuild,
	// never in between.
	Stable

	// History holds conversation turns, tool calls, and tool results. It only
	// grows.
	History

	// Tail holds the current user message and any per-request state. It is the
	// only zone that may be rewritten.
	Tail
)

func (z Zone) String() string {
	switch z {
	case Frozen:
		return "frozen"
	case Stable:
		return "stable"
	case History:
		return "history"
	case Tail:
		return "tail"
	}
	return "unknown"
}

// ErrSealed reports an attempt to write to a zone that is closed for the
// session. It is a programming error rather than a user error: the caller
// should have scheduled a rebuild.
var ErrSealed = errors.New("the stable zone is sealed until the next rebuild")

// Document is a file or retrieved text placed in the stable zone.
//
// Hash is over the content, and it is the whole basis for the zone: a document
// belongs here only while it is known unchanged. When it changes, a new history
// block supersedes it rather than the stable entry being edited, because
// editing would move every block after it.
type Document struct {
	Path    string
	Hash    string
	Content string

	// Pinned documents survive eviction. AGENTS.md and anything the user named
	// explicitly are pinned, because evicting the instructions that shape the
	// session to make room for a file read once is the wrong trade.
	Pinned bool

	// lastReferenced orders eviction. It is a counter rather than a clock so
	// that layout decisions are reproducible from a session log.
	lastReferenced uint64
}

// Layout assembles a request from the four zones.
//
// It is not safe for concurrent use; the agent loop drives one turn at a time.
type Layout struct {
	system []provider.Block
	tools  []provider.ToolDefinition

	documents []*Document
	sealed    bool

	history []provider.Message
	tail    []provider.Block

	// budget caps the stable zone in tokens. Crossing it schedules a rebuild
	// rather than truncating: silently dropping a document the model was told
	// it had is worse than paying to rebuild the prefix deliberately.
	budget int

	clock uint64
}

// New builds a layout. The frozen zone is supplied once and has no setter,
// which is the mechanism §6.1 relies on: dynamic operator context goes in the
// tail, never by editing what has already been cached.
//
// Tool definitions are sorted by name. Two sessions that registered the same
// tools in a different order would otherwise produce different prefixes and
// share no cache, for no reason a user could see.
func New(system []provider.Block, tools []provider.ToolDefinition, stableBudget int) *Layout {
	sorted := append([]provider.ToolDefinition(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	return &Layout{
		system: append([]provider.Block(nil), system...),
		tools:  sorted,
		budget: stableBudget,
	}
}

// Add places a document in the stable zone. It is refused once the zone is
// sealed, which happens as soon as the session starts producing history.
func (l *Layout) Add(doc Document) error {
	if l.sealed {
		return fmt.Errorf("%w: %s must go in the history zone or wait for a rebuild", ErrSealed, doc.Path)
	}
	l.clock++
	doc.lastReferenced = l.clock

	for i, existing := range l.documents {
		if existing.Path != doc.Path {
			continue
		}
		if existing.Hash == doc.Hash {
			// Already present and unchanged. Touching it keeps eviction honest
			// without moving it, because moving it would shift everything after.
			l.documents[i].lastReferenced = doc.lastReferenced
			return nil
		}
		// Replacing in place before the session starts is safe: nothing has
		// been cached yet.
		l.documents[i] = &doc
		return nil
	}
	l.documents = append(l.documents, &doc)
	return nil
}

// Touch records that a document was referenced, which is what keeps a
// frequently used file from being evicted at the next rebuild. It does not move
// anything.
func (l *Layout) Touch(path string) {
	for _, doc := range l.documents {
		if doc.Path == path {
			l.clock++
			doc.lastReferenced = l.clock
			return
		}
	}
}

// Seal closes the stable zone. Everything after this point appends to history.
func (l *Layout) Seal() { l.sealed = true }

func (l *Layout) Sealed() bool { return l.sealed }

// AppendHistory adds a turn. Sealing on the first append is what makes the
// rule automatic rather than something a caller has to remember.
func (l *Layout) AppendHistory(msgs ...provider.Message) {
	if len(msgs) > 0 {
		l.sealed = true
	}
	l.history = append(l.history, msgs...)
}

func (l *Layout) History() []provider.Message {
	return append([]provider.Message(nil), l.history...)
}

// SetTail replaces the volatile tail. This is the only rewrite the layout
// allows, and it is where mode changes, remaining budget, and the current
// instruction belong.
func (l *Layout) SetTail(blocks ...provider.Block) {
	l.tail = append([]provider.Block(nil), blocks...)
}

// Documents returns the stable zone in send order.
func (l *Layout) Documents() []Document {
	out := make([]Document, 0, len(l.documents))
	for _, doc := range l.documents {
		out = append(out, *doc)
	}
	return out
}

// StableTokens estimates the stable zone's size. It is characters over four,
// the same crude estimate the local adapters use and with the same measured
// bias (docs/estimator.md), which is acceptable here because it decides when to
// schedule a rebuild rather than what anything costs.
func (l *Layout) StableTokens() int {
	total := 0
	for _, doc := range l.documents {
		total += tokensOf(doc)
	}
	return total
}

// NeedsRebuild reports whether the stable zone has outgrown its budget.
//
// The answer is advisory. Crossing the budget schedules a rebuild; it does not
// truncate, because dropping a document mid-session both invalidates the cache
// and leaves the model referring to content that is no longer there.
func (l *Layout) NeedsRebuild() bool {
	return l.budget > 0 && l.StableTokens() > l.budget
}

// Rebuild opens the stable zone again and evicts down to the budget.
//
// This is a cache-invalidating event by definition, which is why it is an
// explicit call rather than something that happens when a threshold is crossed.
// Eviction is least-recently-referenced, and pinned documents are exempt.
func (l *Layout) Rebuild() (evicted []string) {
	l.sealed = false
	if l.budget <= 0 {
		return nil
	}

	// Oldest reference first, so the survivors are what the session actually
	// keeps using.
	order := make([]*Document, len(l.documents))
	copy(order, l.documents)
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].lastReferenced < order[j].lastReferenced
	})

	remaining := l.StableTokens()
	drop := map[string]bool{}
	for _, doc := range order {
		if remaining <= l.budget {
			break
		}
		if doc.Pinned {
			continue
		}
		drop[doc.Path] = true
		remaining -= tokensOf(doc)
		evicted = append(evicted, doc.Path)
	}
	if len(drop) == 0 {
		return nil
	}

	kept := l.documents[:0]
	for _, doc := range l.documents {
		if !drop[doc.Path] {
			kept = append(kept, doc)
		}
	}
	l.documents = kept
	return evicted
}

func tokensOf(doc *Document) int {
	return (len(doc.Path) + len(doc.Content)) / 4
}

// Request assembles the canonical request.
//
// The stable zone is rendered as a single user message followed by an assistant
// acknowledgement, rather than as system blocks, so that adding a document
// cannot alter the system prompt a target has already cached.
func (l *Layout) Request() provider.Request {
	messages := make([]provider.Message, 0, len(l.history)+3)

	if len(l.documents) > 0 {
		var b strings.Builder
		b.WriteString("Files already read for this session. " +
			"Each is current as of the hash shown; if one changes, a later message supersedes it.\n")
		for _, doc := range l.documents {
			fmt.Fprintf(&b, "\n<file path=%q sha256=%q>\n%s\n</file>\n", doc.Path, doc.Hash, doc.Content)
		}
		messages = append(messages,
			provider.Message{Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: b.String()}}},
			provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
				provider.Text{Text: "Noted. I will use those contents unless a later message supersedes them."},
			}},
		)
	}

	messages = append(messages, l.history...)
	if len(l.tail) > 0 {
		messages = append(messages, provider.Message{Role: provider.RoleUser, Content: l.tail})
	}

	return provider.Request{
		System:   append([]provider.Block(nil), l.system...),
		Tools:    append([]provider.ToolDefinition(nil), l.tools...),
		Messages: messages,
	}
}

// PrefixHash fingerprints everything above the tail.
//
// The cache tracker keys on this: a prefix that hashes the same is one the
// provider could still be holding, and one that hashes differently cannot be,
// whatever either side believes. The tail is excluded because it is expected to
// change every turn and including it would make every prefix look new.
func (l *Layout) PrefixHash() string {
	h := sha256.New()

	writeBlocks(h, l.system)
	for _, t := range l.tools {
		fmt.Fprintf(h, "tool\x00%s\x00%s\x00%s\x00", t.Name, t.Description, t.Schema)
	}
	for _, doc := range l.documents {
		fmt.Fprintf(h, "doc\x00%s\x00%s\x00", doc.Path, doc.Hash)
	}
	for _, m := range l.history {
		fmt.Fprintf(h, "msg\x00%s\x00", m.Role)
		writeBlocks(h, m.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeBlocks(h interface{ Write([]byte) (int, error) }, blocks []provider.Block) {
	for _, b := range blocks {
		switch v := b.(type) {
		case provider.Text:
			fmt.Fprintf(h, "text\x00%s\x00", v.Text)
		case provider.Thinking:
			// The signature is included: replacing a thinking block with an
			// unsigned copy changes what will be sent, so it changes the prefix.
			fmt.Fprintf(h, "thinking\x00%s\x00%s\x00", v.Text, v.Signature)
		case provider.ToolUse:
			fmt.Fprintf(h, "tool_use\x00%s\x00%s\x00%s\x00", v.ID, v.Name, v.Input)
		case provider.ToolResult:
			fmt.Fprintf(h, "tool_result\x00%s\x00%s\x00", v.ToolUseID, v.Content)
		default:
			fmt.Fprintf(h, "%s\x00", b.Kind())
		}
	}
}
