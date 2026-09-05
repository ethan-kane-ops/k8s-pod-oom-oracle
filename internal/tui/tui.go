package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/render"
)

// Layout constants.
const (
	// reportWidth is how wide internal/render's text report is. The trajectory
	// chart sets it, and every golden file in that package confirms it. It is
	// stated here because the split layout is only worth offering when the
	// report fits beside the list rather than wrapping into noise.
	reportWidth = 108
	// listWidth is how much of a split layout the report list takes.
	listWidth = 48
	// paneChrome is the border and padding a pane adds around its content.
	paneChrome = 4
	// splitWidth is the terminal width at which both panes fit side by side.
	// Below it the dashboard shows one pane at a time, switched with tab.
	splitWidth = listWidth + paneChrome + reportWidth + paneChrome
	// chromeHeight is the header, footer and pane borders the panes sit inside.
	chromeHeight = 8
)

// focus is which pane receives keys.
type focus int

const (
	focusList focus = iota
	focusDetail
)

// Options configures a dashboard.
type Options struct {
	// Client reads the daemon. Required.
	Client Client
	// Interval is how often to refresh. Zero means the default.
	Interval time.Duration
	// Addr is shown in the header so a reader knows which daemon this is.
	Addr string
	// Now supplies the clock, so tests can assert on rendered ages.
	Now func() time.Time
}

// Model is the dashboard state. It satisfies tea.Model.
type Model struct {
	client   Client
	interval time.Duration
	addr     string
	now      func() time.Time

	snapshot Snapshot
	// err is the last fetch failure, cleared by the next success. The previous
	// snapshot is deliberately kept on screen underneath it: a daemon that has
	// just died is exactly when its last reports matter most.
	err       error
	lastFetch time.Time

	selected int
	focus    focus
	detail   viewport.Model

	width, height int
	ready         bool
	quitting      bool
}

var _ tea.Model = Model{}

// New builds a dashboard.
func New(opts Options) Model {
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return Model{
		client:   opts.Client,
		interval: opts.Interval,
		addr:     opts.Addr,
		now:      opts.Now,
	}
}

// snapshotMsg carries one refresh. An error travels with it rather than
// replacing it, so a failed refresh never discards the reports already shown.
type snapshotMsg struct {
	snapshot Snapshot
	err      error
	at       time.Time
}

// tickMsg schedules the next refresh.
type tickMsg time.Time

// Init satisfies tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), m.tick())
}

func (m Model) fetch() tea.Cmd {
	client, now := m.client, m.now
	return func() tea.Msg {
		snapshot, err := client.Fetch(context.Background())
		return snapshotMsg{snapshot: snapshot, err: err, at: now()}
	}
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update satisfies tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		m.ready = true
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetch(), m.tick())

	case snapshotMsg:
		return m.applySnapshot(msg), nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// applySnapshot folds a refresh into the model.
//
// The selection follows the report it was on rather than the index it was at.
// Reports arrive newest first, so a new kill shifts every index down by one and
// an index-based selection would silently jump to a different report at the
// moment one arrives, which is the moment someone is reading it.
func (m Model) applySnapshot(msg snapshotMsg) Model {
	m.lastFetch = msg.at
	m.err = msg.err
	if msg.err != nil {
		return m
	}

	var selectedID string
	if report, ok := m.current(); ok {
		selectedID = report.ID
	}

	m.snapshot = msg.snapshot
	m.selected = 0
	if selectedID != "" {
		for i := range m.snapshot.Reports {
			if m.snapshot.Reports[i].ID == selectedID {
				m.selected = i
				break
			}
		}
	}

	m.refreshDetail()
	return m
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "r":
		return m, m.fetch()

	case "tab":
		if m.focus == focusList {
			m.focus = focusDetail
		} else {
			m.focus = focusList
		}
		return m, nil

	case "j", "down":
		if m.focus == focusDetail {
			m.detail.ScrollDown(1)
			return m, nil
		}
		return m.move(1), nil

	case "k", "up":
		if m.focus == focusDetail {
			m.detail.ScrollUp(1)
			return m, nil
		}
		return m.move(-1), nil

	case "g", "home":
		return m.moveTo(0), nil

	case "G", "end":
		return m.moveTo(len(m.snapshot.Reports) - 1), nil

	case "pgdown", " ":
		m.detail.HalfPageDown()
		return m, nil

	case "pgup":
		m.detail.HalfPageUp()
		return m, nil
	}

	return m, nil
}

func (m Model) move(delta int) Model { return m.moveTo(m.selected + delta) }

func (m Model) moveTo(index int) Model {
	if len(m.snapshot.Reports) == 0 {
		return m
	}
	m.selected = min(max(index, 0), len(m.snapshot.Reports)-1)
	m.refreshDetail()
	return m
}

// current returns the selected report.
func (m Model) current() (oom.Report, bool) {
	if m.selected < 0 || m.selected >= len(m.snapshot.Reports) {
		return oom.Report{}, false
	}
	return m.snapshot.Reports[m.selected], true
}

// resize recomputes the viewport for the current terminal.
func (m *Model) resize() {
	width, height := m.detailSize()
	if !m.ready {
		m.detail = viewport.New(width, height)
	} else {
		m.detail.Width, m.detail.Height = width, height
	}
	m.refreshDetail()
}

func (m Model) detailSize() (width, height int) {
	height = max(m.height-chromeHeight, 3)
	if m.split() {
		return max(m.width-listWidth-2*paneChrome, 20), height
	}
	return max(m.width-paneChrome, 20), height
}

// split reports whether both panes fit side by side.
func (m Model) split() bool { return m.width >= splitWidth }

// refreshDetail re-renders the selected report into the viewport.
func (m *Model) refreshDetail() {
	report, ok := m.current()
	if !ok {
		m.detail.SetContent(m.emptyDetail())
		return
	}
	// The same renderer `oom-oracle inspect` uses, so the dashboard cannot drift
	// from the CLI's account of the same report.
	m.detail.SetContent(render.Text(&report, render.TextOptions{}))
	m.detail.GotoTop()
}

func (m Model) emptyDetail() string {
	if m.err != nil {
		return styleAlert.Render("Cannot reach the daemon.") + "\n\n" +
			styleMuted.Render(m.err.Error()) + "\n\n" +
			styleMuted.Render("Retrying every "+m.interval.String()+". Press r to retry now.")
	}
	return styleMuted.Render(
		"No OOM kills recorded.\n\n" +
			"The daemon is watching this node. This pane fills in when the kernel\n" +
			"kills a process in a container, which on a healthy node is never.")
}

// View satisfies tea.Model.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	if !m.ready {
		return "starting…\n"
	}

	sections := []string{m.header()}
	if m.split() {
		sections = append(sections, lipgloss.JoinHorizontal(
			lipgloss.Top,
			m.pane(focusList, m.list(), listWidth),
			m.pane(focusDetail, m.detail.View(), 0),
		))
	} else {
		// One pane at a time. The report is fixed-width text and half of a
		// narrow terminal renders it as wrapped noise.
		if m.focus == focusList {
			sections = append(sections, m.pane(focusList, m.list(), 0))
		} else {
			sections = append(sections, m.pane(focusDetail, m.detail.View(), 0))
		}
	}
	sections = append(sections, m.footer())

	return strings.Join(sections, "\n")
}

func (m Model) pane(which focus, content string, width int) string {
	style := stylePane
	if m.focus == which {
		style = stylePaneFocused
	}
	switch {
	case width > 0:
		style = style.Width(width)
	case m.split():
		style = style.Width(max(m.width-listWidth-paneChrome-2, 20))
	default:
		style = style.Width(max(m.width-2, 20))
	}
	_, height := m.detailSize()
	return style.Height(height).Render(content)
}

// header states what the daemon is and whether it is healthy.
func (m Model) header() string {
	status := m.snapshot.Status

	title := styleTitle.Render("oom-oracle") + styleMuted.Render("  "+m.addr)

	detector := styleMuted.Render("detector ") + m.detectorBadge()
	node := ""
	if status.Node != "" {
		node = styleMuted.Render("  node ") + status.Node
	}
	ready := styleOK.Render("ready")
	if !status.Ready {
		ready = styleWarn.Render("not ready")
	}

	counts := fmt.Sprintf("%s %d  %s %d  %s %s",
		styleMuted.Render("reports"), status.Reports,
		styleMuted.Render("tracking"), status.TrackedCgroups,
		styleMuted.Render("unattributed"), m.unattributed())

	return title + "\n" +
		detector + node + "  " + ready + "\n" +
		counts
}

// detectorBadge names the detector and says what it costs when it is the
// fallback. "poller" alone reads as a configuration detail rather than as
// "every victim on this screen is a guess".
func (m Model) detectorBadge() string {
	switch m.snapshot.Status.Detector {
	case "ebpf":
		return styleOK.Render("ebpf")
	case "poller":
		return styleWarn.Render("poller") + styleMuted.Render(" (victims are inferred, not traced)")
	case "":
		return styleMuted.Render("unknown")
	default:
		return m.snapshot.Status.Detector
	}
}

// unattributed is the counter worth alerting on: kills from inside the kubepods
// tree that could not be placed. Skipped climbs on any busy node and means
// nothing on its own, so it is not shown here.
func (m Model) unattributed() string {
	count := m.snapshot.Status.Unattributed
	if count == 0 {
		return styleOK.Render("0")
	}
	return styleAlert.Render(fmt.Sprintf("%d", count))
}

// list renders the report list.
func (m Model) list() string {
	if len(m.snapshot.Reports) == 0 {
		if m.err != nil {
			return styleAlert.Render("daemon unreachable")
		}
		return styleMuted.Render("no kills recorded")
	}

	var b strings.Builder
	for i := range m.snapshot.Reports {
		report := &m.snapshot.Reports[i]

		marker := "  "
		line := m.listRow(report, m.listInnerWidth())
		if i == m.selected {
			marker = styleSelected.Render("▌ ")
			line = styleSelected.Render(line)
		}
		b.WriteString(marker + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// listInnerWidth is how much room a row has once the pane's padding and the
// selection marker are taken out. A row wider than this wraps, and a wrapped row
// makes the list unreadable rather than merely truncated.
func (m Model) listInnerWidth() int {
	width := listWidth
	if !m.split() {
		width = m.width - 2
	}
	return max(width-paneChrome, 20)
}

// listRow formats one report to fit exactly.
//
// The time is fixed-width and never abbreviated: it is what someone matches
// against a pod restart or an alert. What remains is split two-to-one between
// the workload name and the victim, because a name identifies the report and a
// comm only confirms it.
func (m Model) listRow(report *oom.Report, width int) string {
	name := report.Identity.PodName
	if name == "" {
		// Before the pod cache syncs a report identifies its pod by UID alone.
		name = report.Identity.PodUID
	}
	if report.Identity.ContainerName != "" {
		name += "/" + report.Identity.ContainerName
	}

	victim := report.Victim.Comm
	if !report.Victim.Known {
		victim = "unknown"
	}

	const (
		timeWidth = 8
		// The kernel truncates comm to 15 characters, so nothing is gained by
		// giving the victim column more than that. Everything else goes to the
		// name, which is what identifies the report.
		maxVictimWidth = 15
	)
	remaining := max(width-timeWidth-2, 6)
	victimWidth := min(maxVictimWidth, remaining/3)
	nameWidth := remaining - victimWidth

	return fmt.Sprintf("%s %-*s %-*s",
		report.Time.Local().Format("15:04:05"),
		nameWidth, shorten(name, nameWidth),
		victimWidth, shorten(victim, victimWidth))
}

// footer shows the keys and how fresh the screen is.
func (m Model) footer() string {
	keys := styleMuted.Render("↑↓ move  tab pane  r refresh  q quit")

	freshness := styleMuted.Render("never refreshed")
	if !m.lastFetch.IsZero() {
		age := m.now().Sub(m.lastFetch).Truncate(time.Second)
		freshness = styleMuted.Render(fmt.Sprintf("updated %s ago", age))
	}
	if m.err != nil {
		freshness = styleAlert.Render("daemon unreachable: " + shorten(m.err.Error(), 60))
	}

	return keys + "  " + freshness
}

// shorten truncates with an ellipsis so a long pod name cannot break the
// layout. Truncation keeps the leading characters: generated pod names differ at
// the end, but what identifies the workload is at the front.
func shorten(text string, width int) string {
	if width <= 1 || len([]rune(text)) <= width {
		return text
	}
	return string([]rune(text)[:width-1]) + "…"
}
