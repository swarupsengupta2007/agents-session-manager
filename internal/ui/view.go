package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"agents-session-manager/internal/agents"
	"agents-session-manager/internal/model"
)

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	orphanStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	selectedFill = lipgloss.Color("236")
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	activeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	lockStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	boxStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
)

const (
	colAgent   = 7
	colOrphan  = 2
	colUpdated = 9
	colMsgs    = 6
	colModel   = 15
	colPadding = 2 // single space between most columns
)

func (m Model) pageSize() int {
	// header, lock row (always reserved), filter, table header, status, footer
	p := m.height - 6 - m.footerLineCount()
	if p < 1 {
		p = 1
	}
	return p
}

// View renders the whole screen.
func (m Model) View() string {
	if m.width == 0 {
		return "…"
	}
	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	b.WriteString(m.renderLockBanner())
	b.WriteString("\n")
	b.WriteString(m.renderFilterBar())
	b.WriteString("\n")

	body := m.renderBody()
	b.WriteString(body)
	b.WriteString("\n")

	b.WriteString(m.renderStatus())
	b.WriteString("\n")
	b.WriteString(m.renderFooter())
	return b.String()
}

func (m Model) renderHeader() string {
	orphans := 0
	for _, s := range m.all {
		if s.Orphan {
			orphans++
		}
	}
	left := titleStyle.Render("agents-session-manager")
	agentNames := make([]string, 0, len(m.agents))
	for _, a := range m.agents {
		agentNames = append(agentNames, agents.AgentLabel(a))
	}
	right := dimStyle.Render(fmt.Sprintf("%d sessions · %d orphaned · agents: %s",
		len(m.all), orphans, strings.Join(agentNames, ", ")))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) renderLockBanner() string {
	l, ok := m.selectedTabLock()
	if !ok {
		return ""
	}
	return lockStyle.Render(truncate("LOCKED  PID "+formatLockPIDs(l), m.width))
}

func formatLockPIDs(l agents.Lock) string {
	if len(l.PIDs) == 0 {
		return "?"
	}
	parts := make([]string, len(l.PIDs))
	for i, p := range l.PIDs {
		parts[i] = fmt.Sprintf("%d", p)
	}
	return strings.Join(parts, ", ")
}

func (m Model) renderFilterBar() string {
	parts := []string{"All"}
	for _, a := range m.agents {
		parts = append(parts, agents.AgentLabel(a))
	}
	for i, p := range parts {
		locked := m.tabLocked(i)
		switch {
		case i == m.agentFilter && locked:
			parts[i] = lockStyle.Render("[ " + p + " ]")
		case i == m.agentFilter:
			parts[i] = activeStyle.Render("[ " + p + " ]")
		case locked:
			parts[i] = lockStyle.Render(p)
		default:
			parts[i] = dimStyle.Render(p)
		}
	}
	bar := strings.Join(parts, " ")
	if m.orphansOnly {
		bar += "  " + orphanStyle.Render("orphans only")
	}
	if m.searching {
		bar += "  /" + m.input.View()
	} else if m.query != "" {
		bar += dimStyle.Render("  /" + m.query)
	}
	if m.scanning {
		bar += "  " + m.spinner.View() + dimStyle.Render(" scanning…")
	}
	return bar
}

func (m Model) renderBody() string {
	if m.mode == modeRemapPreview && m.remapPlan != nil {
		return m.renderPlanPreview()
	}
	list := m.renderList()
	if m.detail {
		if s := m.selected(); s != nil {
			detailW := m.width * 2 / 5
			if detailW > 60 {
				detailW = 60
			}
			if detailW >= 34 {
				return lipgloss.JoinHorizontal(lipgloss.Top, list, " ", m.renderDetail(*s, detailW))
			}
		}
	}
	return list
}

func (m Model) renderList() string {
	page := m.pageSize()
	offset := m.offset
	if offset < 0 {
		offset = 0
	}

	listW := m.width
	if m.detail {
		detailW := m.width * 2 / 5
		if detailW > 60 {
			detailW = 60
		}
		if detailW >= 34 {
			listW = m.width - detailW - 1
		}
	}

	var rows []string
	if !m.loaded {
		rows = append(rows, dimStyle.Render("scanning for sessions…"))
	} else if len(m.agents) == 0 {
		rows = append(rows, "No agent storage found (looked for ~/.claude, ~/.codex, ~/.grok, ~/.gemini, ~/.qwen, ~/.local/share/muse).")
	} else if len(m.filtered) == 0 {
		if len(m.all) == 0 {
			rows = append(rows, dimStyle.Render("No sessions found."))
		} else {
			rows = append(rows, dimStyle.Render("No sessions match the current filters."))
		}
	} else {
		rows = append(rows, m.tableHeader(listW))
		end := offset + page - 1 // leave row 0 for the table header
		if end > len(m.filtered) {
			end = len(m.filtered)
		}
		for i := offset; i < end; i++ {
			rows = append(rows, m.renderRow(m.filtered[i], i == m.cursor, listW))
		}
	}
	return strings.Join(rows, "\n")
}

func (m Model) tableHeader(w int) string {
	rest := w - (colAgent + colOrphan + colUpdated + colMsgs + colModel)
	projW, titleW := splitRest(rest)
	h := strings.Join([]string{
		pad("AGENT", colAgent-1),
		pad("", colOrphan-1),
		pad("UPDATED", colUpdated-1),
		padLeft("MSGS", colMsgs-1),
		pad("MODEL", colModel-1),
		pad("PROJECT", projW),
		truncate("TITLE", titleW),
	}, " ")
	return headerStyle.Render(truncatePlain(h, w))
}

func splitRest(rest int) (projW, titleW int) {
	if rest < 20 {
		return 10, 10
	}
	projW = rest * 2 / 5
	titleW = rest - projW - 1
	return projW, titleW
}

func (m Model) renderRow(s model.Session, selected bool, w int) string {
	rest := w - (colAgent + colOrphan + colUpdated + colMsgs + colModel)
	projW, titleW := splitRest(rest)

	orphan := " "
	if s.Orphan {
		orphan = orphanStyle.Render("!")
	} else if m.sessionLocked(s) || s.Active {
		orphan = lockStyle.Render("L")
	}
	proj := truncate(s.Cwd, projW)
	if s.Orphan {
		proj = orphanStyle.Render(truncate(s.Cwd, projW))
	}
	title := truncate(s.Title, titleW)
	if title == "" {
		title = dimStyle.Render(truncate(s.ID, titleW))
	}
	row := strings.Join([]string{
		pad(string(s.Kind), colAgent-1),
		pad(orphan, colOrphan-1),
		pad(humanAge(s.UpdatedAt), colUpdated-1),
		padLeft(fmt.Sprint(s.Messages), colMsgs-1),
		pad(truncate(s.Model, colModel-1), colModel-1),
		pad(proj, projW),
		truncate(title, titleW),
	}, " ")

	if selected {
		style := lipgloss.NewStyle().Background(selectedFill)
		return style.Render(row)
	}
	if m.sessionLocked(s) {
		return dimStyle.Render(row)
	}
	return row
}

// pad pads a possibly-styled string to w using its visual width.
func pad(s string, w int) string {
	if sw := lipgloss.Width(s); sw >= w {
		return s
	}
	return s + strings.Repeat(" ", w-lipgloss.Width(s))
}

func padLeft(s string, w int) string {
	sw := lipgloss.Width(s)
	if sw >= w {
		return s
	}
	return strings.Repeat(" ", w-sw) + s
}

func (m Model) renderDetail(s model.Session, width int) string {
	inner := width - 4 // border + padding
	label := func(l string) string { return dimStyle.Render(fmt.Sprintf("%-9s", l)) }
	wrap := func(v string) string { return wrapText(v, inner-9) }

	orphanNote := ""
	if s.Orphan {
		orphanNote = " " + orphanStyle.Render("(ORPHANED — press m to remap)")
	}
	if s.Active {
		orphanNote += " " + activeStyle.Render("(ACTIVE)")
	}
	if m.sessionLocked(s) {
		orphanNote += " " + lockStyle.Render("(LOCKED — agent running)")
	}
	agentName := string(s.Kind)
	if s.Store != "" {
		agentName += "  " + dimStyle.Render(s.Store)
	}
	lines := []string{
		headerStyle.Render("Session detail"),
		"",
		label("Agent") + agentName,
		label("ID") + wrapText(s.ID, inner-9),
		label("Title") + wrap(s.Title),
		label("Model") + orDash(s.Model),
		label("Project") + wrap(s.Cwd) + orphanNote,
		label("Messages") + fmt.Sprint(s.Messages),
		label("Size") + humanSize(s.SizeBytes),
		label("Created") + orDash(humanStamp(s.CreatedAt)),
		label("Updated") + orDash(humanStamp(s.UpdatedAt)),
		"",
		label("File"),
		wrapText(s.File, inner),
	}
	return boxStyle.Width(width - 2).Render(strings.Join(lines, "\n"))
}

func (m Model) renderPlanPreview() string {
	p := m.remapPlan
	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf("Migration plan — %s", p.Agent)))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  %s  →  %s\n", boldPath(p.OldCwd), boldPath(p.NewCwd)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("  originals will be copied to %s before any change", m.backupRoot)))
	b.WriteString("\n\n")
	max := m.pageSize() - 5
	if max < 1 {
		max = 1
	}
	for i, a := range p.Actions {
		if i >= max {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  … and %d more actions", len(p.Actions)-max)))
			b.WriteString("\n")
			break
		}
		b.WriteString("  " + okStyle.Render("•") + " " + a.Desc + "\n")
	}
	return b.String()
}

func (m Model) renderStatus() string {
	switch {
	case m.applying:
		return m.spinner.View() + " applying migration…"
	case m.mode == modeRenameInput:
		s := m.renameTarget
		cur := s.Title
		if cur == "" {
			cur = s.ID
		}
		return fmt.Sprintf("Rename %s %s — enter new name, enter to save, esc to cancel:",
			s.Kind, truncate(cur, 40))
	case m.mode == modeAddStore:
		return fmt.Sprintf("Add extra %s home — enter path, enter to save, esc to cancel:", m.addKind)
	case m.mode == modeRemapInput:
		s := m.remapGroup[0]
		return fmt.Sprintf("Remap %d %s session(s) from %s — enter new project path, enter to confirm, esc to cancel:",
			len(m.remapGroup), s.Kind, boldPath(s.Cwd))
	case m.mode == modeRemapPreview:
		return fmt.Sprintf("Remap %s → %s — %d actions. %s to apply, %s to cancel.",
			boldPath(m.remapPlan.OldCwd), boldPath(m.remapPlan.NewCwd), len(m.remapPlan.Actions),
			activeStyle.Render("y"), dimStyle.Render("esc"))
	case m.mode == modeDeleteConfirm:
		return fmt.Sprintf("Delete %s session %s (%s)? %s to confirm, any other key cancels.",
			m.deleteTarget.Kind, truncate(m.deleteTarget.ID, 36), boldPath(m.deleteTarget.Cwd),
			activeStyle.Render("y"))
	case m.errMsg != "":
		return errStyle.Render("✗ " + truncate(m.errMsg, m.width))
	case m.status != "":
		if strings.Contains(m.status, "WARNING") {
			return errStyle.Render(truncate(m.status, m.width))
		}
		return okStyle.Render(truncate(m.status, m.width))
	case len(m.scanErrs) > 0:
		msgs := make([]string, 0, len(m.scanErrs))
		for k, e := range m.scanErrs {
			msgs = append(msgs, string(k)+": "+e.Error())
		}
		return errStyle.Render("scan errors — " + truncate(strings.Join(msgs, "; "), m.width))
	}
	return ""
}

func (m Model) footerKeys() string {
	switch m.mode {
	case modeRemapInput:
		return "enter confirm · esc cancel"
	case modeRemapPreview:
		return "y apply · esc cancel"
	case modeDeleteConfirm:
		return "y delete · esc cancel"
	case modeAddStore:
		return "enter save · esc cancel"
	case modeRenameInput:
		return "enter save · esc cancel"
	default:
		return "↑/↓ move · enter detail · m remap · n rename · x delete · r resume · a extra-dir · / search · tab/S-tab agent · o orphans · L locks · R refresh · q quit"
	}
}

func (m Model) footerLineCount() int {
	n := strings.Count(wrapGuide(m.footerKeys(), m.width), "\n") + 1
	if n < 1 {
		return 1
	}
	return n
}

func (m Model) renderFooter() string {
	return dimStyle.Render(wrapGuide(m.footerKeys(), m.width))
}

// wrapGuide wraps s to width at " · " separators so each key legend stays
// intact. A single token longer than width is hard-wrapped as a last resort.
func wrapGuide(s string, width int) string {
	if width < 1 || s == "" {
		return s
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	parts := strings.Split(s, " · ")
	var lines []string
	var cur string
	flush := func() {
		if cur != "" {
			lines = append(lines, cur)
			cur = ""
		}
	}
	for _, p := range parts {
		if p == "" {
			continue
		}
		if cur == "" {
			if lipgloss.Width(p) <= width {
				cur = p
				continue
			}
			lines = append(lines, hardWrap(p, width)...)
			continue
		}
		next := cur + " · " + p
		if lipgloss.Width(next) <= width {
			cur = next
			continue
		}
		flush()
		if lipgloss.Width(p) <= width {
			cur = p
			continue
		}
		wrapped := hardWrap(p, width)
		lines = append(lines, wrapped[:len(wrapped)-1]...)
		cur = wrapped[len(wrapped)-1]
	}
	flush()
	return strings.Join(lines, "\n")
}

func hardWrap(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var out []string
	var cur []rune
	curW := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if curW > 0 && curW+rw > width {
			out = append(out, string(cur))
			cur = cur[:0]
			curW = 0
		}
		cur = append(cur, r)
		curW += rw
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

// --- helpers ---

func boldPath(p string) string {
	return lipgloss.NewStyle().Bold(true).Render(p)
}

func orDash(s string) string {
	if s == "" {
		return dimStyle.Render("—")
	}
	return s
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// truncatePlain truncates by rune count without styling awareness; used only
// for already-rendered rows where padding matters less than not overflowing.
func truncatePlain(s string, w int) string {
	return truncate(s, w)
}

func wrapText(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if s == "" {
		return dimStyle.Render("—")
	}
	var out []string
	runes := []rune(s)
	for len(runes) > 0 {
		if len(runes) <= w {
			out = append(out, string(runes))
			break
		}
		out = append(out, string(runes[:w]))
		runes = runes[w:]
	}
	return strings.Join(out, "\n"+strings.Repeat(" ", 9))
}

func humanAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("02Jan06")
	}
}

func humanStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}
