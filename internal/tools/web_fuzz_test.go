package tools

import (
	"strings"
	"testing"
)

// The endpoint's HTML is untrusted bytes; the parser must degrade to fewer
// or worse results, never to a panic.
func FuzzParseDDG(f *testing.F) {
	f.Add(`<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fx">t</a>`)
	f.Add(`<div class="result__snippet">s</div>`)
	f.Add("<html><body>plain")
	f.Fuzz(func(t *testing.T, html string) {
		results, err := parseDDG(strings.NewReader(html))
		if err == nil {
			for _, r := range results {
				_ = r.title + r.url + r.snippet
			}
		}
	})
}
