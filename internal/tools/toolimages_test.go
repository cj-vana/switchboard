package tools

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func imageRegistry(t *testing.T, vision bool) *Registry {
	t.Helper()
	r, err := NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	r.SetVision(func() bool { return vision })
	return r
}

func png(size int) provider.Image {
	return provider.Image{MediaType: "image/png", Data: make([]byte, size)}
}

// The case the bridge was losing: a screenshot reaches the model.
func TestAReturnedImageIsQueuedForTheNextRound(t *testing.T) {
	r := imageRegistry(t, true)

	note := r.AcceptToolImages([]provider.Image{png(10)})
	if !strings.Contains(note, "1 image returned") {
		t.Errorf("note = %q, which does not tell the model a picture is coming", note)
	}

	queued := r.TakeToolImages()
	if len(queued) != 1 {
		t.Fatalf("queued = %d images, want the one that was accepted", len(queued))
	}
	if again := r.TakeToolImages(); len(again) != 0 {
		t.Errorf("the queue delivered the same picture twice: %+v", again)
	}
}

// A dropped image the model is not told about is a model reasoning about a
// screenshot it never saw.
func TestARungWithoutVisionIsToldWhatWasDropped(t *testing.T) {
	r := imageRegistry(t, false)

	note := r.AcceptToolImages([]provider.Image{png(10), png(10)})
	if !strings.Contains(note, "2 images returned") || !strings.Contains(note, "dropped") {
		t.Errorf("note = %q, which does not say what happened", note)
	}
	if !strings.Contains(note, "does not read images") {
		t.Errorf("note = %q, which does not say why", note)
	}
	if queued := r.TakeToolImages(); len(queued) != 0 {
		t.Errorf("a rung that cannot see was sent %d images", len(queued))
	}
}

// A surface that never wired a check has no way to know, and no is the closed
// answer.
func TestNoVisionCheckMeansNoDelivery(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	r.AcceptToolImages([]provider.Image{png(10)})
	if queued := r.TakeToolImages(); len(queued) != 0 {
		t.Errorf("images were queued with no vision check wired: %d", len(queued))
	}
}

// A tool that answers with a gallery is answering a different question, and
// the cap has to say when it bit.
func TestTheCountCapBitesAndSaysSo(t *testing.T) {
	r := imageRegistry(t, true)
	var many []provider.Image
	for range maxToolImagesPerCall + 3 {
		many = append(many, png(10))
	}

	note := r.AcceptToolImages(many)
	queued := r.TakeToolImages()
	if len(queued) != maxToolImagesPerCall {
		t.Errorf("queued %d, want the cap of %d", len(queued), maxToolImagesPerCall)
	}
	if !strings.Contains(note, "first") {
		t.Errorf("note = %q, which does not say a cap applied", note)
	}
}

// Base64 image data is the fastest way to fill a context window and a budget
// at once.
func TestTheByteCapBitesAndSaysSo(t *testing.T) {
	r := imageRegistry(t, true)
	huge := []provider.Image{png(maxToolImageBytes - 1), png(maxToolImageBytes)}

	note := r.AcceptToolImages(huge)
	queued := r.TakeToolImages()
	if len(queued) != 1 {
		t.Errorf("queued %d images, want only what fits the byte cap", len(queued))
	}
	if !strings.Contains(note, "MiB") {
		t.Errorf("note = %q, which does not say the byte cap applied", note)
	}
}

// No images is no note: a result should not grow a sentence about something
// that did not happen.
func TestNoImagesAddsNothingToTheResult(t *testing.T) {
	r := imageRegistry(t, true)
	if note := r.AcceptToolImages(nil); note != "" {
		t.Errorf("note = %q, want nothing", note)
	}
}

// A session swap makes the queue meaningless: those pictures answered a
// question the new session never asked.
func TestASessionSwapDropsUndeliveredImages(t *testing.T) {
	r := imageRegistry(t, true)
	r.AcceptToolImages([]provider.Image{png(10)})
	r.ForgetToolImages()
	if queued := r.TakeToolImages(); len(queued) != 0 {
		t.Errorf("%d images survived a session swap", len(queued))
	}
}
