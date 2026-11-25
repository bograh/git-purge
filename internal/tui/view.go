package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/bograh/git-purge/internal/classify"
)

// Palette: every color adapts to light and dark terminal backgrounds.
var (
	cAccent = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#A78BFA"}
	cSafe   = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}
	cReview = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	cStale  = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#22D3EE"}
	cDanger = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	cText   = lipgloss.AdaptiveColor{Light: "#111827", Dark: "#E5E7EB"}
	cMuted  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	cFaint  = lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#52525B"}
	cBorder = lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#3F3F46"}
	cCursor = lipgloss.AdaptiveColor{Light: "#EDE9FE", Dark: "#2A2540"}
	cOnPill = lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#18181B"}
)

var (
	sBrand    = lipgloss.NewStyle().Bold(true).Foreground(cOnPill).Background(cAccent).Padding(0, 1)
	sMuted    = lipgloss.NewStyle().Foreground(cMuted)
	sFaint    = lipgloss.NewStyle().Foreground(cFaint)
	sText     = lipgloss.NewStyle().Foreground(cText)
	sBold     = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sKey      = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sDanger   = lipgloss.NewStyle().Foreground(cDanger).Bold(true)
	sSection  = lipgloss.NewStyle().Bold(true)
	sGone     = lipgloss.NewStyle().Foreground(cDanger)
	sAdd      = lipgloss.NewStyle().Foreground(cSafe)
	sDel      = lipgloss.NewStyle().Foreground(cDanger)
	sDetail   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	sDialog   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDanger).Padding(1, 3)
	sBtnDel   = lipgloss.NewStyle().Bold(true).Foreground(cOnPill).Background(cDanger).Padding(0, 2)
	sBtnBack  = lipgloss.NewStyle().Foreground(cText).Background(cBorder).Padding(0, 2)
	sFilterOn = lipgloss.NewStyle().Foreground(cAccent)
)

func statusColor(s string) lipgloss.TerminalColor {
	switch classify.Status(s) {
	case classify.Safe:
		return cSafe
	case classify.Review:
		return cReview
	case classify.Stale:
		return cStale
	}
	return cMuted
}

func sectionColor(heading string) lipgloss.TerminalColor {
	switch heading {
	case "Safe to delete":
		return cSafe
	case "Needs review":
		return cReview
	case "Stale":
		return cStale
	}
	return cMuted
}

type tally struct{ safe, review, stale, kept, selected int }

func (m *Model) counts() (t tally) {
	for _, r := range m.rows {
		switch classify.Status(r.b.Status) {
		case classify.Safe:
			t.safe++
		case classify.Review:
			t.review++
		case classify.Stale:
			t.stale++
		default:
			t.kept++
		}
		if r.selected {
			t.selected++
		}
	}
	return t
}

func (m *Model) View() string {
	if m.confirm {
		return m.viewConfirm()
	}
	if m.pv != nil {
		return m.viewPreview()
	}
	w := m.width
	var b strings.Builder
	t := m.counts()

	// Header: brand + context on the left, count badges on the right.
	left := sBrand.Render("git-purge") + "  " + sMuted.Render(m.opt.Header)
	badges := []string{badge(cSafe, "●", t.safe, "safe"), badge(cReview, "●", t.review, "review")}
	if t.stale > 0 {
		badges = append(badges, badge(cStale, "●", t.stale, "stale"))
	}
	badges = append(badges, badge(cMuted, "○", t.kept, "kept"))
	b.WriteString(spread(left, strings.Join(badges, "  "), w) + "\n")

	// Filter / sort / scroll position line.
	var sub []string
	switch {
	case m.filtering:
		sub = append(sub, sFilterOn.Render("/ ")+sText.Render(m.filter)+sFilterOn.Render("▏"))
	case m.filter != "":
		sub = append(sub, sMuted.Render("filter: ")+sText.Render(m.filter)+sFaint.Render(" (esc clears)"))
	}
	if m.oldest {
		sub = append(sub, sMuted.Render("sorted oldest first"))
	}
	pos := ""
	if lh := m.listHeight(); len(m.lns) > lh {
		end := min(m.offset+lh, len(m.lns))
		pos = sFaint.Render(fmt.Sprintf("%d%%", 100*end/len(m.lns)))
	}
	b.WriteString(spread(strings.Join(sub, sFaint.Render("  ·  ")), pos, w) + "\n\n")

	// List.
	lh := m.listHeight()
	rendered := 0
	if len(m.lns) == 0 {
		msg := "Nothing to purge — every local branch is still alive."
		if m.filter != "" {
			msg = "No branches match the filter."
		}
		b.WriteString("  " + lipgloss.NewStyle().Foreground(cSafe).Render("✓ "+msg) + "\n")
		rendered++
		if !m.showKept {
			b.WriteString("  " + sMuted.Render("press ") + sKey.Render("h") + sMuted.Render(" to show kept branches") + "\n")
			rendered++
		}
	}
	for i := m.offset; i < len(m.lns) && rendered < lh; i++ {
		b.WriteString(m.renderLine(m.lns[i]) + "\n")
		rendered++
	}
	for ; rendered < lh; rendered++ {
		b.WriteString("\n")
	}

	b.WriteString(m.viewDetail() + "\n")
	b.WriteString(m.viewFooter(t))
	return b.String()
}

func (m *Model) renderLine(l line) string {
	if l.r == nil {
		if strings.TrimSpace(l.heading) == "" {
			return ""
		}
		title := sSection.Foreground(sectionColor(l.heading)).Render(strings.ToUpper(l.heading))
		rule := sFaint.Render(strings.Repeat("─", max(0, m.width-lipgloss.Width(title)-4)))
		return "  " + title + " " + rule
	}
	r := l.r
	isCursor := l.idx == m.cursor
	key := rowKey{selected: r.selected, cursor: isCursor, width: m.width, nameW: m.nameW}
	if r.cache != "" && r.cacheKey == key {
		return r.cache
	}
	r.cache, r.cacheKey = m.renderRow(r, isCursor), key
	return r.cache
}

func (m *Model) renderRow(r *row, isCursor bool) string {
	w, nameW := m.width, m.nameW
	bg := lipgloss.NewStyle()
	if isCursor {
		bg = bg.Background(cCursor)
	}
	st := func(s lipgloss.Style) lipgloss.Style { return s.Inherit(bg) }

	bar := st(lipgloss.NewStyle()).Render("  ")
	if isCursor {
		bar = st(lipgloss.NewStyle().Foreground(cAccent)).Render("▌ ")
	}
	var box string
	switch {
	case !classify.Status(r.b.Status).Deletable():
		box = st(sFaint).Render("·")
	case r.selected:
		box = st(lipgloss.NewStyle().Foreground(statusColor(r.b.Status)).Bold(true)).Render("✓")
	default:
		box = st(sFaint).Render("○")
	}

	nameStyle := sText
	if isCursor {
		nameStyle = sBold
	}
	var name string
	if r.b.Repo != "" {
		label := ansi.Truncate(r.b.Label(), nameW, "…")
		repoPart := ansi.Truncate(r.b.Repo+" › ", lipgloss.Width(label), "")
		rest := strings.TrimPrefix(label, repoPart)
		name = st(sMuted).Render(repoPart) + st(nameStyle).Width(nameW-lipgloss.Width(repoPart)).Render(rest)
	} else {
		name = st(nameStyle).Width(nameW).Render(ansi.Truncate(r.b.Name, nameW, "…"))
	}

	gone := st(lipgloss.NewStyle()).Width(7).Render("")
	if r.b.Gone {
		gone = st(sGone).Width(7).Render("✗ gone")
	}
	tag := ""
	if !classify.Status(r.b.Status).Deletable() {
		tag = st(sMuted).Render(r.b.Status) + st(lipgloss.NewStyle()).Render("  ")
	}
	ageCol := st(sMuted).Width(5).Align(lipgloss.Right).Render(age(r.b.CommitTime))

	sp := st(lipgloss.NewStyle()).Render(" ")
	prefix := bar + box + sp + sp + name + sp + sp + gone + ageCol + sp + sp + tag
	room := w - lipgloss.Width(prefix) - 1
	out := prefix + st(sMuted).Render(ansi.Truncate(r.b.Reason, max(0, room), "…"))
	if pad := w - 1 - lipgloss.Width(out); isCursor && pad > 0 {
		out += st(lipgloss.NewStyle()).Render(strings.Repeat(" ", pad))
	}
	return out
}

func (m *Model) viewDetail() string {
	inner := max(10, m.width-4)
	var content []string
	if r := m.current(); r != nil {
		b := r.b
		color := statusColor(b.Status)
		title := sBold.Render(b.Name) + "  " + lipgloss.NewStyle().Foreground(color).Render("● "+b.Status)
		if b.Repo != "" {
			title = sMuted.Render(b.Repo+" › ") + title
		}
		meta := sMuted.Render(short(b.SHA) + " · " + b.CommitTime.Format("Jan 2 2006") + " · ")
		switch {
		case b.Upstream == "":
			meta += sFaint.Render("no upstream")
		case b.Gone:
			meta += sMuted.Render(b.Upstream+" ") + sGone.Render("deleted on remote")
		default:
			meta += sMuted.Render(b.Upstream + " (live)")
		}
		subject := sText.Render("“" + b.Subject + "”")
		why := lipgloss.NewStyle().Foreground(color).Render("→ ") + sMuted.Render(b.Reason)
		if b.PRURL != "" {
			why += sFaint.Render("  ·  ") + sFilterOn.Render(b.PRURL)
		}
		content = []string{title, meta, subject, why}
	} else {
		content = []string{sFaint.Render("no branch selected"), "", "", ""}
	}
	for i, c := range content {
		content[i] = ansi.Truncate(c, inner, "…")
	}
	return sDetail.Width(m.width - 2).Render(strings.Join(content, "\n"))
}

type keyHelp struct{ k, d string }

func renderKeys(keys []keyHelp) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = sKey.Render(k.k) + " " + sMuted.Render(k.d)
	}
	return strings.Join(parts, sFaint.Render(" · "))
}

func (m *Model) viewFooter(t tally) string {
	action := sFaint.Render("nothing selected")
	if t.selected > 0 {
		action = sKey.Render("enter") + " " + sDanger.Render(fmt.Sprintf("delete %d", t.selected))
	}
	if m.filtering {
		return " " + spread(renderKeys([]keyHelp{{"enter", "apply"}, {"esc", "clear"}}), action+" ", m.width-1)
	}
	// Key hints in display order, each with a priority: on narrow terminals the
	// least important are dropped so the delete action always stays visible.
	type hint struct {
		keyHelp
		prio int
	}
	hints := []hint{{keyHelp{"space", "toggle"}, 0}}
	if m.opt.Preview != nil {
		hints = append(hints, hint{keyHelp{"p", "preview"}, 1})
	}
	hints = append(hints, hint{keyHelp{"s", "safe"}, 3}, hint{keyHelp{"r", "review"}, 3})
	if t.stale > 0 {
		hints = append(hints, hint{keyHelp{"t", "stale"}, 3})
	}
	hints = append(hints, hint{keyHelp{"n", "none"}, 5}, hint{keyHelp{"/", "filter"}, 2},
		hint{keyHelp{"o", "oldest"}, 6}, hint{keyHelp{"h", "kept"}, 4}, hint{keyHelp{"q", "quit"}, 0})

	room := m.width - 2 - lipgloss.Width(action) - 2
	for drop := 6; drop > 0; drop-- {
		var keys []keyHelp
		for _, h := range hints {
			keys = append(keys, h.keyHelp)
		}
		if lipgloss.Width(renderKeys(keys)) <= room {
			break
		}
		kept := hints[:0:0]
		for _, h := range hints {
			if h.prio < drop {
				kept = append(kept, h)
			}
		}
		hints = kept
	}
	keys := make([]keyHelp, len(hints))
	for i, h := range hints {
		keys[i] = h.keyHelp
	}
	return " " + spread(renderKeys(keys), action+" ", m.width-1)
}

func (m *Model) viewConfirm() string {
	sel := m.Selected()
	unproven := 0
	for _, b := range sel {
		if b.Status != string(classify.Safe) {
			unproven++
		}
	}
	var body []string
	body = append(body, sDanger.Render(fmt.Sprintf("Delete %d local branch%s?", len(sel), plural(len(sel)))), "")
	const show = 8
	for i, b := range sel {
		if i == show {
			body = append(body, sFaint.Render(fmt.Sprintf("  … and %d more", len(sel)-show)))
			break
		}
		dot := lipgloss.NewStyle().Foreground(statusColor(b.Status)).Render("●")
		body = append(body, "  "+dot+" "+sText.Render(ansi.Truncate(b.Label(), 56, "…")))
	}
	body = append(body, "")
	if unproven > 0 {
		body = append(body, lipgloss.NewStyle().Foreground(cReview).Bold(true).
			Render(fmt.Sprintf("⚠ %d of them have no proof of being merged.", unproven)))
	}
	body = append(body,
		sMuted.Render("Remote branches are not touched."),
		sMuted.Render("Undo any time: ")+sText.Render("git purge restore --last"),
		"",
		sBtnDel.Render("y  Delete")+"   "+sBtnBack.Render("esc  Back"),
	)
	dialog := sDialog.Render(strings.Join(body, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog)
}

func badge(c lipgloss.TerminalColor, glyph string, n int, label string) string {
	return lipgloss.NewStyle().Foreground(c).Render(glyph) + " " + sBold.Render(fmt.Sprint(n)) + " " + sMuted.Render(label)
}

// spread places left and right on one line of width w.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return ansi.Truncate(left+" "+right, w, "…")
	}
	return left + strings.Repeat(" ", gap) + right
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
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
