package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bograh/git-purge/internal/gitx"
	"github.com/bograh/git-purge/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", file)
	git(t, dir, "commit", "-qm", "edit "+file)
}

// scenario builds a clone of a bare "remote" with one branch per case.
func scenario(t *testing.T) string {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	git(t, root, "init", "-q", "--bare", "-b", "main", remote)
	git(t, root, "clone", "-q", remote, work)
	git(t, work, "checkout", "-qb", "main")
	commit(t, work, "README", "hello\n")
	git(t, work, "push", "-qu", "origin", "main")
	git(t, work, "remote", "set-head", "origin", "main")

	push := func(b string) { git(t, work, "push", "-qu", "origin", b) }
	branch := func(b string) { git(t, work, "checkout", "-qb", b, "main") }

	// merged: classic merge commit, remote branch deleted.
	branch("feat/merged")
	commit(t, work, "merged.txt", "m\n")
	push("feat/merged")
	// squashed: squash-merged, remote deleted.
	branch("feat/squashed")
	commit(t, work, "sq.txt", "a\n")
	commit(t, work, "sq.txt", "a\nb\n")
	push("feat/squashed")
	// rebased: rebase-merged (commits cherry-picked).
	branch("feat/rebased")
	commit(t, work, "rb1.txt", "1\n")
	commit(t, work, "rb2.txt", "2\n")
	push("feat/rebased")
	// abandoned: pushed, remote deleted, never merged.
	branch("feat/abandoned")
	commit(t, work, "ab.txt", "x\n")
	push("feat/abandoned")
	// extra: squash-merged, but a later local commit never landed.
	branch("feat/extra")
	commit(t, work, "ex.txt", "1\n")
	push("feat/extra")
	// active: live on remote, not merged.
	branch("feat/active")
	commit(t, work, "act.txt", "1\n")
	push("feat/active")
	// local: never pushed.
	branch("wip/local")
	commit(t, work, "loc.txt", "1\n")
	// protected by pattern.
	branch("release/1.0")
	commit(t, work, "rel.txt", "1\n")

	git(t, work, "checkout", "-q", "main")
	git(t, work, "merge", "-q", "--no-ff", "-m", "merge", "feat/merged")
	git(t, work, "merge", "-q", "--squash", "feat/squashed")
	git(t, work, "commit", "-qm", "squash")
	git(t, work, "cherry-pick", "main..feat/rebased")
	git(t, work, "merge", "-q", "--squash", "feat/extra")
	git(t, work, "commit", "-qm", "squash extra")
	git(t, work, "push", "-q", "origin", "main")
	for _, b := range []string{"feat/merged", "feat/squashed", "feat/rebased", "feat/abandoned", "feat/extra"} {
		git(t, work, "push", "-q", "origin", "--delete", b)
	}
	git(t, work, "checkout", "-q", "feat/extra")
	commit(t, work, "ex2.txt", "later\n")
	git(t, work, "checkout", "-q", "main")
	return work
}

func TestSyncPurgeRestore(t *testing.T) {
	work := scenario(t)
	repo, err := gitx.Open(work)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, err := st.Repo(repo.CommonDir)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Sync(context.Background(), repo, st, rec, Options{Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Base != "origin/main" {
		t.Fatalf("base = %q", res.Base)
	}
	got := map[string]store.Branch{}
	for _, b := range res.Branches {
		got[b.Name] = b
	}
	want := map[string]string{
		"main":           "protected",
		"release/1.0":    "protected",
		"feat/merged":    "safe",
		"feat/squashed":  "safe",
		"feat/rebased":   "safe",
		"feat/abandoned": "review",
		"feat/extra":     "review",
		"feat/active":    "active",
		"wip/local":      "local",
	}
	for name, status := range want {
		b, ok := got[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		if b.Status != status {
			t.Errorf("%s: status %s (%s), want %s", name, b.Status, b.Reason, status)
		}
	}
	if !got["feat/abandoned"].Gone || got["feat/active"].Gone {
		t.Errorf("gone flags wrong: abandoned=%v active=%v", got["feat/abandoned"].Gone, got["feat/active"].Gone)
	}

	// A branch that moved after sync must not be deleted.
	git(t, work, "checkout", "-q", "feat/squashed")
	commit(t, work, "new.txt", "new work\n")
	git(t, work, "checkout", "-q", "main")

	out := Purge(repo, st, rec, []store.Branch{got["feat/merged"], got["feat/squashed"], got["feat/active"]}, false)
	if out[0].Err != nil {
		t.Fatalf("merged: %v", out[0].Err)
	}
	if out[1].Err == nil {
		t.Fatal("squashed branch moved after sync but was deleted")
	}
	if out[2].Err == nil {
		t.Fatal("active branch was deleted")
	}
	if repo.RefExists("refs/heads/feat/merged") {
		t.Fatal("feat/merged still exists")
	}
	if !repo.RefExists("refs/heads/feat/squashed") || !repo.RefExists("refs/heads/feat/active") {
		t.Fatal("protected branches were deleted")
	}

	batch, err := st.LastBatch(rec.ID)
	if err != nil || len(batch) != 1 {
		t.Fatalf("last batch = %v, %v", batch, err)
	}
	p := batch[0]
	if err := repo.RestoreBranch(p.Name, p.SHA, p.Remote, p.MergeRef); err != nil {
		t.Fatal(err)
	}
	if sha := git(t, work, "rev-parse", "feat/merged"); sha != got["feat/merged"].SHA {
		t.Fatalf("restored at %s, want %s", sha, got["feat/merged"].SHA)
	}
	if up := git(t, work, "config", "branch.feat/merged.merge"); up != "refs/heads/feat/merged" {
		t.Fatalf("upstream config not restored: %q", up)
	}
}
