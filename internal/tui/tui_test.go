package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	m := New([]store.Branch{
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

func names(bs []store.Branch) string {
	var s []string
	for _, b := range bs {
		s = append(s, b.Name)
	}
	return strings.Join(s, ",")
}

// The row cache must never serve stale output.
func TestRowCacheInvalidation(t *testing.T) {
	m := New([]store.Branch{{Name: "a", Status: "safe"}, {Name: "b", Status: "safe"}}, "test")
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
