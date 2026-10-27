// Package ui implements the Bubble Tea chat interface.
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/aachtenberg/converse-tui/internal/chat"
	"github.com/aachtenberg/converse-tui/internal/tools"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	botStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	toolStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	addStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	delStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	warnStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	inputStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
)

const (
	inputHeight   = 3
	maxToolRounds = 25
)

type role int

const (
	roleUser role = iota
	roleAssistant
	roleTool
)

type entry struct {
	role    role
	text    string
	preview []string // diff-style lines for tool calls
	note    string   // stop reason / error / tool result shown under the entry
	err     bool
}

type chunkMsg struct {
	ev chat.Event
	ch <-chan chat.Event
}

// Model is the root Bubble Tea model.
type Model struct {
	client  *chat.Client
	tools   *tools.Runner // nil when tools are disabled
	modelID string
	auto    bool // approve file-changing tools without asking

	history   []types.Message // what is sent to Bedrock
	entries   []entry         // what is rendered
	turnStart int             // len(history) before the current user turn

	// Tool calls from the last response still to run, and results so far.
	pending  []chat.ToolUse
	results  []types.ContentBlock
	awaiting bool // waiting for y/n on pending[0]
	outside  bool // pending[0] touches a path outside the working directory
	rounds   int

	vp viewport.Model
	ta textarea.Model
	sp spinner.Model

	streaming bool
	cancel    context.CancelFunc
	width     int
	ready     bool
	inTok     int32
	outTok    int32
}

// New creates the UI model. runner may be nil.
func New(client *chat.Client, runner *tools.Runner, modelID string, auto bool) Model {
	ta := textarea.New()
	ta.Placeholder = "Ask something… (Enter to send, Alt+Enter / Ctrl+J for newline)"
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.SetHeight(inputHeight)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return Model{client: client, tools: runner, modelID: modelID, auto: auto, ta: ta, sp: sp}
}

func (m Model) Init() tea.Cmd { return textarea.Blink }

func (m Model) busy() bool { return m.streaming || m.awaiting }

func waitFor(ch <-chan chat.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return chunkMsg{ev: ev, ch: ch}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.ta.SetWidth(msg.Width - 2)
		vpHeight := msg.Height - 1 /*header*/ - (inputHeight + 2) /*input+border*/ - 1 /*status*/
		if vpHeight < 1 {
			vpHeight = 1
		}
		if !m.ready {
			m.vp = viewport.New(msg.Width, vpHeight)
			m.vp.KeyMap = viewportKeys()
			m.ready = true
		} else {
			m.vp.Width, m.vp.Height = msg.Width, vpHeight
		}
		m.refresh(true)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		case "ctrl+t":
			m.auto = !m.auto
			if m.awaiting && m.auto && !m.outside {
				return m, m.approve(true)
			}
			return m, nil
		case "esc":
			if m.streaming && m.cancel != nil {
				m.cancel()
			} else if m.awaiting {
				m.cancelTurn()
				m.refresh(true)
			}
			return m, nil
		case "ctrl+home":
			m.vp.GotoTop()
			return m, nil
		case "ctrl+end":
			m.vp.GotoBottom()
			return m, nil
		case "pgup", "pgdown", "ctrl+u", "ctrl+d",
			"shift+up", "shift+down", "ctrl+up", "ctrl+down":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		if m.awaiting {
			switch msg.String() {
			case "y", "Y", "enter":
				return m, m.approve(true)
			case "n", "N":
				return m, m.approve(false)
			case "a", "A":
				if m.outside {
					return m, nil
				}
				m.auto = true
				return m, m.approve(true)
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+n":
			if !m.busy() {
				m.history, m.entries = nil, nil
				m.inTok, m.outTok = 0, 0
				m.refresh(true)
			}
			return m, nil
		case "enter":
			if m.busy() {
				return m, nil
			}
			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.ta.Reset()
			m.rounds = 0
			m.entries = append(m.entries, entry{role: roleUser, text: text})
			if n := len(m.history); n > 0 && m.history[n-1].Role == types.ConversationRoleUser {
				// A cancelled turn left a user message (the request or tool
				// results) at the end; add to it so roles keep alternating and
				// the model still sees what happened. turnStart stays at the
				// cancelled turn so an API error rolls both back.
				m.history[n-1].Content = append(m.history[n-1].Content, &types.ContentBlockMemberText{Value: text})
			} else {
				m.turnStart = len(m.history)
				m.history = append(m.history, chat.UserMessage(text))
			}
			return m, m.startStream()
		}

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case chunkMsg:
		last := &m.entries[len(m.entries)-1]
		if !msg.ev.Done {
			last.text += msg.ev.Text
			m.refresh(m.vp.AtBottom())
			return m, waitFor(msg.ch)
		}
		cmd := m.finish(msg.ev)
		m.refresh(true)
		return m, cmd

	case spinner.TickMsg:
		if m.streaming {
			var cmd tea.Cmd
			m.sp, cmd = m.sp.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m *Model) startStream() tea.Cmd {
	m.entries = append(m.entries, entry{role: roleAssistant})
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.streaming = true
	m.refresh(true)
	return tea.Batch(waitFor(m.client.Stream(ctx, m.history)), m.sp.Tick)
}

// finish handles the end of one model response and, if it asked for tools,
// runs them and continues the turn.
func (m *Model) finish(ev chat.Event) tea.Cmd {
	m.streaming = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if ev.Usage != nil {
		if ev.Usage.InputTokens != nil {
			m.inTok += *ev.Usage.InputTokens
		}
		if ev.Usage.OutputTokens != nil {
			m.outTok += *ev.Usage.OutputTokens
		}
	}

	last := &m.entries[len(m.entries)-1]
	switch {
	case errors.Is(ev.Err, context.Canceled):
		m.cancelTurn()
		return nil
	case ev.Err != nil:
		m.abortTurn(ev.Err.Error())
		return nil
	case ev.Message == nil:
		m.abortTurn("empty response (stop reason: " + ev.StopReason + ")")
		return nil
	}

	m.history = append(m.history, *ev.Message)
	if len(ev.ToolUses) == 0 {
		if ev.StopReason != "" && ev.StopReason != string(types.StopReasonEndTurn) {
			last.note = "stop reason: " + ev.StopReason
		}
		return nil
	}
	if strings.TrimSpace(last.text) == "" {
		m.entries = m.entries[:len(m.entries)-1]
	}
	m.pending, m.results = ev.ToolUses, nil
	return m.runTools()
}

// runTools executes pending tool calls until one needs approval or all are
// done, then sends the results back to the model.
func (m *Model) runTools() tea.Cmd {
	for len(m.pending) > 0 {
		tu := m.pending[0]
		t, known := m.tools.Get(tu.Name)
		e := entry{role: roleTool, text: tools.Summary(tu.Name, tu.Input)}
		if known && t.Mutating {
			e.preview = tools.Preview(tu.Name, tu.Input)
		}
		// Paths outside the working directory always need an explicit yes,
		// even in auto mode.
		m.outside = known && m.tools.Outside(tu.Input)
		if m.outside {
			e.note, e.err = "outside working directory", true
		}
		m.entries = append(m.entries, e)
		if m.outside || (known && t.Mutating && !m.auto) {
			m.awaiting = true
			return nil
		}
		m.execPending()
	}
	return m.sendResults()
}

// approve resolves the approval prompt for pending[0].
func (m *Model) approve(ok bool) tea.Cmd {
	m.awaiting = false
	if !ok {
		m.outside = false
	}
	if ok {
		m.execPending()
	} else {
		tu := m.pending[0]
		m.pending = m.pending[1:]
		m.results = append(m.results, chat.ToolResult(tu.ID, "The user denied this action.", true))
		last := &m.entries[len(m.entries)-1]
		last.note, last.err = "denied", true
	}
	cmd := m.runTools()
	m.refresh(true)
	return cmd
}

// execPending runs pending[0], whose entry is the last one.
func (m *Model) execPending() {
	tu := m.pending[0]
	m.pending = m.pending[1:]
	last := &m.entries[len(m.entries)-1]
	out, err := m.tools.Run(tu.Name, tu.Input, m.outside)
	m.outside = false
	if err != nil {
		m.results = append(m.results, chat.ToolResult(tu.ID, "Error: "+err.Error(), true))
		last.note, last.err = err.Error(), true
		return
	}
	m.results = append(m.results, chat.ToolResult(tu.ID, out, false))
	last.note = resultNote(tu.Name, out)
}

func (m *Model) sendResults() tea.Cmd {
	m.history = append(m.history, types.Message{Role: types.ConversationRoleUser, Content: m.results})
	m.results = nil
	m.rounds++
	if m.rounds > maxToolRounds {
		m.abortTurn(fmt.Sprintf("stopped after %d tool rounds", maxToolRounds))
		return nil
	}
	return m.startStream()
}

// cancelTurn stops the turn at the user's request but keeps it in the
// history, so the model still knows what was asked. Unrun tool calls are
// answered as cancelled.
func (m *Model) cancelTurn() {
	if len(m.pending) > 0 {
		for _, tu := range m.pending {
			m.results = append(m.results, chat.ToolResult(tu.ID, "The user cancelled this action.", true))
		}
		m.history = append(m.history, types.Message{Role: types.ConversationRoleUser, Content: m.results})
	}
	m.streaming, m.awaiting, m.outside = false, false, false
	m.pending, m.results = nil, nil
	last := &m.entries[len(m.entries)-1]
	last.note, last.err = "cancelled", true
}

// abortTurn drops the current turn from the history (keeping it on screen)
// so the conversation stays valid for the next message.
func (m *Model) abortTurn(note string) {
	m.streaming, m.awaiting, m.outside = false, false, false
	m.pending, m.results = nil, nil
	m.history = m.history[:m.turnStart]
	last := &m.entries[len(m.entries)-1]
	last.note, last.err = note, true
}

func resultNote(name, out string) string {
	switch name {
	case "read_file", "list_dir", "search":
		n := strings.Count(out, "\n")
		if out != "" && !strings.HasSuffix(out, "\n") {
			n++
		}
		return fmt.Sprintf("%d lines", n)
	}
	return out
}

func (m *Model) refresh(gotoBottom bool) {
	if !m.ready {
		return
	}
	w := m.width - 2
	if w < 10 {
		w = 10
	}
	body := lipgloss.NewStyle().Width(w).PaddingLeft(2)

	var b strings.Builder
	if len(m.entries) == 0 {
		hint := "  Start typing below. Ctrl+N new chat · Esc cancel · wheel/PgUp/PgDn/Shift+↑↓ scroll · Ctrl+C quit"
		if m.tools != nil {
			hint += "\n  File tools on in " + m.tools.Root + " · Ctrl+T toggles auto-approve"
		}
		b.WriteString(dimStyle.Render(hint))
	}
	for i, e := range m.entries {
		if i > 0 && e.role != roleTool {
			b.WriteString("\n")
		}
		switch e.role {
		case roleUser:
			b.WriteString(userStyle.Render("You") + "\n")
		case roleAssistant:
			b.WriteString(botStyle.Render("Assistant") + "\n")
		case roleTool:
			b.WriteString(body.Render(toolStyle.Render("⚙ "+e.text)) + "\n")
			for _, l := range e.preview {
				st := addStyle
				if strings.HasPrefix(l, "- ") {
					st = delStyle
				} else if !strings.HasPrefix(l, "+ ") {
					st = dimStyle
				}
				b.WriteString(body.Render("  "+st.Render(l)) + "\n")
			}
		}
		if e.role != roleTool {
			text := e.text
			if text == "" && m.streaming && i == len(m.entries)-1 {
				text = dimStyle.Render("thinking…")
			}
			if text != "" {
				b.WriteString(body.Render(text) + "\n")
			}
		}
		if e.note != "" {
			st := dimStyle
			if e.err {
				st = errStyle
			}
			b.WriteString(body.Render(st.Render("  → "+e.note)) + "\n")
		}
	}
	m.vp.SetContent(b.String())
	if gotoBottom {
		m.vp.GotoBottom()
	}
}

func (m Model) View() string {
	if !m.ready {
		return "loading…"
	}
	header := headerStyle.Render("converse-tui") + dimStyle.Render(fmt.Sprintf("  %s · %s", m.modelID, m.client.Region()))
	if m.tools != nil && m.auto {
		header += "  " + warnStyle.Render("AUTO")
	}

	status := dimStyle.Render(fmt.Sprintf("tokens in %d · out %d", m.inTok, m.outTok))
	switch {
	case m.awaiting && m.outside:
		status = errStyle.Render("Allow access OUTSIDE the working directory? [y]es  [n]o  Esc cancel")
	case m.awaiting:
		status = warnStyle.Render("Apply this change? [y]es  [n]o  [a]lways (auto mode)  Esc cancel")
	case m.streaming:
		status = m.sp.View() + dimStyle.Render(" streaming (Esc to cancel) · ") + status
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		m.vp.View(),
		inputStyle.Render(m.ta.View()),
		status,
	)
}

// viewportKeys limits viewport scrolling to keys that don't collide with the
// text input.
func viewportKeys() viewport.KeyMap {
	return viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		Up:           key.NewBinding(key.WithKeys("shift+up", "ctrl+up")),
		Down:         key.NewBinding(key.WithKeys("shift+down", "ctrl+down")),
	}
}
