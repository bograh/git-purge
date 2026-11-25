package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseRemote(t *testing.T) {
	cases := map[string]Remote{
		"git@github.com:acme/widgets.git":             {"github.com", "acme/widgets"},
		"https://github.com/acme/widgets":             {"github.com", "acme/widgets"},
		"ssh://git@gitlab.example.com:2222/a/b/c.git": {"gitlab.example.com", "a/b/c"},
		"https://user@GitLab.com/group/sub/proj.git":  {"gitlab.com", "group/sub/proj"},
	}
	for in, want := range cases {
		got, err := ParseRemote(in)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	if r := (Remote{Path: "a/b/c"}); r.Owner() != "a/b" || r.Name() != "c" {
		t.Errorf("owner/name wrong: %q %q", r.Owner(), r.Name())
	}
}

func TestGitHubFindPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls" || r.URL.Query().Get("head") != "me:feat/x" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing auth")
		}
		w.Write([]byte(`[
			{"number": 3, "state": "closed", "merged_at": null, "html_url": "u3", "head": {"sha": "c3", "ref": "feat/x"}},
			{"number": 2, "state": "closed", "merged_at": "2026-01-01T00:00:00Z", "html_url": "u2", "head": {"sha": "c2", "ref": "feat/x"}}
		]`))
	}))
	defer srv.Close()
	g := &GitHub{API: srv.URL, Base: Remote{"github.com", "acme/widgets"}, HeadOwner: "me", Token: "tok", Client: srv.Client()}
	pr, err := g.FindPR(context.Background(), "feat/x")
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || pr.Number != 2 || pr.State != StateMerged || pr.HeadSHA != "c2" {
		t.Fatalf("got %+v", pr)
	}
}

func TestGitLabFindPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/projects/group%2Fproj/merge_requests" {
			t.Errorf("unexpected path %s", r.URL.EscapedPath())
		}
		w.Write([]byte(`[{"iid": 5, "state": "closed", "sha": "c5", "web_url": "u5", "source_branch": "fix"}]`))
	}))
	defer srv.Close()
	g := &GitLab{API: srv.URL, Base: Remote{"gitlab.com", "group/proj"}, Client: srv.Client()}
	pr, err := g.FindPR(context.Background(), "fix")
	if err != nil || pr == nil || pr.State != StateClosed || pr.Number != 5 {
		t.Fatalf("got %+v, %v", pr, err)
	}
}

func TestRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	g := &GitHub{API: srv.URL, Base: Remote{"github.com", "a/b"}, HeadOwner: "a", Client: srv.Client()}
	if _, err := g.FindPR(context.Background(), "x"); err == nil {
		t.Fatal("expected rate limit error")
	} else if _, ok := err.(*RateLimitError); !ok {
		t.Fatalf("got %T %v", err, err)
	}
}
