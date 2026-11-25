package scan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRepos(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("a/.git")
	mk("a/nested/.git")           // inside a repo: skipped
	mk("group/b/.git")            // depth 2
	mk("group/deep/er/c/.git")    // depth 4: beyond maxDepth 3
	mk("web/node_modules/x/.git") // skip dir
	mk(".hidden/d/.git")          // hidden dir
	mk("wt")                      // linked worktree: .git is a file
	os.WriteFile(filepath.Join(root, "wt/.git"), []byte("gitdir: /x"), 0o644)

	got, err := Repos(root, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "a"), filepath.Join(root, "group/b")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if got, _ := Repos(filepath.Join(root, "a"), 3); len(got) != 1 {
		t.Fatalf("root itself should be found: %v", got)
	}
}
