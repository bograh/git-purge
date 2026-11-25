// Package tui is the interactive branch picker.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/store"
)

var (
	styleSafe      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleReview    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleKeep      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCursor    = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleTitle     = lipgloss.NewStyle().Bold(true)
	styleDim       = lipgloss.NewStyle().Faint(true)
	styleDanger    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleGoneBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

var rank = map[string]int{
	string(classify.Safe): 0, string(classify.Review): 1, string(classify.Active): 2,
	string(classify.Local): 3, string(classify.Protected): 4,
}

type row struct {
	b        store.Branch
	selected bool
}

// Model is the bubbletea model.
type Model struct {
	rows      []*row
	cursor    int
	offset    int
	width     int
	height    int
	showKept  bool
	confirm   bool
	Confirmed bool
	header    string
}

// New builds the picker. Safe branches start selected; review branches don't.
func New(branches []store.Branch, header string) *Model {
	m := &Model{header: header, height: 24, width: 100}
	for _, b := range branches {
		m.rows = append(m.rows, &row{b: b, selected: b.Status == string(classify.Safe)})
	}
	sort.SliceStable(m.rows, func(i, j int) bool { return rank[m.rows[i].b.Status] < rank[m.rows[j].b.Status] })
	return m
}

// Selected returns the branches the user chose.
func (m *Model) Selected() []store.Branch {
	var out []store.Branch
	for _, r := range m.rows {
		if r.selected && classify.Status(r.b.Status).Deletable() {
			out = append(out, r.b)
		}
	}
	return out
}

func (m *Model) visible() []*row {
	if m.showKept {
		return m.rows
	}
	var out []*row
	for _, r := range m.rows {
		if classify.Status(r.b.Status).Deletable() {
			out = append(out, r)
		}
	}
	return out
}

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		if m.confirm {
			switch msg.String() {
			case "y", "Y":
				m.Confirmed = true
				return m, tea.Quit
			default:
				m.confirm = false
			}
			return m, nil
		}
		vis := m.visible()
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			m.cursor--
		case "down", "j":
			m.cursor++
		case "pgup", "ctrl+u":
			m.cursor -= m.listHeight()
		case "pgdown", "ctrl+d":
			m.cursor += m.listHeight()
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = len(vis) - 1
		case " ", "x":
			if m.cursor < len(vis) && classify.Status(vis[m.cursor].b.Status).Deletable() {
				vis[m.cursor].selected = !vis[m.cursor].selected
				m.cursor++
			}
		case "s": // toggle all safe
			m.toggleAll(classify.Safe)
		case "r": // toggle all review
			m.toggleAll(classify.Review)
		case "n":
			for _, r := range m.rows {
				r.selected = false
			}
		case "h":
			var cur *row
			if m.cursor < len(vis) {
				cur = vis[m.cursor]
			}
			m.showKept = !m.showKept
			m.cursor = 0
			for i, r := range m.visible() {
				if r == cur {
					m.cursor = i
				}
			}
		case "enter", "d":
			if len(m.Selected()) > 0 {
				m.confirm = true
			}
		}
		m.clamp()
	}
	return m, nil
}

func (m *Model) toggleAll(s classify.Status) {
	all := true
	for _, r := range m.rows {
		if r.b.Status == string(s) && !r.selected {
			all = false
		}
	}
	for _, r := range m.rows {
		if r.b.Status == string(s) {
			r.selected = !all
		}
	}
}

func (m *Model) listHeight() int {
	h := m.height - 9 // header(2) + details(4) + help(2) + slack
	if h < 3 {
		h = 3
	}
	return h
}

func (m *Model) clamp() {
	n := len(m.visible())
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	lh := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+lh {
		m.offset = m.cursor - lh + 1
	}
}

func (m *Model) View() string {
	var sb strings.Builder
	counts := map[string]int{}
	selected := 0
	for _, r := range m.rows {
		counts[r.b.Status]++
		if r.selected {
			selected++
		}
	}
	sb.WriteString(styleTitle.Render("git-purge") + styleDim.Render(" · "+m.header) + "\n")
	sb.WriteString(fmt.Sprintf("%s  %s  %s  %s\n\n",
		styleSafe.Render(fmt.Sprintf("%d safe", counts["safe"])),
		styleReview.Render(fmt.Sprintf("%d need review", counts["review"])),
		styleKeep.Render(fmt.Sprintf("%d kept", len(m.rows)-counts["safe"]-counts["review"])),
		styleTitle.Render(fmt.Sprintf("%d selected", selected))))

	if m.confirm {
		sel := m.Selected()
		review := 0
		for _, b := range sel {
			if b.Status == string(classify.Review) {
				review++
			}
		}
		sb.WriteString(styleDanger.Render(fmt.Sprintf("Delete %d local branch(es)?", len(sel))) + "\n")
		if review > 0 {
			sb.WriteString(styleReview.Render(fmt.Sprintf("  %d of them have no proof of being merged.", review)) + "\n")
		}
		sb.WriteString(styleDim.Render("  Remote branches are not touched. Undo any time with: git purge restore --last") + "\n\n")
		sb.WriteString("  [y] delete    [any other key] back\n")
		return sb.String()
	}

	vis := m.visible()
	if len(vis) == 0 {
		sb.WriteString(styleSafe.Render("  Nothing to purge — every local branch is still alive.") + "\n")
		if !m.showKept {
			sb.WriteString(styleDim.Render("  press h to show kept branches") + "\n")
		}
	}
	nameW := 0
	for _, r := range vis {
		if len(r.b.Name) > nameW {
			nameW = len(r.b.Name)
		}
	}
	if max := m.width / 2; nameW > max && max > 10 {
		nameW = max
	}
	end := m.offset + m.listHeight()
	if end > len(vis) {
		end = len(vis)
	}
	for i := m.offset; i < end; i++ {
		r := vis[i]
		box := "[ ]"
		if r.selected {
			box = "[x]"
		}
		if !classify.Status(r.b.Status).Deletable() {
			box = "   "
		}
		name := truncate(r.b.Name, nameW)
		gone := "      "
		if r.b.Gone {
			gone = "✗ gone"
		}
		line := fmt.Sprintf("%s %-*s %-9s %s %5s  %s", box, nameW, name, r.b.Status, gone, age(r.b.CommitTime), r.b.Reason)
		line = truncate(line, m.width-1)
		switch {
		case i == m.cursor:
			line = styleCursor.Render(line)
		case r.b.Status == string(classify.Safe):
			line = styleSafe.Render(line)
		case r.b.Status == string(classify.Review):
			line = styleReview.Render(line)
		default:
			line = styleKeep.Render(line)
		}
		sb.WriteString(line + "\n")
	}
	for i := end - m.offset; i < m.listHeight(); i++ {
		sb.WriteString("\n")
	}

	if m.cursor < len(vis) {
		b := vis[m.cursor].b
		sb.WriteString("\n" + styleTitle.Render(b.Name) + styleDim.Render("  "+short(b.SHA)+"  "+b.CommitTime.Format("2006-01-02")) + "\n")
		sb.WriteString("  " + truncate(b.Subject, m.width-3) + "\n")
		detail := b.Upstream
		if b.Gone {
			detail += " " + styleGoneBadge.Render("(deleted on remote)")
		}
		if b.PRURL != "" {
			detail += "  " + b.PRURL
		}
		sb.WriteString("  " + detail + "\n")
	}
	sb.WriteString(styleDim.Render("space toggle · s all safe · r all review · n none · h show kept · enter delete · q quit") + "\n")
	return sb.String()
}

// Run shows the picker and returns the chosen branches (nil if cancelled).
func Run(branches []store.Branch, header string) ([]store.Branch, error) {
	m := New(branches, header)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		return nil, err
	}
	if !m.Confirmed {
		return nil, nil
	}
	return m.Selected(), nil
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return "now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}

// Age is exported for the plain-text list output.
func Age(t time.Time) string { return age(t) }
