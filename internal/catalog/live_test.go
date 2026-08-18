package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// The catalog claims cache minimums that are not monotonic across model
// generations: 512 tokens on Opus 5 and 4,096 on Haiku 4.5, so a prefix of a
// few thousand tokens caches on the larger, newer model and silently does not
// on the smaller one.
//
// §6.4 leans on that being true, and the failure mode is invisible from the
// request side: a marker below the minimum is accepted and does nothing. This
// asks the API which is right.
func TestLiveCacheMinimumsAreNotMonotonic(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run against the live API (this spends money)")
	}
	secret, err := credential.Chain(credential.Settings{}).Get(
		context.Background(), credential.Ref{Provider: "anthropic", Account: "first-party"})
	if err != nil {
		t.Skipf("no credential: %v", err)
	}

	c, err := LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	minimumOf := func(model string) int {
		info, _, ok := c.Lookup(provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: model})
		if !ok {
			t.Fatalf("%s is not in the catalog", model)
		}
		return info.Cache.MinTokens
	}

	const small, large = "claude-haiku-4-5", "claude-opus-5"
	smallMin, largeMin := minimumOf(small), minimumOf(large)
	if largeMin >= smallMin {
		t.Fatalf("the catalog no longer claims a non-monotonic minimum (%s %d, %s %d), so this test proves nothing",
			large, largeMin, small, smallMin)
	}

	// Between the two claimed minimums.
	prefix := strings.Repeat("You are a precise assistant. Follow the tool schema exactly. ", 200)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cached := func(model string) provider.Usage {
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 16,
			"system": []map[string]any{{
				"type": "text", "text": prefix,
				"cache_control": map[string]string{"type": "ephemeral"},
			}},
			"messages": []map[string]string{{"role": "user", "content": "Say OK."}},
		})
		if err != nil {
			t.Fatal(err)
		}

		var last provider.Usage
		// Twice: the first call writes if it can, the second reads.
		for range 2 {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("x-api-key", secret.Expose())
			req.Header.Set("anthropic-version", "2023-06-01")
			req.Header.Set("content-type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			err = json.NewDecoder(resp.Body).Decode(&decoded)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Error != nil {
				t.Fatalf("%s: %s", model, decoded.Error.Message)
			}
			last = provider.Usage{
				InputTokens:      decoded.Usage.InputTokens,
				CacheReadTokens:  decoded.Usage.CacheReadInputTokens,
				CacheWriteTokens: decoded.Usage.CacheCreationInputTokens,
			}
		}
		return last
	}

	big := cached(large)
	if big.CacheReadTokens == 0 && big.CacheWriteTokens == 0 {
		t.Errorf("%s cached nothing for a prefix above its claimed %d token minimum: %+v",
			large, largeMin, big)
	}

	little := cached(small)
	if little.CacheReadTokens != 0 || little.CacheWriteTokens != 0 {
		t.Errorf("%s cached a prefix below its claimed %d token minimum: %+v\n"+
			"the catalog's minimum is too high and every marker under it is being declined for no reason",
			small, smallMin, little)
	}

	fmt.Printf("  %s cached %d tokens; %s left %d uncached\n",
		large, big.CacheReadTokens+big.CacheWriteTokens, small, little.InputTokens)
}
