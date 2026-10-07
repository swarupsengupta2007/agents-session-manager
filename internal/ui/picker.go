package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// dirPicker is the modal directory browser shown when the TUI asks for a path.
type dirPicker struct {
	title    string
	subtitle string
	cwd      string
	all      []string // unfiltered names; ".." first when a parent exists
	entries  []string // names after the filter
	cursor   int
	offset   int
	filter   string
	editing  bool
	err      string
	chosen   string
	confirm  string // directory awaiting y/esc (add-home only)
	note     string // warning shown with confirm
}

type pickerResult int

const (
	pickerStay pickerResult = iota
	pickerCancel
	pickerChosen
)

// openPicker starts a directory browser at start, or the nearest existing parent.
func (m Model) openPicker(title, subtitle, start string) Model {
	m.input.Blur()
	m.input.Prompt = "> "
	m.input.SetValue("")
	m.picker = dirPicker{title: title, subtitle: subtitle, cwd: nearestExistingDir(start)}
	m.loadPickerDir()
	return m
}

func (m *Model) endPathEdit() {
	m.picker.editing = false
	m.input.Blur()
	m.input.Prompt = "> "
	m.input.SetValue("")
}

func (m Model) beginPathEdit(seed string) (Model, tea.Cmd) {
	m.picker.editing = true
	m.picker.filter = ""
	m.picker.err = ""
	m.errMsg = ""
	m.applyPickerFilter()
	m.input.Prompt = ""
	m.input.Placeholder = "path (~ and relative ok)"
	m.input.SetValue(seed)
	m.input.CursorEnd()
	m.syncPickerInputWidth()
	return m, m.input.Focus()
}

func (m *Model) syncPickerInputWidth() {
	w := m.pickerTextWidth() - len("path: ")
	if w < 4 {
		w = 4
	}
	m.input.Width = w
}

func (m Model) updateDirPicker(key tea.KeyMsg) (Model, pickerResult, tea.Cmd) {
	if m.picker.editing {
		switch key.String() {
		case "esc":
			m.endPathEdit()
			return m, pickerStay, nil
		case "enter":
			raw := strings.TrimSpace(m.input.Value())
			if raw == "" {
				m.endPathEdit()
				return m, pickerStay, nil
			}
			p, err := resolvePickerPath(raw, m.picker.cwd)
			if err != nil {
				m.picker.err = err.Error()
				return m, pickerStay, nil
			}
			m.picker.chosen = p
			return m, pickerChosen, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(key)
		return m, pickerStay, cmd
	}

	// Letters always filter, so navigation stays on arrows and named keys.
	switch key.String() {
	case "esc":
		if m.picker.filter != "" {
			m.picker.filter = ""
			m.applyPickerFilter()
			return m, pickerStay, nil
		}
		return m, pickerCancel, nil
	case "enter":
		if m.pickerHighlighted() == "." {
			m.picker.chosen = m.picker.cwd
			return m, pickerChosen, nil
		}
		m.pickerOpen()
		return m, pickerStay, nil
	case "right":
		m.pickerOpen()
		return m, pickerStay, nil
	case " ":
		m.pickerChooseHighlighted()
		if m.picker.chosen != "" {
			return m, pickerChosen, nil
		}
		return m, pickerStay, nil
	case "backspace", "ctrl+h":
		if m.picker.filter != "" {
			r := []rune(m.picker.filter)
			m.picker.filter = string(r[:len(r)-1])
			m.applyPickerFilter()
			return m, pickerStay, nil
		}
		m.pickerUp()
		return m, pickerStay, nil
	case "left":
		m.pickerUp()
		return m, pickerStay, nil
	case "up":
		m.pickerMove(-1)
		return m, pickerStay, nil
	case "down":
		m.pickerMove(1)
		return m, pickerStay, nil
	case "pgup":
		m.pickerMove(-m.pickerPage())
		return m, pickerStay, nil
	case "pgdown":
		m.pickerMove(m.pickerPage())
		return m, pickerStay, nil
	case "home":
		m.pickerMoveTo(0)
		return m, pickerStay, nil
	case "end":
		m.pickerMoveTo(len(m.picker.entries) - 1)
		return m, pickerStay, nil
	default:
		if key.Type == tea.KeyRunes && len(key.Runes) > 0 {
			s := string(key.Runes)
			if strings.ContainsAny(s, `/\`) || strings.HasPrefix(s, "~") || filepath.IsAbs(s) {
				nm, cmd := m.beginPathEdit(s)
				return nm, pickerStay, cmd
			}
			m.appendFilter(s)
			return m, pickerStay, nil
		}
		return m, pickerStay, nil
	}
}

func (m Model) pickerHighlighted() string {
	if m.picker.cursor < 0 || m.picker.cursor >= len(m.picker.entries) {
		return ""
	}
	return m.picker.entries[m.picker.cursor]
}

func (m *Model) pickerChooseHighlighted() {
	m.picker.chosen = ""
	switch name := m.pickerHighlighted(); name {
	case "":
	case ".":
		m.picker.chosen = m.picker.cwd
	case "..":
		m.pickerUp()
	default:
		m.picker.chosen = filepath.Join(m.picker.cwd, name)
	}
}

func (m *Model) pickerOpen() {
	if len(m.picker.entries) == 0 || m.picker.cursor < 0 || m.picker.cursor >= len(m.picker.entries) {
		return
	}
	name := m.picker.entries[m.picker.cursor]
	if name == "." {
		return
	}
	next := m.picker.cwd
	if name == ".." {
		next = filepath.Dir(m.picker.cwd)
	} else {
		next = filepath.Join(m.picker.cwd, name)
	}
	st, err := os.Stat(next)
	if err != nil || !st.IsDir() {
		m.picker.err = "not a directory: " + next
		return
	}
	if next == m.picker.cwd {
		return
	}
	m.picker.cwd = next
	m.errMsg = ""
	m.loadPickerDir()
}

func (m *Model) pickerUp() {
	parent := filepath.Dir(m.picker.cwd)
	if parent == m.picker.cwd {
		return
	}
	m.picker.cwd = parent
	m.errMsg = ""
	m.loadPickerDir()
}

func (m *Model) pickerMove(delta int) {
	m.pickerMoveTo(m.picker.cursor + delta)
}

func (m *Model) pickerMoveTo(i int) {
	if len(m.picker.entries) == 0 {
		m.picker.cursor = 0
		m.picker.offset = 0
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= len(m.picker.entries) {
		i = len(m.picker.entries) - 1
	}
	m.picker.cursor = i
	m.errMsg = ""
	m.picker.err = ""
	m.pickerEnsureVisible()
}

func (m *Model) appendFilter(s string) {
	m.picker.filter += s
	m.errMsg = ""
	m.picker.err = ""
	m.applyPickerFilter()
}

func (m *Model) loadPickerDir() {
	m.picker.filter = ""
	m.picker.cursor = 0
	m.picker.offset = 0
	m.picker.err = ""
	entries, err := readSubdirs(m.picker.cwd)
	if err != nil {
		m.picker.err = err.Error()
		entries = nil
		if filepath.Dir(m.picker.cwd) != m.picker.cwd {
			entries = []string{".."}
		}
	}
	// "." chooses the directory being browsed.
	m.picker.all = append([]string{"."}, entries...)
	m.applyPickerFilter()
}

func (m *Model) applyPickerFilter() {
	f := strings.ToLower(m.picker.filter)
	shown := make([]string, 0, len(m.picker.all))
	for _, name := range m.picker.all {
		if isPickerNav(name) || f == "" || strings.Contains(strings.ToLower(name), f) {
			shown = append(shown, name)
		}
	}
	m.picker.entries = shown
	// Start on the first real directory so enter opens rather than chooses.
	m.picker.cursor = 0
	for i, name := range shown {
		if !isPickerNav(name) {
			m.picker.cursor = i
			break
		}
	}
	m.picker.offset = 0
	m.pickerEnsureVisible()
}

func (m *Model) pickerEnsureVisible() {
	page := m.pickerPage()
	if page < 1 {
		page = 1
	}
	if m.picker.cursor < 0 {
		m.picker.cursor = 0
	}
	if m.picker.cursor < m.picker.offset {
		m.picker.offset = m.picker.cursor
	}
	if m.picker.cursor >= m.picker.offset+page {
		m.picker.offset = m.picker.cursor - page + 1
	}
	if m.picker.offset < 0 {
		m.picker.offset = 0
	}
}

// pickerPage is how many directory rows fit under the header without covering it.
func (m Model) pickerPage() int {
	extra := 0
	if m.picker.filter != "" {
		extra++
	}
	if m.picker.editing {
		extra++
	}
	if m.picker.err != "" || m.errMsg != "" {
		extra++
	}
	if m.picker.subtitle != "" {
		extra++
	}
	// header row kept, plus border, title, cwd, spacers, and hint
	p := m.height - 10 - extra
	if p < 1 {
		p = 1
	}
	if p > 12 {
		p = 12
	}
	return p
}

func (m Model) pickerBoxWidth() int {
	// Border sits outside Width, so Width must stay two columns under the terminal.
	w := m.width - 2
	if w > 72 {
		w = 72
	}
	if w < 8 {
		w = 8
	}
	return w
}

func (m Model) pickerTextWidth() int {
	w := m.pickerBoxWidth() - 2
	if w < 8 {
		w = 8
	}
	return w
}

func (m Model) renderDirPicker() string {
	textW := m.pickerTextWidth()
	var b strings.Builder
	b.WriteString(headerStyle.Render(m.picker.title))
	b.WriteString("\n")
	if m.picker.subtitle != "" {
		b.WriteString(dimStyle.Render(truncate(m.picker.subtitle, textW)))
		b.WriteString("\n")
	}
	cwd := truncate(m.picker.cwd, textW-len("this dir "))
	b.WriteString(dimStyle.Render("this dir ") + boldPath(cwd))
	b.WriteString("\n\n")

	if m.picker.confirm != "" {
		b.WriteString("Add " + boldPath(truncate(m.picker.confirm, textW-len("Add "))) + "\n")
		b.WriteString(fmt.Sprintf("as an extra %s home?\n", m.addKind))
		if m.picker.note != "" {
			b.WriteString(errStyle.Render(truncate(m.picker.note, textW)) + "\n")
		}
		b.WriteString("\n" + dimStyle.Render(truncate("y add · esc back to directories", textW)))
		return boxStyle.Width(m.pickerBoxWidth()).Render(b.String())
	}

	page := m.pickerPage()
	offset := m.picker.offset
	if offset < 0 || offset > len(m.picker.entries) {
		offset = 0
	}
	end := offset + page
	if end > len(m.picker.entries) {
		end = len(m.picker.entries)
	}
	if len(m.picker.entries) == 0 {
		b.WriteString(dimStyle.Render(truncate("  (no subdirectories)", textW)))
		b.WriteString("\n")
	}
	for i := offset; i < end; i++ {
		name := m.picker.entries[i]
		label := name
		if name == "." {
			label = ". (use this directory)"
		}
		line := "  " + label
		if i == m.picker.cursor {
			line = "> " + label
		}
		line = truncate(line, textW)
		switch {
		case i == m.picker.cursor:
			line = activeStyle.Render(line)
		case name == ".." || strings.HasPrefix(name, "."):
			line = dimStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	if m.picker.filter != "" {
		b.WriteString(dimStyle.Render(truncate("filter: "+m.picker.filter, textW)))
		b.WriteString("\n")
	}
	if m.picker.editing {
		// Width is capped on the input itself. Truncating View() would cut ANSI.
		m.syncPickerInputWidth()
		b.WriteString("path: " + m.input.View())
		b.WriteString("\n")
	}
	msg := m.picker.err
	if msg == "" {
		msg = m.errMsg
	}
	if msg != "" {
		b.WriteString(errStyle.Render(truncate(msg, textW)))
		b.WriteString("\n")
	}

	hint := "enter open · space select · type to filter · / or ~ path · esc cancel"
	if m.picker.editing {
		hint = "enter use path · esc back to directories"
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(truncate(hint, textW)))

	return boxStyle.Width(m.pickerBoxWidth()).Render(b.String())
}

// placeOverlay draws modal over bg, leaving the top rows (the screen header) in place.
func placeOverlay(bg, modal string, width, height int) string {
	if modal == "" {
		return bg
	}
	if width < 1 || height < 1 {
		return modal
	}
	bgLines := strings.Split(bg, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}
	if len(bgLines) > height {
		bgLines = bgLines[:height]
	}
	modalLines := strings.Split(modal, "\n")
	if len(modalLines) > 0 && modalLines[len(modalLines)-1] == "" {
		modalLines = modalLines[:len(modalLines)-1]
	}
	if len(modalLines) > height {
		modalLines = modalLines[:height]
	}
	start := 2
	if start+len(modalLines) > height {
		start = height - len(modalLines)
	}
	if start < 0 {
		start = 0
	}
	for i, line := range modalLines {
		vis := lipgloss.Width(line)
		pad := (width - vis) / 2
		if pad < 0 {
			pad = 0
		}
		bgLines[start+i] = strings.Repeat(" ", pad) + line
	}
	return strings.Join(bgLines, "\n")
}

func isPickerNav(name string) bool { return name == "." || name == ".." }

// resolvePickerPath expands a leading ~ and resolves relative paths against
// the directory being browsed, not the process cwd.
func resolvePickerPath(raw, base string) (string, error) {
	if strings.HasPrefix(raw, "~") {
		rest := raw[1:]
		if rest != "" && rest[0] != '/' && rest[0] != filepath.Separator {
			return "", fmt.Errorf("~user paths are not supported: %s", raw)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		raw = home + rest
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(base, raw)
	}
	return filepath.Clean(raw), nil
}

func nearestExistingDir(path string) string {
	if path == "" {
		return defaultStartDir()
	}
	path = filepath.Clean(path)
	for {
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return defaultStartDir()
}

// pickerStart opens on the parent of an existing directory so that directory
// is visible in the list. A missing path opens at the nearest existing
// ancestor, or at the user's home when that ancestor is the filesystem root.
func pickerStart(path string) string {
	path = filepath.Clean(path)
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		parent := filepath.Dir(path)
		if parent != path {
			if st, err := os.Stat(parent); err == nil && st.IsDir() {
				return parent
			}
		}
		return path
	}
	found := nearestExistingDir(path)
	if filepath.Dir(found) == found {
		if home, err := os.UserHomeDir(); err == nil {
			if st, err := os.Stat(home); err == nil && st.IsDir() {
				return home
			}
		}
	}
	return found
}

func defaultStartDir() string {
	if wd, err := os.Getwd(); err == nil {
		if st, err := os.Stat(wd); err == nil && st.IsDir() {
			return wd
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			return home
		}
	}
	return string(filepath.Separator)
}

func readSubdirs(cwd string) ([]string, error) {
	ents, err := os.ReadDir(cwd)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range ents {
		name := e.Name()
		if name == "" || name == "." || name == ".." {
			continue
		}
		st, err := os.Stat(filepath.Join(cwd, name))
		if err != nil || !st.IsDir() {
			continue
		}
		dirs = append(dirs, name)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j])
	})
	if filepath.Dir(cwd) != cwd {
		return append([]string{".."}, dirs...), nil
	}
	return dirs, nil
}
