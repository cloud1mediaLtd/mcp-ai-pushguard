package domain

import "testing"

func TestLifecycleTransitions(t *testing.T) {
	cases := []struct {
		from, to Status
		valid    bool
	}{
		{Queued, Running, true}, {Queued, Cancelled, true}, {Running, Passed, true},
		{Running, Failed, true}, {Passed, Running, false}, {Cancelled, Passed, false}, {Queued, Passed, false},
	}
	for _, tc := range cases {
		if got := ValidateTransition(tc.from, tc.to) == nil; got != tc.valid {
			t.Errorf("%s -> %s valid=%v", tc.from, tc.to, got)
		}
	}
}
