package provider

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestRequestIssuedDistinguishesLocalPreflightFromTransportFailure(t *testing.T) {
	capability := &CapabilityError{Target: "p/s/m", Capability: "test", Detail: "local"}
	if RequestIssued(capability) || RequestIssued(fmt.Errorf("wrapped: %w", capability)) {
		t.Fatal("a local capability failure was classified as an issued request")
	}
	local := MarkUnissued(errors.New("local render failed"))
	if RequestIssued(local) || !errors.Is(local, errors.Unwrap(local)) {
		t.Fatalf("marked local failure lost issuance or unwrap semantics: %v", local)
	}
	if !RequestIssued(errors.New("connection reset after send")) {
		t.Fatal("an unmarked transport failure was not conservatively classified as issued")
	}
}

func TestUsageRejectsNegativeCounts(t *testing.T) {
	for _, usage := range []Usage{
		{InputTokens: -1},
		{OutputTokens: -1},
		{CacheReadTokens: -1},
		{CacheWriteTokens: -1},
	} {
		if err := usage.Validate(); err == nil {
			t.Fatalf("negative usage was accepted: %+v", usage)
		}
	}
}

func TestUsageAggregationNeverWraps(t *testing.T) {
	if _, err := (Usage{InputTokens: math.MaxInt}).CheckedAdd(Usage{InputTokens: 1}); err == nil {
		t.Fatal("checked usage aggregation accepted overflow")
	}
	got := (Usage{InputTokens: math.MaxInt}).Add(Usage{InputTokens: 1})
	if got.InputTokens != math.MaxInt || got.InputTokens < 0 {
		t.Fatalf("saturating usage aggregation wrapped: %+v", got)
	}
	if got := (Usage{InputTokens: math.MaxInt, CacheReadTokens: 1}).TotalInputTokens(); got != math.MaxInt {
		t.Fatalf("input token total = %d, want saturation", got)
	}
}
