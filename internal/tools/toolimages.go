package tools

// Images an external tool returned, on their way to the model.
//
// A browser or devtools server answers a screenshot request with an image
// block, and the bridge kept only the text, writing "[image content omitted]"
// where the picture was. Vision is wired end to end in this program — the
// router excludes a rung that cannot see, the catalog records which can, an
// @-mentioned file already rides in as one — and the only producer was the
// user's own mention. A server that takes a screenshot for the model and then
// cannot show it to the model is the whole feature missing its last inch.
//
// They travel as an injected user-role message at the next round boundary
// rather than inside the tool result, and that is not a convenience. Every
// adapter already maps provider.Image inside a message; none has a captured
// mapping for an image inside a tool_result, and inventing one would be
// mapping a wire format nobody has run. This adds no adapter code and touches
// no wire format.
//
// Two bounds and a gate. A rung with no vision is never sent one, and the tool
// result says how many were dropped rather than omitting them quietly; a
// devtools server returning a full-page screenshot per call is the fastest way
// to spend a context window, so both the count and the bytes per call are
// capped and the result says when a cap bit.

import (
	"fmt"
	"sync"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const (
	// maxToolImagesPerCall bounds one result. A tool that returns a gallery is
	// answering a different question than the one that was asked.
	maxToolImagesPerCall = 4

	// maxToolImageBytes bounds one call's total. Base64 image data is the
	// fastest way to fill a context window and a budget at once.
	maxToolImageBytes = 4 << 20
)

// toolImages is the session's queue of pictures waiting for a round boundary.
type toolImages struct {
	mu      sync.Mutex
	pending []provider.Image

	// vision reports whether the currently bound target can read an image. It
	// is a function rather than a value because the binding moves: a relief
	// substitution or an escalation can change the answer mid-turn, and a flag
	// captured at assembly would be describing a rung that is no longer there.
	vision func() bool
}

// SetVision wires the live capability check. A surface that never sets one
// leaves images undeliverable, which is the closed state a headless or
// unattended assembly should have.
func (r *Registry) SetVision(check func() bool) { r.images.vision = check }

func (r *Registry) visionAvailable() bool {
	if r.images.vision == nil {
		return false
	}
	return r.images.vision()
}

// AcceptToolImages queues what an external tool returned and reports the
// sentence its result should carry.
//
// The sentence is the point of the return value: a dropped image the model is
// not told about is a model reasoning about a screenshot it never saw.
func (r *Registry) AcceptToolImages(images []provider.Image) string {
	if len(images) == 0 {
		return ""
	}
	if !r.visionAvailable() {
		return fmt.Sprintf("\n\n[%s returned, and this model does not read images, so %s dropped. "+
			"Bind a rung with vision if you need to see them.]",
			countImages(len(images)), wereOrWas(len(images)))
	}

	kept, droppedCount, droppedBytes := boundImages(images)
	r.images.mu.Lock()
	r.images.pending = append(r.images.pending, kept...)
	r.images.mu.Unlock()

	note := fmt.Sprintf("\n\n[%s returned; %s shown to you after this result.]",
		countImages(len(images)), countImages(len(kept)))
	switch {
	case droppedBytes:
		note = fmt.Sprintf("\n\n[%s returned; %s shown to you after this result. "+
			"The rest exceeded the %d MiB this call may carry.]",
			countImages(len(images)), countImages(len(kept)), maxToolImageBytes>>20)
	case droppedCount:
		note = fmt.Sprintf("\n\n[%s returned; the first %d are shown to you after this result.]",
			countImages(len(images)), len(kept))
	}
	return note
}

// boundImages applies both caps, count first. It keeps a prefix rather than a
// selection because the order a tool returned them in is the only ordering
// evidence there is.
func boundImages(images []provider.Image) (kept []provider.Image, droppedCount, droppedBytes bool) {
	if len(images) > maxToolImagesPerCall {
		images, droppedCount = images[:maxToolImagesPerCall], true
	}
	total := 0
	for _, image := range images {
		if total+len(image.Data) > maxToolImageBytes {
			droppedBytes = true
			break
		}
		total += len(image.Data)
		kept = append(kept, image)
	}
	return kept, droppedCount, droppedBytes
}

// TakeToolImages drains the queue for delivery at a round boundary. It empties
// as it reads, because an image delivered twice is the context paying for the
// same picture twice.
func (r *Registry) TakeToolImages() []provider.Image {
	r.images.mu.Lock()
	defer r.images.mu.Unlock()
	pending := r.images.pending
	r.images.pending = nil
	return pending
}

// ForgetToolImages drops anything undelivered. A session swap makes the queue
// meaningless: those pictures answered a question the new session never asked.
func (r *Registry) ForgetToolImages() {
	r.images.mu.Lock()
	defer r.images.mu.Unlock()
	r.images.pending = nil
}

func countImages(n int) string {
	if n == 1 {
		return "1 image"
	}
	return fmt.Sprintf("%d images", n)
}

func wereOrWas(n int) string {
	if n == 1 {
		return "it was"
	}
	return "they were"
}
