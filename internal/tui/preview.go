package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/gitx"
)

type previewState struct {
	r      *row
	key    string
	res    *previewMsg // nil while loading
	scroll int
}

type previewMsg struct {
	key string
	p   *gitx.Preview
	err error
}

func previewKey(it Item) string { return fmt.Sprintf("%d\x00%s\x00%s", it.RepoIndex, it.Name, it.SHA) }

// openPreview shows r's preview, loading it in the background when not cached.
func (m *Model) openPreview(r *row) tea.Cmd {
	key := previewKey(r.b)
	m.pv = &previewState{r: r, key: key}
	if res, ok := m.pvCache[key]; ok {
		m.pv.res = &res
		return nil
	}
	load, item := m.opt.Preview, r.b
	return func() tea.Msg {
		p, err := load(item)
		return previewMsg{key: key, p: p, err: err}
	}
}

func (m *Model) receivePreview(msg previewMsg) {
	if m.pvCache == nil {
		m.pvCache = map[string]previewMsg{}
	}
	m.pvCache[msg.key] = msg
	if m.pv != nil && m.pv.key == msg.key {
		m.pv.res = &msg
	}
}

func (m *Model) previewHeight() int { return max(3, m.height-4) }

func (m *Model) updatePreview(msg tea.KeyMsg) tea.Cmd {
	pv := m.pv
	page := m.previewHeight()
	switch msg.String() {
	case "ctrl+c":
		return tea.Quit
	case "esc", "q", "p", "tab":
		m.pv = nil
		return nil
	case "down", "j":
		pv.scroll++
	case "up", "k":
		pv.scroll--
	case "pgdown", "ctrl+d", "f":
		pv.scroll += page
	case "pgup", "ctrl+u", "b":
		pv.scroll -= page
	case "home", "g":
		pv.scroll = 0
	case "end", "G":
		pv.scroll = 1 << 30
	case " ", "x":
		if classify.Status(pv.r.b.Status).Deletable() {
			pv.r.selected = !pv.r.selected
		}
	case "right", "l", "]":
		return m.stepPreview(+1)
	case "left", "h", "[":
		return m.stepPreview(-1)
	}
	pv.scroll = max(0, min(pv.scroll, len(m.previewLines())-page))
	return nil
}

// stepPreview moves the list cursor and the preview to the next/previous branch.
func (m *Model) stepPreview(d int) tea.Cmd {
	next := m.cursor + d
	if next < 0 || next >= len(m.vis) {
		return nil
	}
	m.cursor = next
	m.clamp()
	return m.openPreview(m.vis[next])
}

// previewLines renders the preview body (without header/footer).
func (m *Model) previewLines() []string {
	pv := m.pv
	w := max(20, m.width-4)
	if pv.res == nil {
		return []string{sMuted.Render("Loading…")}
	}
	if pv.res.err != nil {
		return []string{sDanger.Render("Could not load preview: ") + sText.Render(pv.res.err.Error())}
	}
	p := pv.res.p
	var out []string
	heading := func(title string, c lipgloss.TerminalColor, note string) {
		if len(out) > 0 {
			out = append(out, "")
		}
		h := sSection.Foreground(c).Render(strings.ToUpper(title))
		if note != "" {
			h += "  " + sMuted.Render(note)
		}
		out = append(out, h)
	}
	commits := func(list []string, total int) {
		for _, c := range list {
			sha, rest, _ := strings.Cut(c, " ")
			out = append(out, "  "+sKey.Render(sha)+" "+sText.Render(ansi.Truncate(rest, w-len(sha)-3, "…")))
		}
		if total > len(list) {
			out = append(out, sFaint.Render(fmt.Sprintf("  … and %d more", total-len(list))))
		}
	}

	if len(p.NotInPR) > 0 || p.NotInPRN > 0 {
		heading(fmt.Sprintf("Commits not in PR #%d (%d)", pv.r.b.PRNumber, p.NotInPRN), cDanger,
			"these would be lost: they were never part of the merged PR")
		commits(p.NotInPR, p.NotInPRN)
	}

	switch {
	case p.UnmergedN == 0:
		heading("Commits not on "+p.Base, cSafe, "none — every commit is already on "+p.Base)
	case p.PatchMerged:
		heading(fmt.Sprintf("Commits not on %s (%d)", p.Base, p.UnmergedN), cSafe,
			"each has an identical patch on "+p.Base+" (rebase-merged)")
		commits(p.Unmerged, p.UnmergedN)
	default:
		note := ""
		if pv.r.b.Squashed {
			note = "the combined change was squash-merged into " + p.Base
		}
		c := cReview
		if pv.r.b.Status == string(classify.Safe) {
			c = cSafe
		}
		heading(fmt.Sprintf("Commits not on %s (%d)", p.Base, p.UnmergedN), c, note)
		commits(p.Unmerged, p.UnmergedN)
	}

	if strings.TrimSpace(p.DiffStat) != "" {
		heading("Changes since merge-base "+short(p.MergeBase), cMuted, "")
		for _, l := range strings.Split(p.DiffStat, "\n") {
			out = append(out, "  "+colorStat(ansi.Truncate(strings.TrimSpace(l), w-2, "…")))
		}
	}
	return out
}

// colorStat colors the +/- histogram of a `git diff --stat` line.
func colorStat(l string) string {
	i := strings.LastIndex(l, "| ")
	if i < 0 {
		return sMuted.Render(l)
	}
	file, graph := l[:i+2], l[i+2:]
	num, bars, _ := strings.Cut(graph, " ")
	return sText.Render(file) + sMuted.Render(num+" ") +
		sAdd.Render(strings.Repeat("+", strings.Count(bars, "+"))) +
		sDel.Render(strings.Repeat("-", strings.Count(bars, "-")))
}

func (m *Model) viewPreview() string {
	pv := m.pv
	b := pv.r.b
	var sb strings.Builder

	box := "○"
	if pv.r.selected {
		box = "✓"
	}
	if !classify.Status(b.Status).Deletable() {
		box = "·"
	}
	left := sBrand.Render("git-purge") + "  " + sMuted.Render("preview") + "  " +
		lipgloss.NewStyle().Foreground(statusColor(b.Status)).Bold(true).Render(box) + " " +
		sBold.Render(b.Label()) + "  " + lipgloss.NewStyle().Foreground(statusColor(b.Status)).Render("● "+b.Status)
	pos := sFaint.Render(fmt.Sprintf("%d/%d", m.cursor+1, len(m.vis)))
	sb.WriteString(spread(left, pos, m.width) + "\n")
	sb.WriteString(" " + sMuted.Render(ansi.Truncate("→ "+b.Reason, m.width-2, "…")) + "\n\n")

	lines := m.previewLines()
	h := m.previewHeight() - 1
	end := min(len(lines), pv.scroll+h)
	n := 0
	for _, l := range lines[min(pv.scroll, end):end] {
		sb.WriteString(" " + l + "\n")
		n++
	}
	for ; n < h; n++ {
		sb.WriteString("\n")
	}
	keys := []keyHelp{{"j/k", "scroll"}, {"←/→", "prev/next branch"}}
	if classify.Status(b.Status).Deletable() {
		keys = append(keys, keyHelp{"space", "toggle"})
	}
	keys = append(keys, keyHelp{"esc", "back"})
	more := ""
	if len(lines) > h {
		more = sFaint.Render(fmt.Sprintf("%d%% ", 100*end/len(lines)))
	}
	sb.WriteString(" " + spread(renderKeys(keys), more, m.width-1))
	return sb.String()
}
