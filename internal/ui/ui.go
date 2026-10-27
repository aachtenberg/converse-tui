// Package ui implements the Bubble Tea chat interface.
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/aachtenberg/converse-tui/internal/chat"
	"github.com/aachtenberg/converse-tui/internal/config"
	"github.com/aachtenberg/converse-tui/internal/tools"
)

const (
	maxInputHeight = 8
	maxToolRounds  = 25
)

type role int

const (
	roleUser role = iota
	roleAssistant
	roleTool
	roleInfo
)

type entry struct {
	role    role
	text    string
	label   string   // tool calls: e.g. Update(main.go)
	preview []string // diff-style lines for tool calls
	note    string   // stop reason / error / tool result shown under the entry
	err     bool
	done    bool // tool call finished

	// Cached markdown rendering of a finished assistant message.
	rendered      string
	renderedWidth int
}

type chunkMsg struct {
	ev chat.Event
	ch <-chan chat.Event
}

// Options configures the UI.
type Options struct {
	Config config.Config
	Banner string // rendered banner art; empty hides it
}

// Model is the root Bubble Tea model.
type Model struct {
	client  *chat.Client
	tools   *tools.Runner // nil when tools are disabled
	modelID string
	auto    bool // approve file-changing tools without asking
	banner  string
	title   string
	th      theme

	markdown bool
	md       *glamour.TermRenderer
	mdWidth  int

	history   []types.Message // what is sent to Bedrock
	entries   []entry         // what is rendered
	turnStart int             // len(history) before the current user turn

	// Tool calls from the last response still to run, and results so far.
	pending  []chat.ToolUse
	results  []types.ContentBlock
	awaiting bool // waiting for approval of pending[0]
	outside  bool // pending[0] touches a path outside the working directory
	choice   int  // highlighted option in the approval prompt
	rounds   int

	vp viewport.Model
	ta textarea.Model
	sp spinner.Model

	streaming bool
	started   time.Time
	cancel    context.CancelFunc
	width     int
	height    int
	ready     bool
	inTok     int32
	outTok    int32
}

// New creates the UI model. runner may be nil.
func New(client *chat.Client, runner *tools.Runner, opts Options) Model {
	th := newTheme(opts.Config)

	ta := textarea.New()
	ta.Placeholder = "Type a message · /help for commands"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "> "
		}
		return "  "
	})
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = th.dim
	ta.FocusedStyle.Placeholder = th.faint
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Spinner{Frames: spinnerFrames, FPS: 120 * time.Millisecond}
	sp.Style = th.accent

	return Model{
		client:   client,
		tools:    runner,
		modelID:  opts.Config.ModelID,
		auto:     opts.Config.AutoApprove,
		banner:   opts.Banner,
		title:    opts.Config.Banner.Title,
		th:       th,
		markdown: opts.Config.MarkdownEnabled(),
		ta:       ta,
		sp:       sp,
	}
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
		m.width, m.height = msg.Width, msg.Height
		m.ta.SetWidth(max(msg.Width-4, 10)) // border + padding
		if !m.ready {
			m.vp = viewport.New(msg.Width, 1)
			m.vp.KeyMap = viewportKeys()
			m.ready = true
		}
		m.vp.Width = msg.Width
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
			return m, m.toggleAuto()
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
			return m, m.approvalKey(msg.String())
		}
		switch msg.String() {
		case "ctrl+n":
			if !m.busy() {
				m.reset()
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
			m.fitInput()
			if strings.HasPrefix(text, "/") {
				return m, m.command(text)
			}
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
	if m.fitInput() {
		m.refresh(m.vp.AtBottom())
	}
	return m, cmd
}

// fitInput grows the input box with its content; it reports whether the
// height changed.
func (m *Model) fitInput() bool {
	w := max(m.ta.Width(), 1)
	rows := 0
	for _, line := range strings.Split(m.ta.Value(), "\n") {
		rows += max(1, (lipgloss.Width(line)+w)/w)
	}
	h := min(max(rows, 1), maxInputHeight)
	if h == m.ta.Height() {
		return false
	}
	m.ta.SetHeight(h)
	return true
}

func (m *Model) reset() {
	m.history, m.entries = nil, nil
	m.inTok, m.outTok = 0, 0
	m.refresh(true)
}

func (m *Model) toggleAuto() tea.Cmd {
	if m.tools == nil {
		return nil
	}
	m.auto = !m.auto
	if m.awaiting && m.auto && !m.outside {
		return m.approve(true)
	}
	m.refresh(m.vp.AtBottom())
	return nil
}

func (m *Model) info(text string) {
	m.entries = append(m.entries, entry{role: roleInfo, text: text})
	m.refresh(true)
}

// command runs a slash command.
func (m *Model) command(text string) tea.Cmd {
	switch strings.Fields(text)[0] {
	case "/help", "/?":
		m.info(helpText)
	case "/clear", "/new":
		m.reset()
	case "/auto":
		if m.tools == nil {
			m.info("File tools are disabled.")
			return nil
		}
		cmd := m.toggleAuto()
		state := "off"
		if m.auto {
			state = "on"
		}
		m.info("Auto-accept edits " + state + ".")
		return cmd
	case "/exit", "/quit":
		return tea.Quit
	default:
		m.info("Unknown command " + text + " · /help lists commands")
	}
	return nil
}

const helpText = `Commands
  /help          show this help
  /clear, /new   start a new conversation
  /auto          toggle auto-accept for file edits
  /exit          quit

Keys
  enter              send            alt+enter, ctrl+j   new line
  esc                interrupt       ctrl+t              toggle auto-accept
  wheel, pgup/pgdn   scroll          shift+↑/↓           scroll a line
  ctrl+home/end      top / bottom    ctrl+c              quit`

func (m *Model) startStream() tea.Cmd {
	m.entries = append(m.entries, entry{role: roleAssistant})
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.streaming = true
	if m.rounds == 0 {
		m.started = time.Now()
	}
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
		e := entry{role: roleTool, label: toolLabel(tu.Name, tu.Input)}
		if known && t.Mutating {
			e.preview = tools.Preview(tu.Name, tu.Input)
		}
		// Paths outside the working directory always need an explicit yes,
		// even in auto mode.
		m.outside = known && m.tools.Outside(tu.Input)
		if m.outside {
			e.note = "outside working directory"
		}
		m.entries = append(m.entries, e)
		if m.outside || (known && t.Mutating && !m.auto) {
			m.awaiting, m.choice = true, 0
			return nil
		}
		m.execPending()
	}
	return m.sendResults()
}

// approvalOptions lists the choices for the current prompt.
func (m *Model) approvalOptions() []string {
	if m.outside {
		return []string{"Yes", "No, and tell the model (esc to stop)"}
	}
	return []string{"Yes", "Yes, and auto-accept edits this session (ctrl+t)", "No, and tell the model (esc to stop)"}
}

func (m *Model) approvalKey(k string) tea.Cmd {
	opts := m.approvalOptions()
	pick := -1
	switch k {
	case "up", "k":
		m.choice = (m.choice + len(opts) - 1) % len(opts)
	case "down", "j", "tab":
		m.choice = (m.choice + 1) % len(opts)
	case "enter":
		pick = m.choice
	case "y", "Y", "1":
		pick = 0
	case "a", "A":
		if !m.outside {
			pick = 1
		}
	case "2", "3":
		if i := int(k[0] - '1'); i < len(opts) {
			pick = i
		}
	case "n", "N":
		pick = len(opts) - 1
	}
	if pick < 0 {
		m.refresh(m.vp.AtBottom())
		return nil
	}
	switch {
	case pick == 0:
		return m.approve(true)
	case pick == len(opts)-1:
		return m.approve(false)
	default: // auto-accept
		m.auto = true
		return m.approve(true)
	}
}

// approve resolves the approval prompt for pending[0].
func (m *Model) approve(ok bool) tea.Cmd {
	m.awaiting = false
	if ok {
		m.execPending()
	} else {
		m.outside = false
		tu := m.pending[0]
		m.pending = m.pending[1:]
		m.results = append(m.results, chat.ToolResult(tu.ID, "The user denied this action.", true))
		last := &m.entries[len(m.entries)-1]
		last.note, last.err, last.done = "denied", true, true
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
	last.done = true
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
	last.note, last.err, last.done = "interrupted", true, true
}

// abortTurn drops the current turn from the history (keeping it on screen)
// so the conversation stays valid for the next message.
func (m *Model) abortTurn(note string) {
	m.streaming, m.awaiting, m.outside = false, false, false
	m.pending, m.results = nil, nil
	m.history = m.history[:m.turnStart]
	last := &m.entries[len(m.entries)-1]
	last.note, last.err, last.done = note, true, true
}

func resultNote(name, out string) string {
	switch name {
	case "read_file", "list_dir", "search":
		if out == "no matches" || out == "(empty directory)" {
			return out
		}
		n := strings.Count(out, "\n")
		if out != "" && !strings.HasSuffix(out, "\n") {
			n++
		}
		unit := "lines"
		switch name {
		case "list_dir":
			unit = "entries"
		case "search":
			unit = "matches"
		}
		return fmt.Sprintf("%d %s", n, unit)
	}
	return out
}

// toolLabel renders a call as Verb(arg), e.g. Update(main.go).
func toolLabel(name string, in map[string]any) string {
	verbs := map[string]string{
		"list_dir": "List", "read_file": "Read", "search": "Search",
		"write_file": "Write", "edit_file": "Update",
	}
	verb, ok := verbs[name]
	if !ok {
		verb = name
	}
	path, _ := in["path"].(string)
	if name == "search" {
		pat, _ := in["pattern"].(string)
		arg := fmt.Sprintf("%q", pat)
		if path != "" && path != "." {
			arg += " in " + path
		}
		return verb + "(" + arg + ")"
	}
	if path == "" {
		path = "."
	}
	return verb + "(" + path + ")"
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
