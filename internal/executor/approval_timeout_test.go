package executor

import "testing"

// An unbounded approval gate strands its run — and on a schedule strands a new
// one every cycle — so node data can never express "wait forever".
func TestNormalizeApprovalTimeout(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero (legacy 'no timeout') becomes the default", 0, DefaultApprovalTimeout},
		{"negative becomes the default", -1, DefaultApprovalTimeout},
		{"5 minutes is kept", 300, 300},
		{"30 minutes is kept", 1800, 1800},
		{"8 hours is kept", 28800, 28800},
		{"24 hours is kept", 86400, 86400},
		{"3 days is the ceiling and is kept", MaxApprovalTimeout, MaxApprovalTimeout},
		{"beyond 3 days is capped", MaxApprovalTimeout + 1, MaxApprovalTimeout},
		{"the old 7-day fallback is capped", 7 * 24 * 3600, MaxApprovalTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeApprovalTimeout(c.in); got != c.want {
				t.Fatalf("NormalizeApprovalTimeout(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// The UI's largest expressible wait (3 days) must be exactly what the server
// allows, so a user can never configure something that gets silently shortened.
func TestApprovalCeilingMatchesUIMaximum(t *testing.T) {
	const uiMaxDays = 3
	if MaxApprovalTimeout != uiMaxDays*24*60*60 {
		t.Fatalf("ceiling %ds disagrees with the UI's %d-day maximum", MaxApprovalTimeout, uiMaxDays)
	}
	if MaxApprovalTimeout != 72*60*60 {
		t.Fatalf("ceiling %ds disagrees with the UI's 72-hour maximum", MaxApprovalTimeout)
	}
}
