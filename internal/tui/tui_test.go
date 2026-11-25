package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bograh/git-purge/internal/gitx"
	"github.com/bograh/git-purge/internal/store"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestPicker(t *testing.T) {
	m := newTest([]store.Branch{
		{Name: "keep", Status: "active"},
		{Name: "rev", Status: "review", Reason: "remote branch deleted; no merge found"},
		{Name: "ok", Status: "safe"},
	}, "test")
	// Safe preselected, review not, active hidden.
	if got := names(m.Selected()); got != "ok" {
		t.Fatalf("initial selection %q", got)
	}
	if v := m.View(); strings.Contains(v, "keep") || !strings.Contains(v, "rev") {
		t.Fatalf("kept branch should be hidden:\n%s", v)
	}
	m.Update(key("r")) // select all review
	if got := names(m.Selected()); got != "ok,rev" {
		t.Fatalf("after r: %q", got)
	}
	m.Update(key("h")) // show kept; active rows cannot be selected
	m.Update(key("G"))
	m.Update(key(" "))
	if got := names(m.Selected()); got != "ok,rev" {
		t.Fatalf("active branch became selectable: %q", got)
	}
	m.Update(key("enter"))
	if !strings.Contains(m.View(), "1 of them have no proof") {
		t.Fatalf("confirm screen missing review warning:\n%s", m.View())
	}
	m.Update(key("y"))
	if !m.Confirmed {
		t.Fatal("not confirmed")
	}
}

func names(bs []Item) string {
	var s []string
	for _, b := range bs {
		s = append(s, b.Name)
	}
	return strings.Join(s, ",")
}

// The row cache must never serve stale output.
func TestRowCacheInvalidation(t *testing.T) {
	m := newTest([]store.Branch{{Name: "a", Status: "safe"}, {Name: "b", Status: "safe"}}, "test")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	before := m.View()
	m.Update(key(" ")) // deselect a, cursor moves to b
	if m.View() == before {
		t.Fatal("view did not change after toggling selection")
	}
	m.Update(key("k"))
	m.Update(key(" ")) // reselect a, cursor back on b
	m.Update(key("k"))
	if got := m.View(); got != before {
		t.Fatalf("view differs after restoring state:\n%s\nvs\n%s", got, before)
	}
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	for _, l := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(l); w > 50 {
			t.Fatalf("line wider than terminal after resize (%d): %q", w, l)
		}
	}
}

func newTest(bs []store.Branch, header string) *Model {
	items := make([]Item, len(bs))
	for i, b := range bs {
		items[i] = Item{Branch: b}
	}
	return New(items, Options{Header: header})
}

func TestPreviewPane(t *testing.T) {
	calls := 0
	m := New([]Item{
		{Branch: store.Branch{Name: "rev", SHA: "s1", Status: "review", Reason: "no merge found"}},
		{Branch: store.Branch{Name: "ok", SHA: "s2", Status: "safe"}},
	}, Options{Header: "t", Preview: func(it Item) (*gitx.Preview, error) {
		calls++
		return &gitx.Preview{Base: "origin/main", MergeBase: "mb", UnmergedN: 1,
			Unmerged: []string{"abc123 secret work (2 days ago)"}, DiffStat: " a.go | 3 ++-"}, nil
	}})
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 20})

	// "ok" (safe) sorts first; move to "rev" and open its preview.
	m.Update(key("j"))
	_, cmd := m.Update(key("p"))
	if cmd == nil || !strings.Contains(m.View(), "Loading") {
		t.Fatal("preview should load asynchronously")
	}
	m.Update(cmd())
	v := m.View()
	for _, want := range []string{"COMMITS NOT ON ORIGIN/MAIN (1)", "secret work", "a.go"} {
		if !strings.Contains(v, want) {
			t.Fatalf("preview missing %q:\n%s", want, v)
		}
	}
	// Toggle selection from inside the preview.
	m.Update(key(" "))
	if got := names(m.Selected()); got != "ok,rev" {
		t.Fatalf("selection after toggle in preview: %q", got)
	}
	// Close and reopen: served from cache, no second load.
	m.Update(key("p"))
	if _, cmd := m.Update(key("p")); cmd != nil || calls != 1 {
		t.Fatalf("preview not cached (calls=%d)", calls)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if strings.Contains(m.View(), "secret work") {
		t.Fatal("esc should close the preview")
	}
}

func TestStaleAndOldestSort(t *testing.T) {
	now := time.Now()
	m := New([]Item{
		{Branch: store.Branch{Name: "newer", Status: "stale", CommitTime: now.Add(-100 * 24 * time.Hour)}, Repo: "api"},
		{Branch: store.Branch{Name: "older", Status: "stale", CommitTime: now.Add(-400 * 24 * time.Hour)}, Repo: "web"},
	}, Options{Header: "t"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	if len(m.Selected()) != 0 {
		t.Fatal("stale branches must not be preselected")
	}
	v := m.View()
	if !strings.Contains(v, "STALE") || !strings.Contains(v, "api › newer") {
		t.Fatalf("stale section / repo label missing:\n%s", v)
	}
	if strings.Index(v, "newer") > strings.Index(v, "older") {
		t.Fatal("default order should keep input order")
	}
	m.Update(key("o"))
	v = m.View()
	if strings.Index(v, "older") > strings.Index(v, "newer") {
		t.Fatal("o should sort oldest first")
	}
	m.Update(key("t"))
	if len(m.Selected()) != 2 {
		t.Fatal("t should select all stale")
	}
}

func TestFooterKeepsDeleteActionOnNarrowTerminals(t *testing.T) {
	for _, w := range []int{60, 80, 110, 160} {
		m := New([]Item{
			{Branch: store.Branch{Name: "a", Status: "safe"}},
			{Branch: store.Branch{Name: "b", Status: "stale"}},
		}, Options{Header: "t", Preview: func(Item) (*gitx.Preview, error) { return nil, nil }})
		m.Update(tea.WindowSizeMsg{Width: w, Height: 20})
		lines := strings.Split(m.View(), "\n")
		footer := lines[len(lines)-1]
		if !strings.Contains(footer, "delete 1") || lipgloss.Width(footer) > w {
			t.Errorf("width %d: footer %q (width %d)", w, footer, lipgloss.Width(footer))
		}
	}
}
