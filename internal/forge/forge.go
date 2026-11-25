// Package forge asks GitHub / GitLab what happened to a branch's pull request.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// PR states, normalised across forges.
const (
	StateOpen   = "open"
	StateMerged = "merged"
	StateClosed = "closed" // closed without merging
)

// PR is the pull/merge request most relevant to a branch.
type PR struct {
	Number  int
	State   string
	HeadSHA string // last commit the forge saw on the PR branch
	URL     string
}

// Terminal reports whether the PR can no longer change state.
func (p *PR) Terminal() bool { return p != nil && (p.State == StateMerged || p.State == StateClosed) }

// Forge looks up pull requests by head branch name.
type Forge interface {
	Name() string
	// FindPR returns the most relevant PR whose head is branch, or nil.
	FindPR(ctx context.Context, branch string) (*PR, error)
	// HeadRef is the ref the forge exposes a PR's head under, for fetching
	// commits whose branch was already deleted.
	HeadRef(number int) string
}

func (g *GitHub) HeadRef(n int) string { return fmt.Sprintf("refs/pull/%d/head", n) }
func (g *GitLab) HeadRef(n int) string { return fmt.Sprintf("refs/merge-requests/%d/head", n) }

// ErrUnauthorized means the API rejected or required credentials.
var ErrUnauthorized = errors.New("unauthorized")

// RateLimitError means the API rate limit was hit.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return "API rate limit exceeded"
	}
	return fmt.Sprintf("API rate limit exceeded (resets %s)", e.Reset.Local().Format("15:04"))
}

// Remote is a parsed git remote URL.
type Remote struct {
	Host string
	Path string // "owner/repo" or "group/subgroup/repo"
}

// Owner is everything before the final path segment.
func (r Remote) Owner() string {
	i := strings.LastIndex(r.Path, "/")
	if i < 0 {
		return ""
	}
	return r.Path[:i]
}

// Name is the final path segment.
func (r Remote) Name() string { return r.Path[strings.LastIndex(r.Path, "/")+1:] }

var scpLike = regexp.MustCompile(`^(?:[\w.\-]+@)?([\w.\-]+):(.+)$`)

// ParseRemote understands https://, ssh://, git:// and scp-style URLs.
func ParseRemote(raw string) (Remote, error) {
	raw = strings.TrimSpace(raw)
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return Remote{}, err
		}
		host, path = u.Hostname(), u.Path
	} else if m := scpLike.FindStringSubmatch(raw); m != nil {
		host, path = m[1], m[2]
	} else {
		return Remote{}, fmt.Errorf("unrecognised remote URL %q", raw)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || !strings.Contains(path, "/") {
		return Remote{}, fmt.Errorf("unrecognised remote URL %q", raw)
	}
	return Remote{Host: strings.ToLower(host), Path: path}, nil
}

// Kind guesses which forge software serves host. GIT_PURGE_FORGE overrides.
func Kind(host string) string {
	if k := os.Getenv("GIT_PURGE_FORGE"); k != "" {
		return strings.ToLower(k)
	}
	switch {
	case host == "github.com" || strings.Contains(host, "github"):
		return "github"
	case host == "gitlab.com" || strings.Contains(host, "gitlab"):
		return "gitlab"
	}
	return ""
}

// New builds a Forge for PRs targeting base, opened from branches pushed to head
// (base and head differ for fork workflows: base=upstream, head=origin).
func New(base, head Remote) (Forge, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	switch Kind(base.Host) {
	case "github":
		api := "https://api.github.com"
		if base.Host != "github.com" {
			api = "https://" + base.Host + "/api/v3"
		}
		return &GitHub{API: api, Base: base, HeadOwner: head.Owner(), Token: githubToken(base.Host), Client: client}, nil
	case "gitlab":
		return &GitLab{API: "https://" + base.Host + "/api/v4", Base: base, Token: gitlabToken(), Client: client}, nil
	}
	return nil, fmt.Errorf("unsupported forge host %q (set GIT_PURGE_FORGE=github|gitlab)", base.Host)
}

func githubToken(host string) string {
	for _, k := range []string{"GIT_PURGE_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if out, err := exec.Command("gh", "auth", "token", "--hostname", host).Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func gitlabToken() string {
	for _, k := range []string{"GIT_PURGE_TOKEN", "GITLAB_TOKEN", "GL_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func getJSON(ctx context.Context, c *http.Client, u string, hdr map[string]string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "git-purge")
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		var reset time.Time
		if s := resp.Header.Get("X-RateLimit-Reset"); s != "" {
			var n int64
			fmt.Sscan(s, &n)
			reset = time.Unix(n, 0)
		}
		return &RateLimitError{Reset: reset}
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s: not found (private repo without a token?)", u)
	case resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// pick prefers merged, then open, then closed PRs; within a state, the first
// (most recent, as both APIs sort newest first) wins.
func pick(prs []*PR) *PR {
	rank := map[string]int{StateMerged: 0, StateOpen: 1, StateClosed: 2}
	var best *PR
	for _, p := range prs {
		if best == nil || rank[p.State] < rank[best.State] {
			best = p
		}
	}
	return best
}

// GitHub implements Forge with the GitHub REST API.
type GitHub struct {
	API       string
	Base      Remote
	HeadOwner string
	Token     string
	Client    *http.Client
}

func (g *GitHub) Name() string { return "GitHub" }

func (g *GitHub) FindPR(ctx context.Context, branch string) (*PR, error) {
	q := url.Values{}
	q.Set("state", "all")
	q.Set("head", g.HeadOwner+":"+branch)
	q.Set("per_page", "20")
	u := fmt.Sprintf("%s/repos/%s/pulls?%s", g.API, g.Base.Path, q.Encode())
	hdr := map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
	if g.Token != "" {
		hdr["Authorization"] = "Bearer " + g.Token
	}
	var resp []struct {
		Number   int     `json:"number"`
		State    string  `json:"state"`
		MergedAt *string `json:"merged_at"`
		HTMLURL  string  `json:"html_url"`
		Head     struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := getJSON(ctx, g.Client, u, hdr, &resp); err != nil {
		return nil, err
	}
	var prs []*PR
	for _, r := range resp {
		if r.Head.Ref != branch {
			continue
		}
		st := StateOpen
		if r.State == "closed" {
			st = StateClosed
			if r.MergedAt != nil {
				st = StateMerged
			}
		}
		prs = append(prs, &PR{Number: r.Number, State: st, HeadSHA: r.Head.SHA, URL: r.HTMLURL})
	}
	return pick(prs), nil
}

// GitLab implements Forge with the GitLab REST API.
type GitLab struct {
	API    string
	Base   Remote
	Token  string
	Client *http.Client
}

func (g *GitLab) Name() string { return "GitLab" }

func (g *GitLab) FindPR(ctx context.Context, branch string) (*PR, error) {
	q := url.Values{}
	q.Set("source_branch", branch)
	q.Set("state", "all")
	q.Set("per_page", "20")
	u := fmt.Sprintf("%s/projects/%s/merge_requests?%s", g.API, url.PathEscape(g.Base.Path), q.Encode())
	hdr := map[string]string{}
	if g.Token != "" {
		hdr["PRIVATE-TOKEN"] = g.Token
	}
	var resp []struct {
		IID          int    `json:"iid"`
		State        string `json:"state"`
		SHA          string `json:"sha"`
		WebURL       string `json:"web_url"`
		SourceBranch string `json:"source_branch"`
	}
	if err := getJSON(ctx, g.Client, u, hdr, &resp); err != nil {
		return nil, err
	}
	var prs []*PR
	for _, r := range resp {
		if r.SourceBranch != branch {
			continue
		}
		st := StateOpen
		switch r.State {
		case "merged":
			st = StateMerged
		case "closed", "locked":
			st = StateClosed
		}
		prs = append(prs, &PR{Number: r.IID, State: st, HeadSHA: r.SHA, URL: r.WebURL})
	}
	return pick(prs), nil
}
