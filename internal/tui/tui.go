// Package tui is the interactive branch picker.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/store"
)

// Palette: every color adapts to light and dark terminal backgrounds.
var (
	cAccent = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#A78BFA"}
	cSafe   = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}
	cReview = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
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
	}
	return cMuted
}

var rank = map[string]int{
	string(classify.Safe): 0, string(classify.Review): 1, string(classify.Active): 2,
	string(classify.Local): 3, string(classify.Protected): 4,
}

type row struct {
	b        store.Branch
	selected bool
}

// line is one rendered list line: a section heading or a branch row.
type line struct {
	heading string
	r       *row
	idx     int // index into visible() for rows
}

// Model is the bubbletea model.
type Model struct {
	rows      []*row
	cursor    int
	offset    int // first rendered list line
	width     int
	height    int
	showKept  bool
	filter    string
	filtering bool
	confirm   bool
	Confirmed bool
	header    string
}

// New builds the picker. Safe branches start selected; review branches don't.
func New(branches []store.Branch, header string) *Model {
	m := &Model{header: header, height: 30, width: 100}
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
	var out []*row
	q := strings.ToLower(m.filter)
	for _, r := range m.rows {
		if !m.showKept && !classify.Status(r.b.Status).Deletable() {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.b.Name), q) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func sectionOf(status string) string {
	switch classify.Status(status) {
	case classify.Safe:
		return "Safe to delete"
	case classify.Review:
		return "Needs review"
	}
	return "Kept"
}

func (m *Model) lines() []line {
	var out []line
	prev := ""
	for i, r := range m.visible() {
		if s := sectionOf(r.b.Status); s != prev {
			if prev != "" {
				out = append(out, line{heading: " "})
			}
			out = append(out, line{heading: s})
			prev = s
		}
		out = append(out, line{r: r, idx: i})
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
			case "ctrl+c":
				return m, tea.Quit
			}
			m.confirm = false
			return m, nil
		}
		if m.filtering {
			switch msg.Type {
			case tea.KeyEsc:
				m.filter, m.filtering = "", false
			case tea.KeyEnter:
				m.filtering = false
			case tea.KeyBackspace:
				if r := []rune(m.filter); len(r) > 0 {
					m.filter = string(r[:len(r)-1])
				}
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyRunes, tea.KeySpace:
				m.filter += string(msg.Runes)
			}
			m.cursor = 0
			m.clamp()
			return m, nil
		}
		vis := m.visible()
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.filter != "" {
				m.filter = ""
			} else {
				return m, tea.Quit
			}
		case "/":
			m.filtering = true
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
		case "s":
			m.toggleAll(classify.Safe)
		case "r":
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

const (
	headerLines = 3 // title, filter/summary, blank
	detailLines = 6 // 4 content + border
	footerLines = 2
)

func (m *Model) listHeight() int {
	h := m.height - headerLines - detailLines - footerLines
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
	// Scroll by rendered lines so section headings stay in view.
	lines := m.lines()
	at := 0
	for i, l := range lines {
		if l.r != nil && l.idx == m.cursor {
			at = i
			break
		}
	}
	top := at
	if top > 0 && lines[top-1].heading != "" { // keep the heading above the first row visible
		top--
	}
	lh := m.listHeight()
	if top < m.offset {
		m.offset = top
	}
	if at >= m.offset+lh {
		m.offset = at - lh + 1
	}
	if max := len(lines) - lh; m.offset > max {
		m.offset = max
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) counts() (safe, review, kept, selected int) {
	for _, r := range m.rows {
		switch classify.Status(r.b.Status) {
		case classify.Safe:
			safe++
		case classify.Review:
			review++
		default:
			kept++
		}
		if r.selected {
			selected++
		}
	}
	return
}

func (m *Model) View() string {
	if m.confirm {
		return m.viewConfirm()
	}
	w := m.width
	var b strings.Builder

	// Header: brand + context on the left, count badges on the right.
	safe, review, kept, _ := m.counts()
	left := sBrand.Render("git-purge") + "  " + sMuted.Render(m.header)
	right := badge(cSafe, "●", safe, "safe") + "  " + badge(cReview, "●", review, "review") + "  " + badge(cMuted, "○", kept, "kept")
	b.WriteString(spread(left, right, w) + "\n")

	// Filter / scroll position line.
	lines := m.lines()
	var sub string
	switch {
	case m.filtering:
		sub = sFilterOn.Render("/ ") + sText.Render(m.filter) + sFilterOn.Render("▏")
	case m.filter != "":
		sub = sMuted.Render("filter: ") + sText.Render(m.filter) + sFaint.Render("  (esc to clear)")
	}
	pos := ""
	if lh := m.listHeight(); len(lines) > lh {
		end := m.offset + lh
		if end > len(lines) {
			end = len(lines)
		}
		pos = sFaint.Render(fmt.Sprintf("%d%%", 100*end/len(lines)))
	}
	b.WriteString(spread(sub, pos, w) + "\n\n")

	// List.
	lh := m.listHeight()
	rendered := 0
	if len(lines) == 0 {
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
	nameW := m.nameWidth()
	for i := m.offset; i < len(lines) && rendered < lh; i++ {
		b.WriteString(m.renderLine(lines[i], nameW) + "\n")
		rendered++
	}
	for ; rendered < lh; rendered++ {
		b.WriteString("\n")
	}

	// Detail card + footer.
	b.WriteString(m.viewDetail() + "\n")
	b.WriteString(m.viewFooter())
	return b.String()
}

func (m *Model) nameWidth() int {
	n := 12
	for _, r := range m.visible() {
		if l := lipgloss.Width(r.b.Name); l > n {
			n = l
		}
	}
	if max := m.width * 2 / 5; n > max {
		n = max
	}
	return n
}

func (m *Model) renderLine(l line, nameW int) string {
	w := m.width
	if l.r == nil {
		if strings.TrimSpace(l.heading) == "" {
			return ""
		}
		var color lipgloss.TerminalColor = cMuted
		switch l.heading {
		case "Safe to delete":
			color = cSafe
		case "Needs review":
			color = cReview
		}
		title := sSection.Foreground(color).Render(strings.ToUpper(l.heading))
		rule := sFaint.Render(strings.Repeat("─", max(0, w-lipgloss.Width(title)-4)))
		return "  " + title + " " + rule
	}

	r := l.r
	isCursor := l.idx == m.cursor
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
	name := st(nameStyle).Width(nameW).Render(ansi.Truncate(r.b.Name, nameW, "…"))

	gone := st(lipgloss.NewStyle()).Width(7).Render("")
	if r.b.Gone {
		gone = st(sGone).Width(7).Render("✗ gone")
	}
	tag := ""
	if !classify.Status(r.b.Status).Deletable() {
		tag = st(sMuted).Render(r.b.Status) + st(lipgloss.NewStyle()).Render("  ")
	}
	age := st(sMuted).Width(5).Align(lipgloss.Right).Render(age(r.b.CommitTime))

	sp := st(lipgloss.NewStyle()).Render(" ")
	prefix := bar + box + sp + sp + name + sp + sp + gone + age + sp + sp + tag
	room := w - lipgloss.Width(prefix) - 1
	reason := st(sMuted).Render(ansi.Truncate(r.b.Reason, max(0, room), "…"))
	out := prefix + reason
	if pad := w - 1 - lipgloss.Width(out); isCursor && pad > 0 {
		out += st(lipgloss.NewStyle()).Render(strings.Repeat(" ", pad))
	}
	return out
}

func (m *Model) viewDetail() string {
	inner := max(10, m.width-4)
	vis := m.visible()
	var content []string
	if m.cursor < len(vis) {
		b := vis[m.cursor].b
		color := statusColor(b.Status)
		title := sBold.Render(b.Name) + "  " + lipgloss.NewStyle().Foreground(color).Render("● "+b.Status)
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

func (m *Model) viewFooter() string {
	_, _, _, selected := m.counts()
	keys := []struct{ k, d string }{
		{"space", "toggle"}, {"s", "safe"}, {"r", "review"}, {"n", "none"},
		{"/", "filter"}, {"h", "kept"}, {"q", "quit"},
	}
	if m.filtering {
		keys = []struct{ k, d string }{{"enter", "apply"}, {"esc", "clear"}}
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, sKey.Render(k.k)+" "+sMuted.Render(k.d))
	}
	help := strings.Join(parts, sFaint.Render("  ·  "))
	action := sFaint.Render("nothing selected")
	if selected > 0 {
		action = sKey.Render("enter") + " " + sDanger.Render(fmt.Sprintf("delete %d", selected))
	}
	return " " + spread(help, action+" ", m.width-1)
}

func (m *Model) viewConfirm() string {
	sel := m.Selected()
	review := 0
	for _, b := range sel {
		if b.Status == string(classify.Review) {
			review++
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
		body = append(body, "  "+dot+" "+sText.Render(ansi.Truncate(b.Name, 48, "…")))
	}
	body = append(body, "")
	if review > 0 {
		body = append(body, lipgloss.NewStyle().Foreground(cReview).Bold(true).
			Render(fmt.Sprintf("⚠ %d of them have no proof of being merged.", review)))
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

// Age is exported for the plain-text list output.
func Age(t time.Time) string { return age(t) }
