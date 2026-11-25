package classify

import (
	"testing"
	"time"

	"github.com/bograh/git-purge/internal/forge"
)

func ptr(b bool) *bool { return &b }

func TestClassify(t *testing.T) {
	merged := &forge.PR{Number: 7, State: forge.StateMerged, HeadSHA: "abc"}
	closed := &forge.PR{Number: 8, State: forge.StateClosed}
	open := &forge.PR{Number: 9, State: forge.StateOpen}
	cases := []struct {
		name string
		ev   Evidence
		want Status
	}{
		{"default wins over merged", Evidence{IsDefault: true, Merged: true}, Protected},
		{"head", Evidence{IsHead: true, Merged: true}, Protected},
		{"worktree", Evidence{WorktreePath: "/x", Merged: true}, Protected},
		{"pattern", Evidence{Protected: true, Gone: true}, Protected},
		{"merged", Evidence{Merged: true, Gone: true}, Safe},
		{"merged, live remote", Evidence{Merged: true, HasUpstream: true}, Safe},
		{"squashed", Evidence{Squashed: true, Gone: true}, Safe},
		{"PR merged, tip in PR", Evidence{PR: merged, LocalInPR: ptr(true), Gone: true}, Safe},
		{"PR merged, extra commits", Evidence{PR: merged, LocalInPR: ptr(false), ExtraCommits: 2, Gone: true}, Review},
		{"PR merged, unknown head", Evidence{PR: merged, Gone: true}, Review},
		{"PR merged, unknown head but squashed", Evidence{PR: merged, Squashed: true}, Safe},
		{"PR closed", Evidence{PR: closed, Gone: true, HasUpstream: true}, Review},
		{"PR closed, live", Evidence{PR: closed, HasUpstream: true}, Review},
		{"PR open", Evidence{PR: open, HasUpstream: true}, Active},
		{"PR open, gone", Evidence{PR: open, Gone: true, HasUpstream: true}, Review},
		{"gone, no evidence", Evidence{Gone: true, HasUpstream: true}, Review},
		{"live upstream", Evidence{HasUpstream: true}, Active},
		{"never pushed", Evidence{}, Local},
	}
	for _, c := range cases {
		if got := Classify(c.ev); got.Status != c.want {
			t.Errorf("%s: got %s (%s), want %s", c.name, got.Status, got.Reason, c.want)
		}
	}
}

func TestMatchesAny(t *testing.T) {
	if !MatchesAny("release/1.2", DefaultProtect) || MatchesAny("feature/x", DefaultProtect) {
		t.Fatal("pattern matching wrong")
	}
}

func TestParseAge(t *testing.T) {
	day := 24 * time.Hour
	cases := map[string]time.Duration{"90d": 90 * day, "6mo": 180 * day, "2w": 14 * day, "1y": 365 * day, "36h": 36 * time.Hour, "1.5d": 36 * time.Hour}
	for in, want := range cases {
		if got, err := ParseAge(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "-3d", "d"} {
		if _, err := ParseAge(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if HumanAge(90*day) != "3mo" || HumanAge(400*day) != "400d" || HumanAge(730*day) != "2y" {
		t.Errorf("HumanAge: %s %s %s", HumanAge(90*day), HumanAge(400*day), HumanAge(730*day))
	}
}
