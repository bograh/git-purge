// Package classify decides, from collected evidence, whether a local branch is
// safe to delete. It is pure so the rules are easy to read and test.
package classify

import (
	"fmt"
	"path"

	"github.com/bograh/git-purge/internal/forge"
)

// Status is the verdict for a branch.
type Status string

const (
	// Safe: every commit on the branch already exists on the base branch
	// (merged, squash-merged, rebase-merged, or contained in a merged PR).
	Safe Status = "safe"
	// Review: the remote branch is gone but there is no proof the work landed
	// (PR closed unmerged, no PR found, or local commits beyond the merged PR).
	Review Status = "review"
	// Active: the branch still exists on the remote and is not merged.
	Active Status = "active"
	// Local: never pushed and not merged — local-only work.
	Local Status = "local"
	// Protected: default branch, checked out, or matches a protect pattern.
	Protected Status = "protected"
)

// Deletable reports whether the status is offered for deletion at all.
func (s Status) Deletable() bool { return s == Safe || s == Review }

// Evidence is everything known about one branch.
type Evidence struct {
	Name         string
	IsDefault    bool
	IsHead       bool
	WorktreePath string
	Protected    bool // matched a protect pattern

	HasUpstream bool
	Gone        bool // upstream configured but deleted on the remote

	Merged   bool // tip reachable from base
	Squashed bool // squash- or rebase-merged into base (patch equivalence)

	PR *forge.PR
	// LocalInPR: local tip equals, or is an ancestor of, the PR's head commit.
	// Nil when unknown (e.g. the PR head commit is not available locally).
	LocalInPR *bool
	// ExtraCommits: commits on the local branch beyond the PR head, when known.
	ExtraCommits int

	Base string // e.g. "origin/main", for messages
}

// Verdict is a status plus a human-readable reason.
type Verdict struct {
	Status Status
	Reason string
}

// Classify applies the safety rules in order of precedence.
func Classify(e Evidence) Verdict {
	switch {
	case e.IsDefault:
		return Verdict{Protected, "default branch"}
	case e.IsHead:
		return Verdict{Protected, "currently checked out"}
	case e.WorktreePath != "":
		return Verdict{Protected, "checked out in worktree " + e.WorktreePath}
	case e.Protected:
		return Verdict{Protected, "matches a protect pattern"}
	}

	gone := ""
	if e.Gone {
		gone = "; remote branch deleted"
	}

	if e.Merged {
		return Verdict{Safe, "merged into " + e.Base + gone}
	}
	if e.PR != nil && e.PR.State == forge.StateMerged {
		switch {
		case e.LocalInPR != nil && *e.LocalInPR:
			return Verdict{Safe, fmt.Sprintf("PR #%d merged; local tip is part of it%s", e.PR.Number, gone)}
		case e.Squashed:
			return Verdict{Safe, fmt.Sprintf("PR #%d merged; changes found on %s%s", e.PR.Number, e.Base, gone)}
		case e.LocalInPR != nil && e.ExtraCommits > 0:
			return Verdict{Review, fmt.Sprintf("PR #%d merged, but %d local commit(s) were never in it", e.PR.Number, e.ExtraCommits)}
		default:
			return Verdict{Review, fmt.Sprintf("PR #%d merged, but local tip differs from the merged head", e.PR.Number)}
		}
	}
	if e.Squashed {
		return Verdict{Safe, "squash/rebase-merged into " + e.Base + gone}
	}

	if e.PR != nil {
		switch e.PR.State {
		case forge.StateClosed:
			if e.Gone {
				return Verdict{Review, fmt.Sprintf("PR #%d closed without merging; remote branch deleted", e.PR.Number)}
			}
			return Verdict{Review, fmt.Sprintf("PR #%d closed without merging", e.PR.Number)}
		case forge.StateOpen:
			if e.Gone {
				return Verdict{Review, fmt.Sprintf("PR #%d still open but remote branch is gone", e.PR.Number)}
			}
			return Verdict{Active, fmt.Sprintf("PR #%d open", e.PR.Number)}
		}
	}

	switch {
	case e.Gone:
		return Verdict{Review, "remote branch deleted; no merge found"}
	case e.HasUpstream:
		return Verdict{Active, "tracking a live remote branch"}
	default:
		return Verdict{Local, "never pushed"}
	}
}

// DefaultProtect are branch patterns that are never offered for deletion.
var DefaultProtect = []string{"main", "master", "trunk", "develop", "dev", "staging", "production", "release/*"}

// MatchesAny reports whether name matches any glob pattern.
func MatchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}
