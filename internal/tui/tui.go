// Package tui is the interactive branch picker.
package tui

import (
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bograh/git-purge/internal/classify"
	"github.com/bograh/git-purge/internal/gitx"
	"github.com/bograh/git-purge/internal/store"
)

// Item is a branch shown in the picker. Repo is a display label, set only when
// branches from several repositories are shown together; RepoIndex lets the
// caller map a chosen item back to its repository.
type Item struct {
	store.Branch
	Repo      string
	RepoIndex int
}

// Label is the name shown in the list: "repo › branch" in multi-repo mode.
func (it Item) Label() string {
	if it.Repo == "" {
		return it.Name
	}
	return it.Repo + " › " + it.Name
}

// PreviewFunc loads what deleting a branch would remove.
type PreviewFunc func(Item) (*gitx.Preview, error)

// Options configure the picker.
type Options struct {
	Header  string
	Preview PreviewFunc // nil disables the preview pane
}

var rank = map[string]int{
	string(classify.Safe): 0, string(classify.Review): 1, string(classify.Stale): 2,
	string(classify.Active): 3, string(classify.Local): 4, string(classify.Protected): 5,
}

type row struct {
	b        Item
	order    int // original position, for restoring the default order
	selected bool

	// Rendered-line cache: rows are redrawn only when their own state changes.
	cache    string
	cacheKey rowKey
}

type rowKey struct {
	selected, cursor bool
	width, nameW     int
}

// line is one rendered list line: a section heading or a branch row.
type line struct {
	heading string
	r       *row
	idx     int // index into vis for rows
}

// Model is the bubbletea model.
type Model struct {
	opt       Options
	rows      []*row
	cursor    int
	offset    int // first rendered list line
	width     int
	height    int
	showKept  bool
	oldest    bool // sort oldest first within each section
	filter    string
	filtering bool
	confirm   bool
	Confirmed bool

	// Derived from rows + filter + showKept + oldest; rebuilt by refresh()
	// only when those change, so cursor movement stays O(1).
	vis   []*row
	lns   []line
	nameW int

	pv      *previewState // non-nil while the preview pane is open
	pvCache map[string]previewMsg
}

// New builds the picker. Safe branches start selected; review and stale don't.
func New(items []Item, opt Options) *Model {
	m := &Model{opt: opt, height: 30, width: 100}
	for i, it := range items {
		m.rows = append(m.rows, &row{b: it, order: i, selected: it.Status == string(classify.Safe)})
	}
	sort.SliceStable(m.rows, func(i, j int) bool { return rank[m.rows[i].b.Status] < rank[m.rows[j].b.Status] })
	m.refresh()
	return m
}

// Selected returns the items the user chose.
func (m *Model) Selected() []Item {
	var out []Item
	for _, r := range m.rows {
		if r.selected && classify.Status(r.b.Status).Deletable() {
			out = append(out, r.b)
		}
	}
	return out
}

// refresh rebuilds the visible rows, list lines and name column width.
func (m *Model) refresh() {
	q := strings.ToLower(m.filter)
	m.vis = m.vis[:0]
	for _, r := range m.rows {
		if !m.showKept && !classify.Status(r.b.Status).Deletable() {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.b.Label()), q) {
			continue
		}
		m.vis = append(m.vis, r)
	}
	sort.SliceStable(m.vis, func(i, j int) bool {
		a, b := m.vis[i], m.vis[j]
		if ra, rb := rank[a.b.Status], rank[b.b.Status]; ra != rb {
			return ra < rb
		}
		if m.oldest && !a.b.CommitTime.Equal(b.b.CommitTime) {
			return a.b.CommitTime.Before(b.b.CommitTime)
		}
		return a.order < b.order
	})

	m.lns = m.lns[:0]
	prev := ""
	for i, r := range m.vis {
		if s := sectionOf(r.b.Status); s != prev {
			if prev != "" {
				m.lns = append(m.lns, line{heading: " "})
			}
			m.lns = append(m.lns, line{heading: s})
			prev = s
		}
		m.lns = append(m.lns, line{r: r, idx: i})
	}

	n := 12
	for _, r := range m.vis {
		if l := lipgloss.Width(r.b.Label()); l > n {
			n = l
		}
	}
	m.nameW = min(n, max(12, m.width*2/5))
}

func sectionOf(status string) string {
	switch classify.Status(status) {
	case classify.Safe:
		return "Safe to delete"
	case classify.Review:
		return "Needs review"
	case classify.Stale:
		return "Stale"
	}
	return "Kept"
}

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) current() *row {
	if m.cursor >= 0 && m.cursor < len(m.vis) {
		return m.vis[m.cursor]
	}
	return nil
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.refresh()
		m.clamp()
		return m, nil
	case previewMsg:
		m.receivePreview(msg)
		return m, nil
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
		if m.pv != nil {
			return m, m.updatePreview(msg)
		}
		if m.filtering {
			return m, m.updateFilter(msg)
		}
		return m, m.updateList(msg)
	}
	return m, nil
}

func (m *Model) updateFilter(msg tea.KeyMsg) tea.Cmd {
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
		return tea.Quit
	case tea.KeyRunes, tea.KeySpace:
		m.filter += string(msg.Runes)
	}
	m.refresh()
	m.cursor = 0
	m.clamp()
	return nil
}

func (m *Model) updateList(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "q", "ctrl+c":
		return tea.Quit
	case "esc":
		if m.filter == "" {
			return tea.Quit
		}
		m.filter = ""
		m.refresh()
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
		m.cursor = len(m.vis) - 1
	case " ", "x":
		if r := m.current(); r != nil && classify.Status(r.b.Status).Deletable() {
			r.selected = !r.selected
			m.cursor++
		}
	case "s":
		m.toggleAll(classify.Safe)
	case "r":
		m.toggleAll(classify.Review)
	case "t":
		m.toggleAll(classify.Stale)
	case "n":
		for _, r := range m.rows {
			r.selected = false
		}
	case "h":
		m.keepCursor(func() { m.showKept = !m.showKept })
	case "o":
		m.keepCursor(func() { m.oldest = !m.oldest })
	case "p", "tab":
		if r := m.current(); r != nil && m.opt.Preview != nil {
			return m.openPreview(r)
		}
	case "enter", "d":
		if len(m.Selected()) > 0 {
			m.confirm = true
		}
	}
	m.clamp()
	return nil
}

// keepCursor applies a change that reorders the list, keeping the cursor on
// the same branch when it is still visible.
func (m *Model) keepCursor(change func()) {
	cur := m.current()
	change()
	m.refresh()
	m.cursor = 0
	for i, r := range m.vis {
		if r == cur {
			m.cursor = i
		}
	}
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
	return max(3, m.height-headerLines-detailLines-footerLines)
}

func (m *Model) clamp() {
	m.cursor = max(0, min(m.cursor, len(m.vis)-1))
	// Scroll by rendered lines so section headings stay in view.
	at := 0
	for i, l := range m.lns {
		if l.r != nil && l.idx == m.cursor {
			at = i
			break
		}
	}
	top := at
	if top > 0 && m.lns[top-1].heading != "" { // keep the heading above the first row visible
		top--
	}
	lh := m.listHeight()
	if top < m.offset {
		m.offset = top
	}
	if at >= m.offset+lh {
		m.offset = at - lh + 1
	}
	m.offset = max(0, min(m.offset, len(m.lns)-lh))
}

// Run shows the picker and returns the chosen items (nil if cancelled).
func Run(items []Item, opt Options) ([]Item, error) {
	// Detect the terminal background before bubbletea starts reading stdin.
	// Done lazily on first render instead, the OSC 11 query races the input
	// reader and can stall startup or leak escape codes as keypresses.
	lipgloss.HasDarkBackground()
	m := New(items, opt)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		return nil, err
	}
	if !m.Confirmed {
		return nil, nil
	}
	return m.Selected(), nil
}

// Age renders how long ago t was, compactly ("3d", "5mo").
func Age(t time.Time) string { return age(t) }
