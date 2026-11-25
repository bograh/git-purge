package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BatchFinder is implemented by forges that can look up many branches in one
// request. Engines should prefer it when CanBatch reports true.
type BatchFinder interface {
	CanBatch() bool
	// FindPRs returns the most relevant PR per branch name (absent = no PR).
	FindPRs(ctx context.Context, branches []string) (map[string]*PR, error)
}

// GraphQLBatchSize is the number of branches per GraphQL query. Each alias is a
// separate connection, so the query cost stays well under GitHub's limits.
const GraphQLBatchSize = 50

// CanBatch: GitHub's GraphQL API requires authentication.
func (g *GitHub) CanBatch() bool { return g.Token != "" }

func (g *GitHub) graphqlURL() string {
	if strings.HasSuffix(g.API, "/api/v3") { // GitHub Enterprise Server
		return strings.TrimSuffix(g.API, "/v3") + "/graphql"
	}
	return g.API + "/graphql"
}

// FindPRs looks up PRs for up to GraphQLBatchSize branches per request.
func (g *GitHub) FindPRs(ctx context.Context, branches []string) (map[string]*PR, error) {
	out := map[string]*PR{}
	for start := 0; start < len(branches); start += GraphQLBatchSize {
		end := min(start+GraphQLBatchSize, len(branches))
		if err := g.findBatch(ctx, branches[start:end], out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (g *GitHub) findBatch(ctx context.Context, branches []string, out map[string]*PR) error {
	owner, name := g.Base.Owner(), g.Base.Name()
	vars := map[string]any{"owner": owner, "name": name}
	var decl, body strings.Builder
	decl.WriteString("$owner: String!, $name: String!")
	for i, b := range branches {
		fmt.Fprintf(&decl, ", $h%d: String!", i)
		fmt.Fprintf(&body, "  b%d: pullRequests(headRefName: $h%d, first: 20, orderBy: {field: CREATED_AT, direction: DESC}) {\n"+
			"    nodes { number state url headRefOid headRefName headRepositoryOwner { login } }\n  }\n", i, i)
		vars[fmt.Sprintf("h%d", i)] = b
	}
	query := fmt.Sprintf("query(%s) {\n repository(owner: $owner, name: $name) {\n%s }\n}", decl.String(), body.String())

	payload, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.graphqlURL(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "git-purge")
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		var reset time.Time
		if s := resp.Header.Get("X-RateLimit-Reset"); s != "" {
			var n int64
			fmt.Sscan(s, &n)
			reset = time.Unix(n, 0)
		}
		return &RateLimitError{Reset: reset}
	case resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub GraphQL: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	type node struct {
		Number              int    `json:"number"`
		State               string `json:"state"`
		URL                 string `json:"url"`
		HeadRefOid          string `json:"headRefOid"`
		HeadRefName         string `json:"headRefName"`
		HeadRepositoryOwner *struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	var res struct {
		Data struct {
			Repository map[string]struct {
				Nodes []node `json:"nodes"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("GitHub GraphQL: %w", err)
	}
	if len(res.Errors) > 0 {
		if res.Errors[0].Type == "RATE_LIMITED" {
			return &RateLimitError{}
		}
		return fmt.Errorf("GitHub GraphQL: %s", res.Errors[0].Message)
	}
	if res.Data.Repository == nil {
		return fmt.Errorf("GitHub GraphQL: repository %s not found", g.Base.Path)
	}

	for i, b := range branches {
		var prs []*PR
		for _, n := range res.Data.Repository[fmt.Sprintf("b%d", i)].Nodes {
			// Match REST's head=owner:branch: ignore same-named branches on other forks.
			if n.HeadRefName != b || n.HeadRepositoryOwner == nil || !strings.EqualFold(n.HeadRepositoryOwner.Login, g.HeadOwner) {
				continue
			}
			st := StateOpen
			switch n.State {
			case "MERGED":
				st = StateMerged
			case "CLOSED":
				st = StateClosed
			}
			prs = append(prs, &PR{Number: n.Number, State: st, HeadSHA: n.HeadRefOid, URL: n.URL})
		}
		if p := pick(prs); p != nil {
			out[b] = p
		}
	}
	return nil
}
