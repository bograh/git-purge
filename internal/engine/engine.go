// Package engine ties git, the forge and the database together: Sync builds a
// classified snapshot of every local branch, Purge deletes selected ones.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/forge"
	"github.com/bograh/git-purge/internal/gitx"
	"github.com/bograh/git-purge/internal/store"
)

// Options control a sync.
type Options struct {
	NoFetch  bool     // skip `git fetch --all --prune`
	Offline  bool     // skip forge API lookups
	Protect  []string // extra protect patterns
	Workers  int
	Progress func(format string, args ...any)
}

// Result is the outcome of a sync.
type Result struct {
	Base     string
	Forge    string
	Branches []store.Branch
	Warnings []string
}

const prNone = "none" // cached "no PR found" marker

// Sync inspects every local branch and stores the classified snapshot.
func Sync(ctx context.Context, repo *gitx.Repo, st *store.Store, rec *store.Repo, opt Options) (*Result, error) {
	progress := opt.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	res := &Result{}
	warn := func(format string, args ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...)) }

	if !opt.NoFetch {
		progress("Fetching and pruning remotes…")
		if err := repo.FetchPrune(); err != nil {
			warn("fetch failed, using existing remote-tracking refs: %v", err)
		}
	}

	base, baseName, err := repo.DefaultBranch()
	if err != nil {
		return nil, err
	}
	res.Base = base

	branches, err := repo.Branches()
	if err != nil {
		return nil, err
	}
	merged, err := repo.MergedInto(base)
	if err != nil {
		return nil, err
	}
	// Work merged into the local default branch (not yet pushed) is also safe.
	if base != baseName && repo.RefExists("refs/heads/"+baseName) {
		if local, err := repo.MergedInto("refs/heads/" + baseName); err == nil {
			for k := range local {
				merged[k] = true
			}
		}
	}

	protect := append(append(append([]string{}, classify.DefaultProtect...), repo.ConfigAll("gitpurge.protect")...), opt.Protect...)

	prev := map[string]store.Branch{}
	if old, err := st.Branches(rec.ID); err == nil {
		for _, b := range old {
			prev[b.Name] = b
		}
	}

	var fg forge.Forge
	baseRemote := "origin"
	if !opt.Offline {
		fg, baseRemote, err = detectForge(repo)
		if err != nil {
			warn("forge lookups disabled: %v", err)
		} else {
			res.Forge = fg.Name()
		}
	}

	type item struct {
		b   gitx.Branch
		ev  classify.Evidence
		row store.Branch
	}
	items := make([]*item, len(branches))
	for i, b := range branches {
		items[i] = &item{b: b, ev: classify.Evidence{
			Name:         b.Name,
			IsDefault:    b.Name == baseName,
			IsHead:       b.IsHead,
			WorktreePath: b.WorktreePath,
			Protected:    classify.MatchesAny(b.Name, protect),
			HasUpstream:  b.Upstream != "",
			Gone:         b.Gone,
			Merged:       merged[b.Name],
			Base:         base,
		}}
	}
	candidate := func(it *item) bool {
		e := it.ev
		return !(e.IsDefault || e.IsHead || e.WorktreePath != "" || e.Protected || e.Merged)
	}

	// Offline evidence: squash/rebase merges.
	progress("Checking %d branches for squash/rebase merges…", len(items))
	parallel(items, opt.Workers, func(it *item) {
		if !candidate(it) {
			return
		}
		ok, err := repo.SquashMerged(base, it.b.SHA)
		if err != nil {
			return
		}
		it.ev.Squashed = ok
	})

	// Online evidence: PR state, only for pushed branches lacking proof.
	if fg != nil {
		var lookups []*item
		for _, it := range items {
			if !candidate(it) || it.ev.Squashed || it.b.Upstream == "" {
				continue
			}
			if p, ok := prev[it.b.Name]; ok && p.PRChecked == it.b.SHA && (p.PRState == forge.StateMerged || p.PRState == forge.StateClosed) {
				it.ev.PR = &forge.PR{Number: p.PRNumber, State: p.PRState, HeadSHA: p.PRHeadSHA, URL: p.PRURL}
				continue // terminal PR state, branch unchanged: cache hit
			}
			lookups = append(lookups, it)
		}
		if len(lookups) > 0 {
			progress("Asking %s about %d branch(es)…", fg.Name(), len(lookups))
		}
		ctx, cancel := context.WithCancel(ctx)
		var mu sync.Mutex
		var fatal error
		failed := 0
		parallel(lookups, opt.Workers, func(it *item) {
			name := it.b.UpstreamBranch()
			if name == "" {
				name = it.b.Name
			}
			pr, err := fg.FindPR(ctx, name)
			if err != nil {
				mu.Lock()
				defer mu.Unlock()
				var rl *forge.RateLimitError
				if errors.Is(err, forge.ErrUnauthorized) || errors.As(err, &rl) {
					if fatal == nil {
						fatal = err
						cancel()
					}
				} else if !errors.Is(err, context.Canceled) {
					failed++
					if failed == 1 {
						warn("%s lookup failed: %v", fg.Name(), err)
					}
				}
				return
			}
			it.ev.PR = pr
			it.row.PRChecked = it.b.SHA
			if pr == nil {
				it.row.PRState = prNone
			}
		})
		cancel()
		if fatal != nil {
			warn("%s lookups stopped: %v (set GH_TOKEN / GITLAB_TOKEN or use --offline)", fg.Name(), fatal)
		}
		if failed > 1 {
			warn("%d %s lookups failed in total", failed, fg.Name())
		}
	}

	// Compare the local tip with merged PR heads.
	for _, it := range items {
		pr := it.ev.PR
		if pr == nil || pr.State != forge.StateMerged || pr.HeadSHA == "" {
			continue
		}
		if pr.HeadSHA == it.b.SHA {
			t := true
			it.ev.LocalInPR = &t
			continue
		}
		if !repo.CommitExists(pr.HeadSHA) && fg != nil {
			_, _ = repo.Run("fetch", "--quiet", "--no-write-fetch-head", baseRemote, fg.HeadRef(pr.Number))
		}
		if repo.CommitExists(pr.HeadSHA) {
			in := repo.IsAncestor(it.b.SHA, pr.HeadSHA)
			it.ev.LocalInPR = &in
			if !in {
				it.ev.ExtraCommits, _ = repo.CountCommits(pr.HeadSHA, it.b.SHA)
			}
		}
	}

	for _, it := range items {
		v := classify.Classify(it.ev)
		row := store.Branch{
			Name:       it.b.Name,
			SHA:        it.b.SHA,
			Upstream:   it.b.Upstream,
			Remote:     it.b.UpstreamRemote,
			MergeRef:   it.b.UpstreamRef,
			Gone:       it.b.Gone,
			Merged:     it.ev.Merged,
			Squashed:   it.ev.Squashed,
			PRChecked:  it.row.PRChecked,
			PRState:    it.row.PRState,
			Status:     string(v.Status),
			Reason:     v.Reason,
			CommitTime: it.b.CommitTime,
			Subject:    it.b.Subject,
		}
		if pr := it.ev.PR; pr != nil {
			row.PRNumber, row.PRState, row.PRHeadSHA, row.PRURL = pr.Number, pr.State, pr.HeadSHA, pr.URL
			if row.PRChecked == "" {
				row.PRChecked = it.b.SHA // came from cache
			}
		}
		res.Branches = append(res.Branches, row)
	}

	if err := st.ReplaceBranches(rec, base, res.Forge, res.Branches); err != nil {
		return nil, err
	}
	return res, nil
}

// detectForge builds a forge client from the repo's remotes. PRs target
// "upstream" when it exists (fork workflow), otherwise "origin".
func detectForge(repo *gitx.Repo) (forge.Forge, string, error) {
	headRemote := "origin"
	if !repo.HasRemote(headRemote) {
		return nil, "", errors.New("no 'origin' remote")
	}
	baseRemote := headRemote
	if repo.HasRemote("upstream") {
		baseRemote = "upstream"
	}
	parse := func(name string) (forge.Remote, error) {
		u, err := repo.RemoteURL(name)
		if err != nil {
			return forge.Remote{}, err
		}
		return forge.ParseRemote(u)
	}
	head, err := parse(headRemote)
	if err != nil {
		return nil, "", err
	}
	base, err := parse(baseRemote)
	if err != nil {
		return nil, "", err
	}
	f, err := forge.New(base, head)
	return f, baseRemote, err
}

func parallel[T any](items []T, workers int, fn func(T)) {
	ch := make(chan T)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				fn(it)
			}
		}()
	}
	for _, it := range items {
		ch <- it
	}
	close(ch)
	wg.Wait()
}

// Outcome is the result of deleting one branch.
type Outcome struct {
	Branch store.Branch
	Err    error
}

// Purge deletes the given branches. Each deletion is a compare-and-swap on the
// synced SHA and is logged so it can be restored.
func Purge(repo *gitx.Repo, st *store.Store, rec *store.Repo, branches []store.Branch, dryRun bool) []Outcome {
	batch := store.NewBatch()
	var out []Outcome
	for _, b := range branches {
		if !classify.Status(b.Status).Deletable() {
			out = append(out, Outcome{b, fmt.Errorf("refusing to delete %s branch", b.Status)})
			continue
		}
		if dryRun {
			out = append(out, Outcome{Branch: b})
			continue
		}
		if err := repo.DeleteBranch(b.Name, b.SHA); err != nil {
			out = append(out, Outcome{b, err})
			continue
		}
		if err := st.LogPurge(rec.ID, batch, b); err != nil {
			// The branch is gone but unlogged: surface the SHA so it is recoverable.
			out = append(out, Outcome{b, fmt.Errorf("deleted, but failed to log (restore with: git branch %s %s): %w", b.Name, b.SHA, err)})
			continue
		}
		_ = st.RemoveBranch(rec.ID, b.Name)
		out = append(out, Outcome{Branch: b})
	}
	return out
}
