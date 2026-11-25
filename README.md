# git-purge

Safely clear out the hundreds of dead local branches left behind after PRs merge.

When a PR merges, GitHub/GitLab delete the remote branch, but your local copy
lives forever. `git-purge` syncs a local SQLite database with your remotes and
your forge, works out which branches are **dead on remote** and whether their
work actually landed, and lets you delete them from an interactive picker.
Every deletion can be undone.

```
 git-purge   origin/main · GitHub · 214 branches            ● 131 safe  ● 9 review  ○ 74 kept

  SAFE TO DELETE ───────────────────────────────────────────────────────────────
▌ ✓  feat/login-flow     ✗ gone   3mo  PR #482 merged; local tip is part of it
  ✓  fix/typo            ✗ gone   5mo  squash/rebase-merged into origin/main

  NEEDS REVIEW ─────────────────────────────────────────────────────────────────
  ○  spike/new-cache     ✗ gone   8mo  PR #377 closed without merging
  ○  feat/billing        ✗ gone   2mo  PR #501 merged, but 2 local commit(s) were never in it
╭──────────────────────────────────────────────────────────────────────────────╮
│ feat/login-flow  ● safe                                                      │
│ a1b2c3d4e5 · Aug 22 2026 · origin/feat/login-flow deleted on remote          │
│ “Add OAuth login flow”                                                       │
│ → PR #482 merged; local tip is part of it  ·  https://github.com/acme/app/…  │
╰──────────────────────────────────────────────────────────────────────────────╯
 space toggle · s safe · r review · n none · / filter · h kept · q quit   enter delete 131
```

## Install

```sh
go install github.com/bograh/git-purge@latest   # or: go build -o ~/.local/bin/git-purge .
```

Because the binary is named `git-purge`, it also works as `git purge`.

## Usage

```sh
git purge                  # fetch --prune, classify, open the picker
git purge sync             # just refresh the database and print a summary
git purge list [--all]     # show the last sync (--json for scripts)
git purge purge --yes      # non-interactive: delete only "safe" branches
git purge purge --dry-run --yes
git purge restore --last   # undo the last purge
git purge restore feat/x   # bring back one branch
git purge history          # everything ever purged in this repo
```

### Stale branches

Branches nobody touched in months are clutter even if they were never merged
or never pushed. `--older-than` shows only branches whose last commit is older
than the given age, and turns old `active`/`local` branches into **stale** ones
you can pick (never preselected):

```sh
git purge --older-than 6mo                         # picker, oldest clutter only
git purge list --older-than 90d
git purge purge --yes --older-than 1y --include-stale   # scripted cleanup
```

Ages: `90d`, `6mo`, `2w`, `1y`, or Go durations like `36h`.

### Every repository at once

```sh
git purge --all-repos ~/Code               # one picker across every repo
git purge list --all-repos ~/Code --json
git purge restore --last --all-repos ~/Code
```

Repositories are found up to `--depth` levels deep (default 3). Nested repos,
`node_modules`, `vendor` and hidden folders are skipped, and up to four repos
sync in parallel. One purge across many repos is one undo step.

### Picker

| Key | Action |
|-----|--------|
| `space` | toggle the branch under the cursor |
| `p` / `tab` | **preview**: commits not on the base branch, commits that were never in the merged PR, and the diffstat (`←`/`→` steps through branches, `space` toggles) |
| `s` / `r` / `t` | toggle all safe / review / stale |
| `n` | select none |
| `/` | filter by name |
| `o` | sort oldest first within each section |
| `h` | show kept branches |
| `enter` | delete the selection (asks to confirm) |
| `q` | quit |

## How a branch is judged

| Status      | Meaning | Default |
|-------------|---------|---------|
| `safe`      | Every commit is already on the base branch: merged, squash-merged, rebase-merged, or the local tip is part of a merged PR | selected |
| `review`    | Dead on remote, but no proof the work landed (PR closed unmerged, no PR found, or local commits beyond the merged PR) | not selected |
| `stale`     | Only with `--older-than`: an active or local branch with no commits for that long | not selected |
| `active`    | Still on the remote and not merged | never deleted |
| `local`     | Never pushed and not merged | never deleted |
| `protected` | Default branch, checked out (here or in a worktree), or matches a protect pattern | never deleted |

Evidence, cheapest first:

1. **`git fetch --all --prune`** marks branches whose upstream vanished as *gone*.
2. **Ancestry**: tip reachable from `origin/main` (or local `main`).
3. **Squash / rebase detection** (offline): checks whether the branch's patches,
   or its combined diff, already exist on the base branch (`git cherry` patch-ids).
4. **Forge lookup** (GitHub / GitLab API) for pushed branches still lacking proof.
   A merged PR only counts as safe if the local tip **is** the PR head or an
   ancestor of it. If you committed after opening the PR, it shows as `review`.
   Terminal PR states are cached, so re-syncs only hit the API for new or changed branches.
   With a GitHub token, lookups are batched through GraphQL, 50 branches per request,
   so a first sync of 500 branches takes 10 requests instead of 500.

## Safety guarantees

- **Compare-and-swap deletes.** Branches are deleted with
  `git update-ref -d <ref> <synced-sha>`. If a branch got new commits since the
  sync, the delete fails instead of losing work.
- **Undo log.** Every deleted branch's SHA and upstream config is recorded;
  `restore` recreates it exactly (as long as git hasn't garbage-collected the commits,
  which by default takes at least two weeks).
- **Remote branches are never touched.** Only local refs are removed.
- Non-interactive mode (`--yes`) deletes only `safe` branches unless you add `--include-review`.

## Configuration

| Setting | How |
|---------|-----|
| GitHub token | `GH_TOKEN` / `GITHUB_TOKEN`, or picked up from `gh auth token` |
| GitLab token | `GITLAB_TOKEN` |
| Self-hosted forge | auto-detected from the host name; otherwise `GIT_PURGE_FORGE=github\|gitlab` |
| Base branch | auto (`origin/HEAD`, then main/master/…); override with `git config gitpurge.base origin/develop` |
| Protect patterns | `--protect 'team/*'` or `git config --add gitpurge.protect 'team/*'` (built-in: main, master, trunk, develop, dev, staging, production, release/*) |
| Database | `--db`, `GIT_PURGE_DB`, default `~/.local/share/git-purge/state.db` |
| No network | `--offline` (git-only evidence), `--no-fetch` |

Fork workflow: if an `upstream` remote exists, PRs are looked up there with
`origin`'s owner as the head.

## Development

```sh
go test ./...
```

The engine test builds a real repository with a bare remote and covers merge,
squash-merge, rebase-merge, abandoned, partially-merged, active, local-only and
protected branches, plus the compare-and-swap delete and restore.
