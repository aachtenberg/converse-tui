package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// refresh re-lays out the screen and rebuilds the transcript.
func (m *Model) refresh(gotoBottom bool) {
	if !m.ready {
		return
	}
	m.vp.Height = max(m.height-lipgloss.Height(m.bottom()), 1)
	m.vp.SetContent(m.transcript())
	if gotoBottom {
		m.vp.GotoBottom()
	}
}

func (m Model) View() string {
	if !m.ready {
		return ""
	}
	return m.vp.View() + "\n" + m.bottom()
}

func (m *Model) transcript() string {
	w := max(m.width-1, 20)
	parts := []string{m.welcome(w)}
	for i := range m.entries {
		if s := m.renderEntry(&m.entries[i], i == len(m.entries)-1, w); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func (m *Model) welcome(w int) string {
	th := m.th
	var lines []string
	inner := w - 4 // border + padding
	if m.banner != "" && lipgloss.Width(m.banner) <= inner {
		lines = append(lines, th.accent.Render(m.banner), "")
	}
	title := m.title
	if title == "" {
		title = "Welcome to converse-tui"
	}
	lines = append(lines, th.accent.Render("✻ ")+th.bold.Render(title), "")
	kv := func(k, v string) string {
		if room := inner - 9; lipgloss.Width(v) > room && room > 10 {
			r := []rune(v)
			v = "…" + string(r[len(r)-room+1:])
		}
		return th.dim.Render(fmt.Sprintf("  %-7s", k)) + v
	}
	lines = append(lines, kv("model", m.modelID), kv("region", m.client.Region()))
	if m.tools != nil {
		edits := "ask before changing files"
		if m.auto {
			edits = th.warn.Render("auto-accept")
		}
		lines = append(lines, kv("cwd", m.tools.Root), kv("edits", edits))
	} else {
		lines = append(lines, kv("tools", "off"))
	}
	body := strings.Join(lines, "\n")

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(th.accentBorder).Padding(0, 1)
	if lipgloss.Width(body)+4 > w {
		box = box.Width(w - 2)
	}
	tips := th.dim.Render("  /help for commands · esc to interrupt · ctrl+t to auto-accept edits")
	return box.Render(body) + "\n" + tips
}

func (m *Model) renderEntry(e *entry, last bool, w int) string {
	th := m.th
	cw := max(w-2, 10)
	wrap := lipgloss.NewStyle().Width(cw)

	switch e.role {
	case roleUser:
		return prefixBlock(th.dim.Render("> "), "  ", wrap.Foreground(th.userMsg.GetForeground()).Render(e.text))

	case roleInfo:
		return prefixBlock("  ", "  ", th.dim.Render(e.text))

	case roleAssistant:
		var out string
		if e.text != "" {
			out = prefixBlock(th.bold.Render(glyphBullet)+" ", "  ", m.assistantBody(e, last, cw))
		}
		if e.note != "" {
			note := resultLine(th, e.note, e.err, cw)
			if out == "" {
				out = prefixBlock(th.bad.Render(glyphBullet)+" ", "  ", note)
			} else {
				out += "\n" + note
			}
		}
		return out

	case roleTool:
		bullet := th.dim
		switch {
		case !e.done && m.awaiting && last:
			bullet = th.warn
		case e.err:
			bullet = th.bad
		case e.done:
			bullet = th.ok
		}
		head := bullet.Render(glyphBullet) + " " + boldVerb(th, e.label)
		var body []string
		pw := lipgloss.NewStyle().Width(max(cw-5, 10))
		for _, l := range e.preview {
			st := th.dim
			switch {
			case strings.HasPrefix(l, "+ "):
				st = th.add
			case strings.HasPrefix(l, "- "):
				st = th.del
			}
			body = append(body, st.Render(pw.Render(l)))
		}
		if e.note != "" {
			st := th.dim
			if e.err {
				st = th.bad
			}
			body = append(body, st.Render(pw.Render(e.note)))
		}
		if len(body) == 0 {
			return head
		}
		return head + "\n" + prefixBlock("  "+th.dim.Render(glyphResult)+"  ", "     ", strings.Join(body, "\n"))
	}
	return ""
}

// assistantBody renders a reply as markdown, caching finished messages.
func (m *Model) assistantBody(e *entry, last bool, cw int) string {
	live := last && m.streaming
	if !live && e.rendered != "" && e.renderedWidth == cw {
		return e.rendered
	}
	out := lipgloss.NewStyle().Width(cw).Render(e.text)
	if m.markdown {
		if m.md == nil || m.mdWidth != cw {
			m.md, m.mdWidth = newMarkdown(m.th.light, cw), cw
		}
		if s, ok := renderMarkdown(m.md, e.text); ok {
			out = s
		}
	}
	if !live {
		e.rendered, e.renderedWidth = out, cw
	}
	return out
}

func resultLine(th theme, note string, isErr bool, cw int) string {
	st := th.dim
	if isErr {
		st = th.bad
	}
	return prefixBlock("  "+th.dim.Render(glyphResult)+"  ", "     ", st.Render(lipgloss.NewStyle().Width(max(cw-5, 10)).Render(note)))
}

// boldVerb bolds the "Verb" in "Verb(arg)".
func boldVerb(th theme, label string) string {
	if i := strings.IndexByte(label, '('); i > 0 {
		return th.bold.Render(label[:i]) + th.dim.Render(label[i:])
	}
	return th.bold.Render(label)
}

// prefixBlock prefixes the first line of block with first and the rest with rest.
func prefixBlock(first, rest, block string) string {
	lines := strings.Split(block, "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = first + lines[i]
		} else {
			lines[i] = rest + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// bottom is everything below the transcript: activity line, input or
// approval prompt, and status line.
func (m Model) bottom() string {
	th := m.th
	activity := ""
	if m.streaming {
		verb := "Thinking…"
		if m.rounds > 0 {
			verb = "Working…"
		}
		secs := int(time.Since(m.started).Seconds())
		activity = m.sp.View() + " " + th.accent.Render(verb) + " " +
			th.dim.Render(fmt.Sprintf("(%ds · esc to interrupt)", secs))
	}

	var mid string
	if m.awaiting {
		mid = m.approvalView()
	} else {
		mid = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(th.border).
			Padding(0, 1).Width(max(m.width-2, 10)).Render(m.ta.View())
	}
	return lipgloss.JoinVertical(lipgloss.Left, activity, mid, m.statusLine())
}

func (m Model) approvalView() string {
	th := m.th
	tu := m.pending[0]
	titles := map[string]string{
		"write_file": "Write file", "edit_file": "Edit file", "read_file": "Read file",
		"list_dir": "List directory", "search": "Search",
	}
	title, ok := titles[tu.Name]
	if !ok {
		title = tu.Name
	}
	border := th.accentBorder
	question := "Do you want to proceed?"
	if m.outside {
		border = th.warnBorder
		title += th.warn.Render(" · outside the working directory")
		question = "Allow access outside " + m.tools.Root + "?"
	} else if tu.Name == "edit_file" {
		question = "Do you want to make this edit?"
	} else if tu.Name == "write_file" {
		question = "Do you want to write this file?"
	}

	lines := []string{th.bold.Render(title), th.dim.Render(toolLabel(tu.Name, tu.Input)), "", question}
	for i, o := range m.approvalOptions() {
		line := fmt.Sprintf("%d. %s", i+1, o)
		if i == m.choice {
			lines = append(lines, th.accent.Render(glyphCursor+" "+line))
		} else {
			lines = append(lines, "  "+line)
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(max(m.width-2, 10)).Render(strings.Join(lines, "\n"))
}

func (m Model) statusLine() string {
	th := m.th
	var left string
	switch {
	case m.awaiting:
		left = th.dim.Render("↑/↓ select · enter confirm · esc stop")
	case m.tools != nil && m.auto:
		left = th.warn.Render("⏵⏵ auto-accept edits on") + th.dim.Render(" (ctrl+t to toggle)")
	default:
		left = th.dim.Render("? /help for commands")
	}
	right := th.dim.Render(fmt.Sprintf("%s · %s in · %s out", shortModel(m.modelID), fmtTokens(m.inTok), fmtTokens(m.outTok)))
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if pad < 1 {
		return " " + left
	}
	return " " + left + strings.Repeat(" ", pad) + right
}

// shortModel trims ARN and inference-profile prefixes for display.
func shortModel(id string) string {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.LastIndex(id, "anthropic."); i >= 0 {
		id = id[i+len("anthropic."):]
	}
	return id
}

func fmtTokens(n int32) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
