// Command git-purge safely clears out local branches whose work already landed.
//
// Install it on your PATH and it also works as `git purge`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/engine"
	"github.com/bograh/git-purge/internal/forge"
	"github.com/bograh/git-purge/internal/gitx"
	"github.com/bograh/git-purge/internal/scan"
	"github.com/bograh/git-purge/internal/store"
	"github.com/bograh/git-purge/internal/tui"
)

var version = "dev"

const usage = `git-purge — safely clear out dead local branches

Usage:
  git purge [flags]               sync, then pick branches to delete interactively
  git purge sync [flags]          fetch --prune, classify branches, update the database
  git purge list [--all] [--json] show the last synced classification
  git purge purge [--yes] [--dry-run] [--include-review] [--include-stale]
                                  delete branches (interactive unless --yes)
  git purge restore --last | <branch>...
                                  bring back purged branches
  git purge history               show the purge log

Statuses:
  safe       every commit is already on the base branch (merged, squash-merged,
             rebase-merged, or the local tip is part of a merged PR)
  review     dead on remote but no proof the work landed — look before deleting
  stale      active/local branch untouched for longer than --older-than
  active     still exists on the remote, not merged            (never deleted)
  local      never pushed, not merged                          (never deleted)
  protected  default/current/worktree branch or protect pattern (never deleted)

Common flags:
  -C <dir>             run as if started in <dir>
  --all-repos <dir>    work on every git repository under <dir>
  --depth <n>          how deep --all-repos searches (default 3)
  --older-than <age>   only branches with no commits for <age> (90d, 6mo, 2w, 1y);
                       old unmerged branches become "stale" and can be picked
  --offline            skip GitHub/GitLab API lookups (git-only evidence)
  --no-fetch           don't run git fetch --all --prune first
  --protect <glob>     never touch matching branches (repeatable; also:
                       git config --add gitpurge.protect <glob>)
  --db <path>          database path (default: $GIT_PURGE_DB or ~/.local/share/git-purge/state.db)

Picker keys: space toggle · p preview commits/diff · s/r/t toggle all safe/review/stale
             / filter · o oldest first · h show kept · enter delete · q quit

Auth: GitHub uses GH_TOKEN / GITHUB_TOKEN or 'gh auth token' (batched GraphQL
lookups need a token); GitLab uses GITLAB_TOKEN.
`

// target is one repository being worked on.
type target struct {
	label string // display name in multi-repo mode, "" otherwise
	repo  *gitx.Repo
	rec   *store.Repo
}

type globals struct {
	dir       string
	allRepos  string
	depth     int
	olderThan string
	minAge    time.Duration
	offline   bool
	noFetch   bool
	protect   multi
	db        string

	st      *store.Store
	targets []*target
}

func (g *globals) multi() bool { return g.allRepos != "" }

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.dir, "C", ".", "")
	fs.StringVar(&g.allRepos, "all-repos", "", "")
	fs.IntVar(&g.depth, "depth", 3, "")
	fs.StringVar(&g.olderThan, "older-than", "", "")
	fs.BoolVar(&g.offline, "offline", false, "")
	fs.BoolVar(&g.noFetch, "no-fetch", false, "")
	fs.Var(&g.protect, "protect", "")
	fs.StringVar(&g.db, "db", "", "")
}

func (g *globals) open() error {
	if g.olderThan != "" {
		d, err := classify.ParseAge(g.olderThan)
		if err != nil {
			return err
		}
		g.minAge = d
	}
	path := g.db
	if path == "" {
		var err error
		if path, err = store.DefaultPath(); err != nil {
			return err
		}
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	g.st = st

	dirs := []string{g.dir}
	root := ""
	if g.multi() {
		if root, err = filepath.Abs(g.allRepos); err != nil {
			return err
		}
		if dirs, err = scan.Repos(root, g.depth); err != nil {
			return err
		}
		if len(dirs) == 0 {
			return fmt.Errorf("no git repositories found under %s (try a larger --depth)", root)
		}
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		repo, err := gitx.Open(d)
		if err != nil {
			if !g.multi() {
				return err
			}
			warnf("skipping %s: %v", d, err)
			continue
		}
		if seen[repo.CommonDir] {
			continue
		}
		seen[repo.CommonDir] = true
		rec, err := st.Repo(repo.CommonDir)
		if err != nil {
			return err
		}
		t := &target{repo: repo, rec: rec}
		if g.multi() {
			if t.label, err = filepath.Rel(root, repo.Dir); err != nil || t.label == "." {
				t.label = filepath.Base(repo.Dir)
			}
		}
		g.targets = append(g.targets, t)
	}
	return nil
}

func (g *globals) syncOptions() engine.Options {
	opt := engine.Options{NoFetch: g.noFetch, Offline: g.offline, Protect: g.protect}
	if !g.multi() {
		opt.Progress = func(f string, a ...any) { fmt.Fprintln(os.Stderr, dim(fmt.Sprintf(f, a...))) }
	}
	return opt
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "git-purge:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	g := &globals{}
	fs := flag.NewFlagSet("git-purge "+cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	g.register(fs)
	parse := func() error {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				os.Exit(0)
			}
			return err
		}
		return g.open()
	}
	defer func() {
		if g.st != nil {
			g.st.Close()
		}
	}()

	switch cmd {
	case "", "purge":
		var o purgeOpts
		fs.BoolVar(&o.yes, "yes", false, "")
		fs.BoolVar(&o.dry, "dry-run", false, "")
		fs.BoolVar(&o.inclReview, "include-review", false, "")
		fs.BoolVar(&o.inclStale, "include-stale", false, "")
		fs.BoolVar(&o.noSync, "no-sync", false, "")
		if err := parse(); err != nil {
			return err
		}
		return cmdPurge(ctx, g, o)
	case "sync":
		if err := parse(); err != nil {
			return err
		}
		res := syncAll(ctx, g)
		if !g.multi() {
			if res[0] == nil {
				return errors.New("sync failed")
			}
			printSummary(engine.AgeFilter(res[0].Branches, g.minAge, time.Now()))
		}
		return nil
	case "list", "ls":
		all := fs.Bool("all", false, "")
		asJSON := fs.Bool("json", false, "")
		if err := parse(); err != nil {
			return err
		}
		return cmdList(ctx, g, *all, *asJSON)
	case "restore":
		last := fs.Bool("last", false, "")
		if err := parse(); err != nil {
			return err
		}
		return cmdRestore(g, *last, fs.Args())
	case "history", "log":
		if err := parse(); err != nil {
			return err
		}
		return cmdHistory(g)
	case "help":
		fmt.Print(usage)
		return nil
	case "version":
		fmt.Println("git-purge", version)
		return nil
	}
	return fmt.Errorf("unknown command %q (see git-purge --help)", cmd)
}

// syncAll syncs every target, a few at a time in multi-repo mode. A failed
// repository yields a nil result and a warning rather than aborting the rest.
func syncAll(ctx context.Context, g *globals) []*engine.Result {
	res := make([]*engine.Result, len(g.targets))
	workers := 1
	if g.multi() {
		workers = 4
		fmt.Fprintln(os.Stderr, dim(fmt.Sprintf("Syncing %d repositories…", len(g.targets))))
	}
	var mu sync.Mutex
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, t := range g.targets {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			r, err := engine.Sync(ctx, t.repo, g.st, t.rec, g.syncOptions())
			mu.Lock()
			defer mu.Unlock()
			prefix := ""
			if t.label != "" {
				prefix = t.label + ": "
			}
			if err != nil {
				warnf("%s%v", prefix, err)
				return
			}
			for _, w := range r.Warnings {
				warnf("%s%s", prefix, w)
			}
			if !g.multi() {
				for _, n := range r.Notes {
					fmt.Fprintln(os.Stderr, dim("note: "+n))
				}
			}
			res[i] = r
			if g.multi() {
				c := countStatus(engine.AgeFilter(r.Branches, g.minAge, time.Now()))
				fmt.Fprintf(os.Stderr, "  %s %-32s %s\n", green("✓"), t.label, dim(summaryLine(c)))
			}
		}()
	}
	wg.Wait()
	return res
}

// load returns each target's branches, syncing first unless asked not to
// (or, with onlyIfNeeded, only when a repository was never synced).
func load(ctx context.Context, g *globals, noSync, onlyIfNeeded bool) [][]store.Branch {
	out := make([][]store.Branch, len(g.targets))
	needSync := !noSync
	if onlyIfNeeded {
		needSync = false
		for _, t := range g.targets {
			if t.rec.LastSync.IsZero() {
				needSync = true
			}
		}
	}
	if needSync {
		for i, r := range syncAll(ctx, g) {
			if r != nil {
				out[i] = r.Branches
			}
		}
	} else {
		for i, t := range g.targets {
			b, err := g.st.Branches(t.rec.ID)
			if err != nil {
				warnf("%s: %v", t.label, err)
			}
			out[i] = b
		}
	}
	now := time.Now()
	for i := range out {
		out[i] = engine.AgeFilter(out[i], g.minAge, now)
	}
	return out
}

func header(g *globals, n int) string {
	var parts []string
	if g.multi() {
		parts = append(parts, fmt.Sprintf("%d repos", len(g.targets)))
	} else {
		parts = append(parts, "base "+g.targets[0].rec.Base)
	}
	parts = append(parts, fmt.Sprintf("%d branches", n))
	if g.minAge > 0 {
		parts = append(parts, "older than "+classify.HumanAge(g.minAge))
	}
	if g.offline {
		parts = append(parts, "offline")
	} else if !g.multi() && g.targets[0].rec.Forge != "" {
		parts = append(parts, g.targets[0].rec.Forge)
	}
	return strings.Join(parts, " · ")
}

type purgeOpts struct{ yes, dry, inclReview, inclStale, noSync bool }

func cmdPurge(ctx context.Context, g *globals, o purgeOpts) error {
	perRepo := load(ctx, g, o.noSync, o.noSync)

	var items []tui.Item
	for i, bs := range perRepo {
		for _, b := range bs {
			items = append(items, tui.Item{Branch: b, Repo: g.targets[i].label, RepoIndex: i})
		}
	}

	var chosen []tui.Item
	if o.yes {
		for _, it := range items {
			switch classify.Status(it.Status) {
			case classify.Safe:
				chosen = append(chosen, it)
			case classify.Review:
				if o.inclReview {
					chosen = append(chosen, it)
				}
			case classify.Stale:
				if o.inclStale {
					chosen = append(chosen, it)
				}
			}
		}
	} else {
		if !isTTY() {
			return errors.New("not a terminal; use --yes (deletes only 'safe' branches) or --dry-run --yes")
		}
		preview := func(it tui.Item) (*gitx.Preview, error) {
			t := g.targets[it.RepoIndex]
			prHead := ""
			if it.PRState == forge.StateMerged {
				prHead = it.PRHeadSHA
			}
			return t.repo.BranchPreview(t.rec.Base, it.SHA, prHead)
		}
		var err error
		if chosen, err = tui.Run(items, tui.Options{Header: header(g, len(items)), Preview: preview}); err != nil {
			return err
		}
		if chosen == nil {
			fmt.Println("Nothing deleted.")
			return nil
		}
	}
	if len(chosen) == 0 {
		fmt.Println("Nothing to purge.")
		return nil
	}

	// Group by repository; one batch id across all of them so
	// `restore --last --all-repos` undoes the whole run.
	byRepo := map[int][]store.Branch{}
	var order []int
	for _, it := range chosen {
		if _, ok := byRepo[it.RepoIndex]; !ok {
			order = append(order, it.RepoIndex)
		}
		byRepo[it.RepoIndex] = append(byRepo[it.RepoIndex], it.Branch)
	}
	batch := store.NewBatch()
	deleted, failed := 0, 0
	for _, i := range order {
		t := g.targets[i]
		if t.label != "" {
			fmt.Println(bold(t.label))
		}
		for _, oc := range engine.Purge(t.repo, g.st, t.rec, byRepo[i], batch, o.dry) {
			switch {
			case oc.Err != nil:
				failed++
				fmt.Printf("  %s %s: %v\n", red("✗"), oc.Branch.Name, oc.Err)
			case o.dry:
				fmt.Printf("  would delete %s  %s\n", oc.Branch.Name, dim("("+oc.Branch.Status+": "+oc.Branch.Reason+")"))
			default:
				deleted++
				fmt.Printf("  %s deleted %s %s\n", green("✓"), oc.Branch.Name, dim("(was "+oc.Branch.SHA[:10]+")"))
			}
		}
	}
	if deleted > 0 {
		undo := "git purge restore --last"
		if g.multi() {
			undo += " --all-repos " + g.allRepos
		}
		fmt.Printf("\nDeleted %d branch(es). Undo with: %s\n", deleted, undo)
	}
	if failed > 0 {
		return fmt.Errorf("%d branch(es) could not be deleted", failed)
	}
	return nil
}

func cmdList(ctx context.Context, g *globals, all, asJSON bool) error {
	perRepo := load(ctx, g, true, true)
	type entry struct {
		t *target
		b store.Branch
	}
	var entries []entry
	for i, bs := range perRepo {
		for _, b := range bs {
			if all || classify.Status(b.Status).Deletable() {
				entries = append(entries, entry{g.targets[i], b})
			}
		}
	}

	if asJSON {
		type out struct {
			Repo       string    `json:"repo"`
			Name       string    `json:"name"`
			SHA        string    `json:"sha"`
			Status     string    `json:"status"`
			Reason     string    `json:"reason"`
			Upstream   string    `json:"upstream,omitempty"`
			GoneRemote bool      `json:"dead_on_remote"`
			PRNumber   int       `json:"pr,omitempty"`
			PRState    string    `json:"pr_state,omitempty"`
			PRURL      string    `json:"pr_url,omitempty"`
			Committed  time.Time `json:"committed_at"`
		}
		list := []out{}
		for _, e := range entries {
			b := e.b
			o := out{e.t.repo.Dir, b.Name, b.SHA, b.Status, b.Reason, b.Upstream, b.Gone, b.PRNumber, b.PRState, b.PRURL, b.CommitTime}
			if o.PRState == "none" {
				o.PRState = ""
			}
			list = append(list, o)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}

	if !g.multi() {
		fmt.Fprintln(os.Stderr, dim(fmt.Sprintf("last synced %s ago", time.Since(g.targets[0].rec.LastSync).Round(time.Second))))
	}
	if len(entries) == 0 {
		fmt.Println("No dead branches. (use --all to see every branch)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	cols := "STATUS\tBRANCH\tREMOTE\tAGE\tREASON"
	if g.multi() {
		cols = "REPO\t" + cols
	}
	fmt.Fprintln(tw, cols)
	var flat []store.Branch
	for _, e := range entries {
		b := e.b
		flat = append(flat, b)
		remote := "-"
		switch {
		case b.Gone:
			remote = "gone"
		case b.Upstream != "":
			remote = "live"
		}
		if g.multi() {
			fmt.Fprintf(tw, "%s\t", e.t.label)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", b.Status, b.Name, remote, tui.Age(b.CommitTime), b.Reason)
	}
	tw.Flush()
	printSummary(flat)
	return nil
}

func countStatus(branches []store.Branch) map[string]int {
	c := map[string]int{}
	for _, b := range branches {
		c[b.Status]++
	}
	return c
}

func summaryLine(c map[string]int) string {
	s := fmt.Sprintf("%d safe · %d review", c["safe"], c["review"])
	if c["stale"] > 0 {
		s += fmt.Sprintf(" · %d stale", c["stale"])
	}
	return s + fmt.Sprintf(" · %d active · %d local-only · %d protected", c["active"], c["local"], c["protected"])
}

func printSummary(branches []store.Branch) {
	c := countStatus(branches)
	line := summaryLine(c)
	line = strings.Replace(line, fmt.Sprintf("%d safe", c["safe"]), green(fmt.Sprintf("%d safe", c["safe"])), 1)
	line = strings.Replace(line, fmt.Sprintf("%d review", c["review"]), yellow(fmt.Sprintf("%d review", c["review"])), 1)
	fmt.Println("\n" + line)
	if c["safe"]+c["review"]+c["stale"] > 0 {
		fmt.Println("Run `git purge` to choose what to delete.")
	}
}

func cmdRestore(g *globals, last bool, names []string) error {
	type job struct {
		t *target
		p store.Purged
	}
	var jobs []job
	switch {
	case last:
		// The newest batch across all targets; a multi-repo purge shares one batch id.
		var newest int64
		perTarget := make([][]store.Purged, len(g.targets))
		for i, t := range g.targets {
			entries, err := g.st.LastBatch(t.rec.ID)
			if err != nil {
				return err
			}
			perTarget[i] = entries
			for _, p := range entries {
				newest = max(newest, p.Batch)
			}
		}
		for i, entries := range perTarget {
			for _, p := range entries {
				if p.Batch == newest {
					jobs = append(jobs, job{g.targets[i], p})
				}
			}
		}
		if len(jobs) == 0 {
			return errors.New("nothing to restore")
		}
	case len(names) > 0:
		if g.multi() {
			return errors.New("restoring by name works on one repository; use -C <repo> instead of --all-repos")
		}
		t := g.targets[0]
		for _, n := range names {
			p, err := g.st.LatestPurged(t.rec.ID, n)
			if err != nil {
				return err
			}
			if p == nil {
				return fmt.Errorf("no purge record for %q (see git purge history)", n)
			}
			jobs = append(jobs, job{t, *p})
		}
	default:
		return errors.New("usage: git purge restore --last | <branch>...")
	}
	failed := 0
	for _, j := range jobs {
		name := j.p.Name
		if j.t.label != "" {
			name = j.t.label + " › " + name
		}
		if err := j.t.repo.RestoreBranch(j.p.Name, j.p.SHA, j.p.Remote, j.p.MergeRef); err != nil {
			failed++
			fmt.Printf("  %s %s: %v\n", red("✗"), name, err)
			continue
		}
		_ = g.st.MarkRestored(j.p.ID)
		fmt.Printf("  %s restored %s at %s\n", green("✓"), name, j.p.SHA[:10])
	}
	if failed > 0 {
		return fmt.Errorf("%d branch(es) could not be restored", failed)
	}
	return nil
}

func cmdHistory(g *globals) error {
	type entry struct {
		label string
		p     store.Purged
	}
	var all []entry
	for _, t := range g.targets {
		entries, err := g.st.History(t.rec.ID, 200)
		if err != nil {
			return err
		}
		for _, p := range entries {
			all = append(all, entry{t.label, p})
		}
	}
	if len(all) == 0 {
		fmt.Println("No branches purged yet.")
		return nil
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].p.PurgedAt.After(all[j].p.PurgedAt) })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	cols := "PURGED\tBRANCH\tSHA\tSTATUS\tRESTORED"
	if g.multi() {
		cols = "PURGED\tREPO\tBRANCH\tSHA\tSTATUS\tRESTORED"
	}
	fmt.Fprintln(tw, cols)
	for _, e := range all {
		p := e.p
		restored := ""
		if !p.RestoredAt.IsZero() {
			restored = p.RestoredAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t", p.PurgedAt.Format("2006-01-02 15:04"))
		if g.multi() {
			fmt.Fprintf(tw, "%s\t", e.label)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, p.SHA[:10], p.Status, restored)
	}
	return tw.Flush()
}

func warnf(format string, a ...any) {
	fmt.Fprintln(os.Stderr, yellow("warning:")+" "+fmt.Sprintf(format, a...))
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	in, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0 && in.Mode()&os.ModeCharDevice != 0
}

var color = os.Getenv("NO_COLOR") == "" && isTTY()

func paint(code, s string) string {
	if !color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func dim(s string) string    { return paint("2", s) }
func bold(s string) string   { return paint("1", s) }
func red(s string) string    { return paint("31", s) }
func green(s string) string  { return paint("32", s) }
func yellow(s string) string { return paint("33", s) }
