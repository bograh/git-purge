// Package scan finds git repositories below a directory.
package scan

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skipDirs are never descended into: large, and never contain repos you own.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".venv": true, "venv": true, "target": true,
	"dist": true, "build": true, ".cache": true, "Library": true, ".Trash": true,
}

// Repos returns working-tree roots of git repositories under root, searching at
// most maxDepth directory levels below it. A repository's own subdirectories are
// not searched (submodules and nested clones are skipped), and linked worktrees
// (".git" files) are skipped because they share their main repo's branches.
func Repos(root string, maxDepth int) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []string
	base := strings.Count(root, string(os.PathSeparator))
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return fs.SkipDir // unreadable: skip quietly
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (skipDirs[name] || (strings.HasPrefix(name, ".") && name != ".git")) {
			return fs.SkipDir
		}
		if fi, err := os.Stat(filepath.Join(path, ".git")); err == nil && fi.IsDir() {
			out = append(out, path)
			return fs.SkipDir
		}
		if strings.Count(path, string(os.PathSeparator))-base >= maxDepth {
			return fs.SkipDir
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
