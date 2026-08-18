package session

import (
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestPinSurvivesReopen(t *testing.T) {
	store, sess := forkFixture(t)

	p, err := sess.AppendPin("before-tools")
	if err != nil {
		t.Fatal(err)
	}
	if p.Messages != len(sess.State().Messages) {
		t.Fatalf("the pin's count is not the session's: %d", p.Messages)
	}
	id := sess.ID()
	sess.Close()

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, ok := reopened.State().Pin("before-tools")
	if !ok || got.Messages != p.Messages {
		t.Fatalf("the pin did not survive replay: %+v ok=%v", got, ok)
	}
}

func TestAReusedPinNameMovesRatherThanStacks(t *testing.T) {
	_, sess := forkFixture(t)

	first, err := sess.AppendPin("here")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("third question")); err != nil {
		t.Fatal(err)
	}
	second, err := sess.AppendPin("here")
	if err != nil {
		t.Fatal(err)
	}
	if second.Messages == first.Messages {
		t.Fatal("the moved pin did not move")
	}

	state := sess.State()
	if len(state.Pins) != 1 {
		t.Fatalf("one name produced %d pins", len(state.Pins))
	}
	if got, _ := state.Pin("here"); got.Messages != second.Messages {
		t.Errorf("the older pin won: %+v", got)
	}
}

func TestAPinInsideTheKeptPrefixRidesTheFork(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "scripted/local/test", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	appendMsg := func(m provider.Message) {
		t.Helper()
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	appendMsg(provider.UserText("first question"))
	appendMsg(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "first answer"}}})
	if _, err := sess.AppendPin("early"); err != nil {
		t.Fatal(err)
	}
	appendMsg(provider.UserText("second question"))
	appendMsg(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "second answer"}}})
	if _, err := sess.AppendPin("late"); err != nil {
		t.Fatal(err)
	}

	fork, err := store.Fork(sess.ID(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()

	if pin, ok := fork.State().Pin("early"); !ok || pin.Messages != 2 {
		t.Errorf("a pin inside the kept prefix was dropped or moved: %+v ok=%v", pin, ok)
	}
	// The late pin names a point the fork does not contain; carrying it
	// would let /fork cut past the log's own end.
	if _, ok := fork.State().Pin("late"); ok {
		t.Error("a pin past the cut rode the fork")
	}
}
