package main

import (
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
)

func TestWorkspaceInvalidationFollowsCompletedEffects(t *testing.T) {
	for _, test := range []struct {
		effect permission.Effect
		want   bool
	}{
		{permission.EffectRead, false},
		{permission.EffectExternal, false},
		{permission.EffectWrite, true},
		{permission.EffectExecute, true},
	} {
		if got := invalidatesWorkspace(permission.Request{Effect: test.effect}); got != test.want {
			t.Errorf("effect %s invalidates = %v, want %v", test.effect, got, test.want)
		}
	}
}
