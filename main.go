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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/engine"
	"github.com/bograh/git-purge/internal/gitx"
	"github.com/bograh/git-purge/internal/store"
	"github.com/bograh/git-purge/internal/tui"
)

var version = "dev"

const usage = `git-purge — safely clear out dead local branches

Usage:
  git purge [flags]               sync, then pick branches to delete interactively
  git purge sync [flags]          fetch --prune, classify branches, update the database
  git purge list [--all] [--json] show the last synced classification
  git purge purge [--yes] [--dry-run] [--include-review]
                                  delete branches (interactive unless --yes)
  git purge restore --last | <branch>...
                                  bring back purged branches
  git purge history               show the purge log

Statuses:
  safe       every commit is already on the base branch (merged, squash-merged,
             rebase-merged, or the local tip is part of a merged PR)
  review     dead on remote but no proof the work landed — look before deleting
  active     still exists on the remote, not merged            (never deleted)
  local      never pushed, not merged                          (never deleted)
  protected  default/current/worktree branch or protect pattern (never deleted)

Common flags:
  -C <dir>          run as if started in <dir>
  --offline         skip GitHub/GitLab API lookups (git-only evidence)
  --no-fetch        don't run git fetch --all --prune first
  --protect <glob>  never touch matching branches (repeatable; also: git config --add gitpurge.protect <glob>)
  --db <path>       database path (default: $GIT_PURGE_DB or ~/.local/share/git-purge/state.db)

Auth: GitHub uses GH_TOKEN / GITHUB_TOKEN or 'gh auth token'; GitLab uses GITLAB_TOKEN.
`

type globals struct {
	dir      string
	offline  bool
	noFetch  bool
	protect  multi
	db       string
	repo     *gitx.Repo
	st       *store.Store
	rec      *store.Repo
	syncOpts engine.Options
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.dir, "C", ".", "")
	fs.BoolVar(&g.offline, "offline", false, "")
	fs.BoolVar(&g.noFetch, "no-fetch", false, "")
	fs.Var(&g.protect, "protect", "")
	fs.StringVar(&g.db, "db", "", "")
}

func (g *globals) open() error {
	repo, err := gitx.Open(g.dir)
	if err != nil {
		return err
	}
	path := g.db
	if path == "" {
		if path, err = store.DefaultPath(); err != nil {
			return err
		}
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	rec, err := st.Repo(repo.CommonDir)
	if err != nil {
		st.Close()
		return err
	}
	g.repo, g.st, g.rec = repo, st, rec
	g.syncOpts = engine.Options{
		NoFetch: g.noFetch,
		Offline: g.offline,
		Protect: g.protect,
		Progress: func(f string, a ...any) {
			fmt.Fprintln(os.Stderr, dim(fmt.Sprintf(f, a...)))
		},
	}
	return nil
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

	switch cmd {
	case "", "purge":
		yes := fs.Bool("yes", false, "")
		dry := fs.Bool("dry-run", false, "")
		inclReview := fs.Bool("include-review", false, "")
		noSync := fs.Bool("no-sync", false, "")
		if err := fs.Parse(args); err != nil {
			return quietHelp(err)
		}
		if err := g.open(); err != nil {
			return err
		}
		defer g.st.Close()
		return cmdPurge(ctx, g, *yes, *dry, *inclReview, *noSync)
	case "sync":
		if err := fs.Parse(args); err != nil {
			return quietHelp(err)
		}
		if err := g.open(); err != nil {
			return err
		}
		defer g.st.Close()
		res, err := doSync(ctx, g)
		if err != nil {
			return err
		}
		printSummary(res.Branches)
		return nil
	case "list", "ls":
		all := fs.Bool("all", false, "")
		asJSON := fs.Bool("json", false, "")
		if err := fs.Parse(args); err != nil {
			return quietHelp(err)
		}
		if err := g.open(); err != nil {
			return err
		}
		defer g.st.Close()
		return cmdList(ctx, g, *all, *asJSON)
	case "restore":
		last := fs.Bool("last", false, "")
		if err := fs.Parse(args); err != nil {
			return quietHelp(err)
		}
		if err := g.open(); err != nil {
			return err
		}
		defer g.st.Close()
		return cmdRestore(g, *last, fs.Args())
	case "history", "log":
		if err := fs.Parse(args); err != nil {
			return quietHelp(err)
		}
		if err := g.open(); err != nil {
			return err
		}
		defer g.st.Close()
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

func quietHelp(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	return err
}

func doSync(ctx context.Context, g *globals) (*engine.Result, error) {
	res, err := engine.Sync(ctx, g.repo, g.st, g.rec, g.syncOpts)
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	return res, nil
}

func header(g *globals, n int) string {
	h := fmt.Sprintf("base %s · %d branches", g.rec.Base, n)
	if g.rec.Forge != "" {
		h += " · " + g.rec.Forge
	} else {
		h += " · offline"
	}
	return h
}

func cmdPurge(ctx context.Context, g *globals, yes, dry, inclReview, noSync bool) error {
	var branches []store.Branch
	if noSync && !g.rec.LastSync.IsZero() {
		var err error
		if branches, err = g.st.Branches(g.rec.ID); err != nil {
			return err
		}
	} else {
		res, err := doSync(ctx, g)
		if err != nil {
			return err
		}
		branches = res.Branches
	}

	var chosen []store.Branch
	if yes {
		for _, b := range branches {
			if b.Status == string(classify.Safe) || (inclReview && b.Status == string(classify.Review)) {
				chosen = append(chosen, b)
			}
		}
	} else {
		if !isTTY() {
			return errors.New("not a terminal; use --yes (deletes only 'safe' branches) or --dry-run")
		}
		var err error
		if chosen, err = tui.Run(branches, header(g, len(branches))); err != nil {
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

	failed := 0
	for _, o := range engine.Purge(g.repo, g.st, g.rec, chosen, dry) {
		switch {
		case o.Err != nil:
			failed++
			fmt.Printf("  %s %s: %v\n", red("✗"), o.Branch.Name, o.Err)
		case dry:
			fmt.Printf("  would delete %s  (%s: %s)\n", o.Branch.Name, o.Branch.Status, o.Branch.Reason)
		default:
			fmt.Printf("  %s deleted %s %s\n", green("✓"), o.Branch.Name, dim("(was "+o.Branch.SHA[:10]+")"))
		}
	}
	if !dry && len(chosen) > failed {
		fmt.Printf("\nDeleted %d branch(es). Undo with: git purge restore --last\n", len(chosen)-failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d branch(es) could not be deleted", failed)
	}
	return nil
}

func cmdList(ctx context.Context, g *globals, all, asJSON bool) error {
	if g.rec.LastSync.IsZero() {
		if _, err := doSync(ctx, g); err != nil {
			return err
		}
	}
	branches, err := g.st.Branches(g.rec.ID)
	if err != nil {
		return err
	}
	if !all {
		var f []store.Branch
		for _, b := range branches {
			if classify.Status(b.Status).Deletable() {
				f = append(f, b)
			}
		}
		branches = f
	}
	if asJSON {
		type out struct {
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
		for _, b := range branches {
			o := out{b.Name, b.SHA, b.Status, b.Reason, b.Upstream, b.Gone, b.PRNumber, b.PRState, b.PRURL, b.CommitTime}
			if o.PRState == "none" {
				o.PRState = ""
			}
			list = append(list, o)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	fmt.Fprintln(os.Stderr, dim(fmt.Sprintf("last synced %s ago", time.Since(g.rec.LastSync).Round(time.Second))))
	if len(branches) == 0 {
		fmt.Println("No dead branches. (use --all to see every branch)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tBRANCH\tREMOTE\tAGE\tREASON")
	for _, b := range branches {
		remote := "-"
		switch {
		case b.Gone:
			remote = "gone"
		case b.Upstream != "":
			remote = "live"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", b.Status, b.Name, remote, tui.Age(b.CommitTime), b.Reason)
	}
	tw.Flush()
	printSummary(branches)
	return nil
}

func printSummary(branches []store.Branch) {
	c := map[string]int{}
	for _, b := range branches {
		c[b.Status]++
	}
	fmt.Printf("\n%s · %s · %d active · %d local-only · %d protected\n",
		green(fmt.Sprintf("%d safe", c["safe"])), yellow(fmt.Sprintf("%d review", c["review"])), c["active"], c["local"], c["protected"])
	if c["safe"]+c["review"] > 0 {
		fmt.Println("Run `git purge` to choose what to delete.")
	}
}

func cmdRestore(g *globals, last bool, names []string) error {
	var entries []store.Purged
	switch {
	case last:
		var err error
		if entries, err = g.st.LastBatch(g.rec.ID); err != nil {
			return err
		}
		if len(entries) == 0 {
			return errors.New("nothing to restore")
		}
	case len(names) > 0:
		for _, n := range names {
			p, err := g.st.LatestPurged(g.rec.ID, n)
			if err != nil {
				return err
			}
			if p == nil {
				return fmt.Errorf("no purge record for %q (see git purge history)", n)
			}
			entries = append(entries, *p)
		}
	default:
		return errors.New("usage: git purge restore --last | <branch>...")
	}
	failed := 0
	for _, p := range entries {
		if err := g.repo.RestoreBranch(p.Name, p.SHA, p.Remote, p.MergeRef); err != nil {
			failed++
			fmt.Printf("  %s %v\n", red("✗"), err)
			continue
		}
		_ = g.st.MarkRestored(p.ID)
		fmt.Printf("  %s restored %s at %s\n", green("✓"), p.Name, p.SHA[:10])
	}
	if failed > 0 {
		return fmt.Errorf("%d branch(es) could not be restored", failed)
	}
	return nil
}

func cmdHistory(g *globals) error {
	entries, err := g.st.History(g.rec.ID, 200)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("No branches purged yet.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PURGED\tBRANCH\tSHA\tSTATUS\tRESTORED")
	for _, p := range entries {
		restored := ""
		if !p.RestoredAt.IsZero() {
			restored = p.RestoredAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.PurgedAt.Format("2006-01-02 15:04"), p.Name, p.SHA[:10], p.Status, restored)
	}
	return tw.Flush()
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
func red(s string) string    { return paint("31", s) }
func green(s string) string  { return paint("32", s) }
func yellow(s string) string { return paint("33", s) }
