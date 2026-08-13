package execution

import (
	"fmt"
	"sync"
)

// capture accepts every byte a command writes but keeps only the head and the
// tail. It keeps both ends because either can hold the answer: a compiler puts
// its errors last, a directory listing puts what you asked for first. Dropping
// only the middle, and saying so, beats picking one end and hoping.
//
// It never stops accepting writes. A writer that refuses output would block the
// child on a full pipe, turning an over-talkative command into a hang.
type capture struct {
	mu sync.Mutex

	head []byte

	// tail is a ring holding the last tailMax bytes written after head filled.
	tail     []byte
	tailPos  int
	tailLen  int
	tailMax  int
	headMax  int
	total    int
	overflow bool
}

func newCapture(maxOutput int) *capture {
	// The head gets the larger share: the start of output orients a reader,
	// while the tail usually only needs to carry the final error.
	head := maxOutput * 3 / 5
	tail := maxOutput - head
	if tail < 1 {
		tail = 1
	}
	return &capture{
		headMax: head,
		tailMax: tail,
		tail:    make([]byte, tail),
	}
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := len(p)
	c.total += n

	if room := c.headMax - len(c.head); room > 0 {
		take := min(room, len(p))
		c.head = append(c.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		c.overflow = true
		c.writeTail(p)
	}
	return n, nil
}

func (c *capture) writeTail(p []byte) {
	if len(p) >= c.tailMax {
		copy(c.tail, p[len(p)-c.tailMax:])
		c.tailPos = 0
		c.tailLen = c.tailMax
		return
	}
	for len(p) > 0 {
		n := copy(c.tail[c.tailPos:], p)
		p = p[n:]
		c.tailPos = (c.tailPos + n) % c.tailMax
		c.tailLen = min(c.tailLen+n, c.tailMax)
	}
}

// String returns the captured output and whether anything was dropped. A
// truncation the model cannot see is worse than no output at all, because it
// will draw a confident conclusion from a fragment it believes is complete
// (§10.3).
func (c *capture) String() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.overflow {
		return string(c.head), false
	}

	tail := make([]byte, 0, c.tailLen)
	if c.tailLen == c.tailMax {
		tail = append(tail, c.tail[c.tailPos:]...)
		tail = append(tail, c.tail[:c.tailPos]...)
	} else {
		tail = append(tail, c.tail[:c.tailLen]...)
	}

	dropped := c.total - len(c.head) - len(tail)
	if dropped <= 0 {
		return string(c.head) + string(tail), false
	}
	marker := fmt.Sprintf("\n[switchboard: %d bytes of output omitted from the middle]\n", dropped)
	return string(c.head) + marker + string(tail), true
}
