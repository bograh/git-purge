package engine

import (
	"fmt"
	"time"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/store"
)

// AgeFilter keeps only branches whose last commit is older than minAge and
// promotes old active/local branches to Stale so they can be picked. Protected
// branches stay protected. A zero minAge returns branches unchanged.
func AgeFilter(branches []store.Branch, minAge time.Duration, now time.Time) []store.Branch {
	if minAge <= 0 {
		return branches
	}
	var out []store.Branch
	for _, b := range branches {
		age := now.Sub(b.CommitTime)
		if age < minAge {
			continue
		}
		days := classify.HumanAge(age.Truncate(24 * time.Hour))
		switch classify.Status(b.Status) {
		case classify.Active:
			b.Status, b.Reason = string(classify.Stale), fmt.Sprintf("no commits for %s; still on remote, unmerged", days)
		case classify.Local:
			b.Status, b.Reason = string(classify.Stale), fmt.Sprintf("no commits for %s; never pushed", days)
		}
		out = append(out, b)
	}
	return out
}
