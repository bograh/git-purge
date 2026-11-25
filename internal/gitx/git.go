// Package gitx is a thin, dependency-free wrapper around the git CLI.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo is a handle on a local git repository.
type Repo struct {
	// Dir is the working directory commands run in.
	Dir string
	// CommonDir is the absolute path of the shared .git directory. Worktrees of
	// the same repository share it, so it is used as the repository's identity.
	CommonDir string
}

// Branch is a local branch and everything git knows about it.
type Branch struct {
	Name           string
	SHA            string
	Upstream       string // e.g. "origin/feature-x" (still set when the remote branch is gone)
	UpstreamRemote string // e.g. "origin"
	UpstreamRef    string // e.g. "refs/heads/feature-x"
	Gone           bool   // upstream configured but no longer exists on the remote
	Ahead, Behind  int
	CommitTime     time.Time
	Subject        string
	IsHead         bool   // checked out in this worktree
	WorktreePath   string // non-empty when checked out in any worktree
}

// UpstreamBranch returns the remote-side branch name (refs/heads/ stripped).
func (b Branch) UpstreamBranch() string {
	return strings.TrimPrefix(b.UpstreamRef, "refs/heads/")
}

// Open finds the repository containing dir.
func Open(dir string) (*Repo, error) {
	r := &Repo{Dir: dir}
	top, err := r.Run("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git repository (or no work tree): %s", dir)
	}
	r.Dir = top
	common, err := r.Run("rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(top, common)
	}
	r.CommonDir = filepath.Clean(common)
	return r, nil
}

// Run executes git with args and returns trimmed stdout.
func (r *Repo) Run(args ...string) (string, error) {
	return r.RunEnv(nil, args...)
}

// RunEnv is Run with extra environment variables.
func (r *Repo) RunEnv(env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimRight(stdout.String(), "\n"), &Error{Args: args, Msg: msg, Err: err}
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Error is a failed git invocation.
type Error struct {
	Args []string
	Msg  string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), e.Msg) }
func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns the process exit code of a failed git command, or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// FetchPrune fetches every remote and prunes remote-tracking branches that were
// deleted upstream. This is what turns merged-and-deleted branches into "gone".
func (r *Repo) FetchPrune() error {
	_, err := r.Run("fetch", "--all", "--prune", "--quiet")
	return err
}

const sep = "\x00"

// Branches lists all local branches.
func (r *Repo) Branches() ([]Branch, error) {
	format := strings.Join([]string{
		"%(refname:short)",
		"%(objectname)",
		"%(upstream:short)",
		"%(upstream:track)",
		"%(committerdate:unix)",
		"%(HEAD)",
		"%(worktreepath)",
		"%(upstream:remotename)",
		"%(upstream:remoteref)",
		"%(contents:subject)",
	}, "%00")
	out, err := r.Run("for-each-ref", "--format="+format, "refs/heads")
	if err != nil {
		return nil, err
	}
	var branches []Branch
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, sep)
		if len(f) < 10 {
			return nil, fmt.Errorf("unexpected for-each-ref output: %q", line)
		}
		ts, _ := strconv.ParseInt(f[4], 10, 64)
		b := Branch{
			Name:           f[0],
			SHA:            f[1],
			Upstream:       f[2],
			CommitTime:     time.Unix(ts, 0),
			IsHead:         f[5] == "*",
			WorktreePath:   f[6],
			UpstreamRemote: f[7],
			UpstreamRef:    f[8],
			Subject:        f[9],
		}
		b.Gone, b.Ahead, b.Behind = parseTrack(f[3])
		branches = append(branches, b)
	}
	return branches, nil
}

// parseTrack parses %(upstream:track): "[gone]", "[ahead 1, behind 2]", "".
func parseTrack(s string) (gone bool, ahead, behind int) {
	s = strings.Trim(s, "[]")
	if s == "gone" {
		return true, 0, 0
	}
	for _, part := range strings.Split(s, ",") {
		fields := strings.Fields(part)
		if len(fields) != 2 {
			continue
		}
		n, _ := strconv.Atoi(fields[1])
		switch fields[0] {
		case "ahead":
			ahead = n
		case "behind":
			behind = n
		}
	}
	return false, ahead, behind
}

// DefaultBranch returns the ref that branches are merged into, preferring the
// remote-tracking ref (e.g. "origin/main") because it is the freshest after a
// fetch. It also returns the bare branch name (e.g. "main").
func (r *Repo) DefaultBranch() (ref, name string, err error) {
	if out, err := r.Run("config", "--get", "gitpurge.base"); err == nil && out != "" {
		return out, strings.TrimPrefix(out, "origin/"), nil
	}
	if out, err := r.Run("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		_, n, _ := strings.Cut(out, "/")
		return out, n, nil
	}
	for _, cand := range []string{"main", "master", "trunk", "develop"} {
		if r.RefExists("refs/remotes/origin/" + cand) {
			return "origin/" + cand, cand, nil
		}
	}
	for _, cand := range []string{"main", "master", "trunk", "develop"} {
		if r.RefExists("refs/heads/" + cand) {
			return cand, cand, nil
		}
	}
	return "", "", errors.New("could not determine the default branch; set it with: git config gitpurge.base origin/main")
}

// RefExists reports whether a fully qualified ref exists.
func (r *Repo) RefExists(ref string) bool {
	_, err := r.Run("show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// CommitExists reports whether sha names a commit present in the object store.
func (r *Repo) CommitExists(sha string) bool {
	if sha == "" {
		return false
	}
	_, err := r.Run("cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// MergedInto returns the set of local branch names whose tips are reachable
// from base (classic merge or fast-forward).
func (r *Repo) MergedInto(base string) (map[string]bool, error) {
	out, err := r.Run("for-each-ref", "--merged="+base, "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			set[l] = true
		}
	}
	return set, nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(a, b string) bool {
	_, err := r.Run("merge-base", "--is-ancestor", a, b)
	return err == nil
}

// CountCommits counts commits reachable from to but not from from.
func (r *Repo) CountCommits(from, to string) (int, error) {
	out, err := r.Run("rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// SquashMerged detects branches that landed on base via "squash and merge" or
// "rebase and merge", which ordinary ancestry checks cannot see.
//
// Rebase-merge: every commit of the branch has a patch-equivalent commit on base.
// Squash-merge: a synthetic commit holding the branch's whole diff (its tree on
// top of the merge-base) is patch-equivalent to a commit on base.
func (r *Repo) SquashMerged(base, sha string) (bool, error) {
	mb, err := r.Run("merge-base", base, sha)
	if err != nil {
		return false, nil // unrelated histories: cannot have been merged
	}
	if mb == sha {
		return true, nil
	}

	// Rebase-merge check.
	if out, err := r.Run("cherry", base, sha, mb); err == nil && out != "" && allPatchEquivalent(out) {
		return true, nil
	}

	// Squash-merge check.
	tree, err := r.Run("rev-parse", sha+"^{tree}")
	if err != nil {
		return false, err
	}
	if baseTree, err := r.Run("rev-parse", mb+"^{tree}"); err == nil && baseTree == tree {
		return false, nil // branch has no net change; nothing to compare
	}
	env := []string{
		"GIT_AUTHOR_NAME=git-purge", "GIT_AUTHOR_EMAIL=git-purge@localhost",
		"GIT_COMMITTER_NAME=git-purge", "GIT_COMMITTER_EMAIL=git-purge@localhost",
	}
	tmp, err := r.RunEnv(env, "commit-tree", tree, "-p", mb, "-m", "git-purge squash probe")
	if err != nil {
		return false, err
	}
	out, err := r.Run("cherry", base, tmp, mb)
	if err != nil {
		return false, err
	}
	return out != "" && allPatchEquivalent(out), nil
}

func allPatchEquivalent(cherryOut string) bool {
	for _, l := range strings.Split(cherryOut, "\n") {
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "- ") {
			return false
		}
	}
	return true
}

// RemoteURL returns the fetch URL of a remote.
func (r *Repo) RemoteURL(remote string) (string, error) {
	return r.Run("remote", "get-url", remote)
}

// HasRemote reports whether a remote with this name is configured.
func (r *Repo) HasRemote(remote string) bool {
	_, err := r.RemoteURL(remote)
	return err == nil
}

// ConfigAll returns all values of a multi-valued config key.
func (r *Repo) ConfigAll(key string) []string {
	out, err := r.Run("config", "--get-all", key)
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// DeleteBranch deletes refs/heads/name only if it still points at expectSHA,
// so work committed after the last sync can never be lost. It also removes the
// branch's config section (upstream tracking), like `git branch -D` would.
func (r *Repo) DeleteBranch(name, expectSHA string) error {
	if _, err := r.Run("update-ref", "-d", "refs/heads/"+name, expectSHA); err != nil {
		return fmt.Errorf("branch %s moved since last sync (expected %s); re-run sync", name, short(expectSHA))
	}
	_, _ = r.Run("config", "--remove-section", "branch."+name)
	return nil
}

// RestoreBranch recreates a deleted branch at sha (failing if it already
// exists) and restores its upstream tracking config when known.
func (r *Repo) RestoreBranch(name, sha, remote, mergeRef string) error {
	if !r.CommitExists(sha) {
		return fmt.Errorf("commit %s for %s is no longer in the object store (garbage collected?)", short(sha), name)
	}
	if _, err := r.Run("update-ref", "refs/heads/"+name, sha, ""); err != nil {
		return fmt.Errorf("cannot restore %s: a branch with that name already exists", name)
	}
	if remote != "" && mergeRef != "" {
		_, _ = r.Run("config", "branch."+name+".remote", remote)
		_, _ = r.Run("config", "branch."+name+".merge", mergeRef)
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// Preview describes what a branch would take with it if deleted.
type Preview struct {
	Base        string
	MergeBase   string
	Unmerged    []string // "sha subject (age)" for commits on the branch but not reachable from base
	UnmergedN   int
	NotInPR     []string // commits beyond the merged PR head, when known
	NotInPRN    int
	DiffStat    string // merge-base..branch
	PatchMerged bool   // every unmerged commit has a patch-equivalent on base
}

const previewLimit = 30

// BranchPreview gathers commits and the diffstat a branch carries relative to base.
func (r *Repo) BranchPreview(base, sha, prHead string) (*Preview, error) {
	p := &Preview{Base: base}
	mb, err := r.Run("merge-base", base, sha)
	if err != nil {
		return nil, fmt.Errorf("no common history with %s", base)
	}
	p.MergeBase = mb
	log := func(from, to string) ([]string, int) {
		n, _ := r.CountCommits(from, to)
		out, err := r.Run("log", "--no-color", fmt.Sprintf("-%d", previewLimit), "--format=%h %s (%cr)", from+".."+to)
		if err != nil || out == "" {
			return nil, n
		}
		return strings.Split(out, "\n"), n
	}
	p.Unmerged, p.UnmergedN = log(base, sha)
	if prHead != "" && prHead != sha && r.CommitExists(prHead) {
		p.NotInPR, p.NotInPRN = log(prHead, sha)
	}
	if p.UnmergedN > 0 {
		if out, err := r.Run("cherry", base, sha, mb); err == nil {
			p.PatchMerged = out != "" && allPatchEquivalent(out)
		}
	}
	p.DiffStat, _ = r.Run("diff", "--no-color", "--stat=100", mb, sha)
	return p, nil
}
