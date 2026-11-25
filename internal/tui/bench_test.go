package tui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/bograh/git-purge/internal/store"
)

func benchModel(n int) *Model {
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	st := []string{"safe", "review", "active", "local"}
	var bs []store.Branch
	for i := 0; i < n; i++ {
		bs = append(bs, store.Branch{Name: fmt.Sprintf("feature/some-branch-%04d", i), SHA: "0123456789abcdef", Status: st[i%4],
			Gone: i%3 == 0, Upstream: "origin/x", Reason: "PR #482 merged; local tip is part of it; remote branch deleted",
			Subject: "Some commit subject", CommitTime: time.Now().Add(-time.Duration(i) * time.Hour)})
	}
	m := newTest(bs, "origin/main · GitHub")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

func BenchmarkView(b *testing.B) {
	for _, n := range []int{50, 500, 5000} {
		m := benchModel(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

func BenchmarkKeyDown(b *testing.B) {
	for _, n := range []int{50, 500, 5000} {
		m := benchModel(n)
		down := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}
		up := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				m.Update(down)
				m.Update(up)
			}
		})
	}
}
