// Package ui implements the Bubble Tea chat interface.
package ui

import (
	"context"
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
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	botStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	inputStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
)

const inputHeight = 3

type entry struct {
	user bool
	text string
	note string // stop reason / error shown under the message
	err  bool
}

type chunkMsg struct {
	ev chat.Event
	ch <-chan chat.Event
}

// Model is the root Bubble Tea model.
type Model struct {
	client  *chat.Client
	modelID string

	history []types.Message // what is sent to Bedrock
	entries []entry         // what is rendered

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

// New creates the UI model.
func New(client *chat.Client, modelID string) Model {
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

	return Model{client: client, modelID: modelID, ta: ta, sp: sp}
}

func (m Model) Init() tea.Cmd { return textarea.Blink }

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
	var cmds []tea.Cmd

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
		case "esc":
			if m.streaming && m.cancel != nil {
				m.cancel()
			}
			return m, nil
		case "ctrl+n":
			if !m.streaming {
				m.history, m.entries = nil, nil
				m.inTok, m.outTok = 0, 0
				m.refresh(true)
			}
			return m, nil
		case "enter":
			if m.streaming {
				return m, nil
			}
			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.ta.Reset()
			return m, m.send(text)
		case "pgup", "pgdown", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}

	case chunkMsg:
		last := &m.entries[len(m.entries)-1]
		if !msg.ev.Done {
			last.text += msg.ev.Text
			m.refresh(m.vp.AtBottom())
			return m, waitFor(msg.ch)
		}
		m.finish(msg.ev)
		m.refresh(true)
		return m, nil

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
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m *Model) send(text string) tea.Cmd {
	m.entries = append(m.entries, entry{user: true, text: text}, entry{})
	m.history = append(m.history, chat.UserMessage(text))

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.streaming = true
	m.refresh(true)

	ch := m.client.Stream(ctx, m.history)
	return tea.Batch(waitFor(ch), m.sp.Tick)
}

func (m *Model) finish(ev chat.Event) {
	m.streaming = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	last := &m.entries[len(m.entries)-1]
	if ev.Usage != nil {
		if ev.Usage.InputTokens != nil {
			m.inTok += *ev.Usage.InputTokens
		}
		if ev.Usage.OutputTokens != nil {
			m.outTok += *ev.Usage.OutputTokens
		}
	}

	switch {
	case ev.Err != nil:
		last.err = true
		if ev.Err == context.Canceled {
			last.note = "cancelled"
		} else {
			last.note = ev.Err.Error()
		}
	case strings.TrimSpace(last.text) == "":
		last.err = true
		last.note = "empty response (stop reason: " + ev.StopReason + ")"
	default:
		if ev.StopReason != "" && ev.StopReason != string(types.StopReasonEndTurn) {
			last.note = "stop reason: " + ev.StopReason
		}
		m.history = append(m.history, chat.AssistantMessage(last.text))
		return
	}
	// Failed turn: drop the unanswered user message so roles keep alternating.
	m.history = m.history[:len(m.history)-1]
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
		b.WriteString(dimStyle.Render("  Start typing below. Ctrl+N new chat · Esc cancel · PgUp/PgDn scroll · Ctrl+C quit"))
	}
	for i, e := range m.entries {
		if i > 0 {
			b.WriteString("\n")
		}
		if e.user {
			b.WriteString(userStyle.Render("You") + "\n")
		} else {
			b.WriteString(botStyle.Render("Assistant") + "\n")
		}
		text := e.text
		if text == "" && !e.user && m.streaming && i == len(m.entries)-1 {
			text = dimStyle.Render("thinking…")
		}
		if text != "" {
			b.WriteString(body.Render(text) + "\n")
		}
		if e.note != "" {
			st := dimStyle
			if e.err {
				st = errStyle
			}
			b.WriteString(body.Render(st.Render("["+e.note+"]")) + "\n")
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

	status := fmt.Sprintf("tokens in %d · out %d", m.inTok, m.outTok)
	if m.streaming {
		status = m.sp.View() + " streaming (Esc to cancel) · " + status
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		m.vp.View(),
		inputStyle.Render(m.ta.View()),
		dimStyle.Render(status),
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
	}
}
