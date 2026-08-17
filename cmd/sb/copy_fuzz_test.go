package main

import "testing"

// A response's markdown is whatever the model streamed, cut anywhere; the
// fence extractor must hand back blocks or nothing, never panic.
func FuzzCodeBlocks(f *testing.F) {
	f.Add("```go\ncode\n```")
	f.Add("~~~\nx")
	f.Add("````nested")
	f.Fuzz(func(t *testing.T, text string) {
		for _, b := range codeBlocks(text) {
			_ = len(b)
		}
		_ = plainLine(text)
	})
}
