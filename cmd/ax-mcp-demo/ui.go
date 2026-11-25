package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rakyll/ax-mcp/internal/mcp"
)

var (
	bold   = lipgloss.NewStyle().Bold(true)
	faint  = lipgloss.NewStyle().Faint(true)
	red    = lipgloss.NewStyle().Foreground(lipgloss.Red)
	green  = lipgloss.NewStyle().Foreground(lipgloss.Green)
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	blue   = lipgloss.NewStyle().Foreground(lipgloss.BrightWhite).Background(lipgloss.Blue)
	purple = lipgloss.NewStyle().Foreground(lipgloss.Magenta)
	cyan   = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	banner = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightWhite).Background(lipgloss.Magenta)
)

func statusIcon(status string) (string, lipgloss.Style) {
	switch status {
	case mcp.StatusCompleted:
		return "✔", green
	case mcp.StatusFailed:
		return "✖", red
	case mcp.StatusCancelled:
		return "⊘", faint
	case mcp.StatusInputRequired:
		return "?", purple
	}
	return "●", yellow
}

// doneMsg tells the UI the tour is over.
type doneMsg struct{}

// elicitMsg asks the UI to put a task's elicitation input request to the user.
type elicitMsg struct {
	taskID, job string
	key         string // the inputRequests key
	params      mcp.ElicitParams
	reply       chan mcp.ElicitResult
}

// autoMsg and typeMsg drive -auto's answers: pick the next field's answer,
// and type the next character of a text answer.
type (
	autoMsg struct{ seq int }
	typeMsg struct{ seq int }
)

type model struct {
	d     *demo
	width int
	spin  spinner.Model
	snap  snapshot
	form  *form
	seq   int
	done  bool
}

func newModel(d *demo) model {
	return model{d: d, width: 100, spin: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(yellow))}
}

func (m model) Init() tea.Cmd { return m.spin.Tick }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		m.snap = m.d.snapshot()
		return m, cmd
	case doneMsg:
		m.done = true
		return m, tea.Quit
	case elicitMsg:
		m.seq++
		m.form = newForm(msg, m.seq)
		cmd := m.form.focus()
		if *auto {
			cmd = tea.Batch(cmd, autoAfter(m.seq, 800*time.Millisecond))
		}
		return m, cmd
	case autoMsg:
		if m.form == nil || msg.seq != m.form.seq {
			return m, nil
		}
		f := m.form
		if fl := f.field(); fl.kind == "string" {
			f.typing = []rune(autoAnswers[fl.name])
			return m, typeAfter(f.seq)
		}
		f.choice = f.autoChoice()
		return m, m.commit()
	case typeMsg:
		if m.form == nil || msg.seq != m.form.seq {
			return m, nil
		}
		f := m.form
		if len(f.typing) == 0 {
			return m, m.commit()
		}
		f.input.SetValue(f.input.Value() + string(f.typing[0]))
		f.input.CursorEnd()
		f.typing = f.typing[1:]
		return m, typeAfter(f.seq)
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.d.cancel()
			return m, tea.Quit
		}
		if m.form != nil && !*auto {
			return m.formKey(msg)
		}
	}
	if m.form != nil && m.form.field().kind == "string" {
		var cmd tea.Cmd
		m.form.input, cmd = m.form.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) formKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := m.form
	fl := f.field()
	switch msg.String() {
	case "esc":
		f.reply <- mcp.ElicitResult{Action: "decline"}
		m.form = nil
		return m, nil
	case "enter":
		return m, m.commit()
	}
	if fl.kind == "string" {
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		return m, cmd
	}
	n := len(fl.options)
	switch msg.String() {
	case "left", "up", "shift+tab", "h", "k":
		f.choice = (f.choice + n - 1) % n
	case "right", "down", "tab", "l", "j":
		f.choice = (f.choice + 1) % n
	case "y":
		if fl.kind == "bool" {
			f.choice = 0
		}
	case "n":
		if fl.kind == "bool" {
			f.choice = 1
		}
	}
	return m, nil
}

// commit records the current field's answer and moves to the next field, or
// answers the elicitation after the last one.
func (m *model) commit() tea.Cmd {
	f := m.form
	fl := f.field()
	switch fl.kind {
	case "bool":
		f.content[fl.name] = f.choice == 0
	case "enum":
		f.content[fl.name] = fl.options[f.choice]
	default:
		f.content[fl.name] = f.input.Value()
	}
	f.i++
	if f.i == len(f.fields) {
		f.reply <- mcp.ElicitResult{Action: "accept", Content: f.content}
		m.form = nil
		return nil
	}
	cmd := f.focus()
	if *auto {
		cmd = tea.Batch(cmd, autoAfter(f.seq, 600*time.Millisecond))
	}
	return cmd
}

func autoAfter(seq int, d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return autoMsg{seq} })
}

func typeAfter(seq int) tea.Cmd {
	return tea.Tick(25*time.Millisecond, func(time.Time) tea.Msg { return typeMsg{seq} })
}

func (m model) View() tea.View {
	if m.done || m.d.plain {
		return tea.NewView("")
	}
	var b strings.Builder
	if m.snap.board {
		b.WriteString(renderBoard(m.snap, m.spin.View(), m.width, true))
	} else if m.snap.activity != "" {
		b.WriteString("  " + m.spin.View() + " " + faint.Render(m.snap.activity+"…") + "\n")
	}
	if m.form != nil {
		b.WriteString(m.form.view(m.width))
	}
	return tea.NewView(b.String())
}

// renderBoard draws the task table, its counters and the event log. spin is
// the spinner frame for working tasks; width 0 means don't truncate.
func renderBoard(s snapshot, spin string, width int, events bool) string {
	var b strings.Builder
	b.WriteString(faint.Render(fmt.Sprintf("  %-20s %-9s %-16s %s", "TASK", "JOB", "STATUS", "STATUS MESSAGE")) + "\n")
	for _, r := range s.rows {
		icon, style := statusIcon(r.status)
		if r.status == mcp.StatusWorking && spin != "" {
			icon = spin
		}
		msg, _, _ := strings.Cut(r.message, "\n")
		if width > 0 {
			msg = ansi.Truncate(msg, max(10, width-51), "…")
		}
		fmt.Fprintf(&b, "  %s %-9s %s %s %s\n", cyan.Render(fmt.Sprintf("%-20s", r.id)), r.job, icon, style.Render(fmt.Sprintf("%-14s", r.status)), msg)
	}
	b.WriteString("\n" + faint.Render(fmt.Sprintf("  notifications/tasks: %d   tasks/get: %d   server restarts: %d", s.notifs, s.polls, s.restarts)) + "\n")
	if events {
		for _, e := range s.events {
			b.WriteString("  " + e + "\n")
		}
	}
	return b.String()
}

type field struct {
	name, title string
	kind        string // bool, enum, or string
	options     []string
	def         string
}

// form is an elicitation/create request being answered.
type form struct {
	elicitMsg
	seq     int
	fields  []field
	i       int // the field being answered
	choice  int // the selected option of a bool or enum field
	input   textinput.Model
	typing  []rune // what -auto has left to type
	content map[string]any
}

func newForm(msg elicitMsg, seq int) *form {
	f := &form{elicitMsg: msg, seq: seq, content: map[string]any{}, input: textinput.New()}
	f.input.Prompt = ""
	props, _ := msg.params.RequestedSchema["properties"].(map[string]any)
	for _, name := range fieldOrder(msg.params.RequestedSchema) {
		prop, _ := props[name].(map[string]any)
		fl := field{name: name, kind: "string"}
		fl.title, _ = prop["title"].(string)
		if fl.title == "" {
			fl.title = name
		}
		switch enum, _ := prop["enum"].([]any); {
		case prop["type"] == "boolean":
			fl.kind, fl.options, fl.def = "bool", []string{"yes", "no"}, "no"
			if v, _ := prop["default"].(bool); v {
				fl.def = "yes"
			}
		case len(enum) > 0:
			fl.kind = "enum"
			for _, o := range enum {
				fl.options = append(fl.options, fmt.Sprint(o))
			}
			fl.def, _ = prop["default"].(string)
		}
		f.fields = append(f.fields, fl)
	}
	return f
}

func (f *form) field() field { return f.fields[f.i] }

// focus gets the current field ready for an answer.
func (f *form) focus() tea.Cmd {
	fl := f.field()
	if fl.kind == "string" {
		f.input.SetValue("")
		return f.input.Focus()
	}
	f.input.Blur()
	f.choice = max(0, slices.Index(fl.options, fl.def))
	return nil
}

// autoChoice is what -auto picks: yes, or the field's default.
func (f *form) autoChoice() int {
	if f.field().kind == "bool" {
		return 0
	}
	return max(0, slices.Index(f.field().options, f.field().def))
}

func (f *form) view(width int) string {
	w := min(max(width-4, 40), 96)
	var b strings.Builder
	b.WriteString(purple.Bold(true).Render("elicitation/create") + " " + faint.Render("inputRequests["+strconv.Quote(f.key)+"] · task "+f.taskID+" · job "+f.job) + "\n")
	b.WriteString(bold.Width(w-4).Render(f.params.Message) + "\n\n")
	titleW := 0
	for _, fl := range f.fields {
		titleW = max(titleW, lipgloss.Width(fl.title))
	}
	f.input.SetWidth(w - titleW - 10)
	for i, fl := range f.fields {
		marker := "  "
		if i == f.i {
			marker = purple.Render("› ")
		}
		b.WriteString(marker + bold.Render(fmt.Sprintf("%-*s", titleW, fl.title)) + "  ")
		switch {
		case i < f.i:
			b.WriteString(green.Render(fmt.Sprint(f.answer(fl))))
		case i > f.i:
			b.WriteString(faint.Render("…"))
		case fl.kind == "string":
			b.WriteString(f.input.View())
		default:
			for j, o := range fl.options {
				if j == f.choice {
					b.WriteString(lipgloss.NewStyle().Reverse(true).Bold(true).Render(" "+o+" ") + " ")
				} else {
					b.WriteString(faint.Render(" "+o+" ") + " ")
				}
			}
		}
		b.WriteString("\n")
	}
	help := "enter confirm · ←/→ choose · esc decline"
	if *auto {
		help = "-auto is answering"
	}
	b.WriteString("\n" + faint.Render(help))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Magenta).Padding(0, 1).Margin(1, 0, 0, 2).Width(w).Render(b.String()) + "\n"
}

// autoContent answers an elicitation the way -auto does, without a form.
func autoContent(msg elicitMsg) map[string]any {
	f := newForm(msg, 0)
	for f.i = range f.fields {
		switch fl := f.field(); fl.kind {
		case "bool":
			f.content[fl.name] = true
		case "enum":
			f.content[fl.name] = fl.options[f.autoChoice()]
		default:
			f.content[fl.name] = autoAnswers[fl.name]
		}
	}
	return f.content
}

func (f *form) answer(fl field) any {
	v := f.content[fl.name]
	if b, ok := v.(bool); ok {
		return map[bool]string{true: "yes", false: "no"}[b]
	}
	return v
}
