package main

import (
	"slices"
	"strings"
	"testing"
)

func TestClassifyShim(t *testing.T) {
	for _, tc := range []struct {
		args     string
		disabled bool
		want     shimAction
		chdirs   []string
	}{
		{"worktree add ../x -b x", false, addWorktree, nil},
		{"-C /repo -C sub worktree add ../x", false, addWorktree, []string{"/repo", "sub"}},
		{"worktree add --no-checkout ../x", false, passThrough, nil},
		{"worktree add -h", false, passThrough, nil},
		{"worktree list", false, passThrough, nil},
		{"worktree add ../x", true, passThrough, nil},
		{"checkout main", false, checkoutSync, nil},
		{"-C /repo switch -c feature", false, checkoutSync, []string{"/repo"}},
		{"checkout main", true, passThrough, nil},
		{"-c core.x=y worktree add ../x", false, passThrough, nil}, // other global options: plain git
		{"status", false, passThrough, nil},
		{"", false, passThrough, nil},
	} {
		got, chdirs, _ := classifyShim(strings.Fields(tc.args), tc.disabled)
		if got != tc.want || !slices.Equal(chdirs, tc.chdirs) {
			t.Errorf("classifyShim(%q, disabled=%v) = %v %v, want %v %v", tc.args, tc.disabled, got, chdirs, tc.want, tc.chdirs)
		}
	}
}
