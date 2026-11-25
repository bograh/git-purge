package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
