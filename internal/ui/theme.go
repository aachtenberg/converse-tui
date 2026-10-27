package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/common-nighthawk/go-figure"

	"github.com/aachtenberg/converse-tui/internal/config"
)

const (
	defaultAccent     = "#d97757"
	defaultBannerText = "converse"
	defaultBannerFont = "standard"

	glyphBullet = "●"
	glyphResult = "⎿"
	glyphCursor = "❯"
)

// spinnerFrames pulse through a few star-like glyphs.
var spinnerFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}

type theme struct {
	accent, dim, faint, bold         lipgloss.Style
	ok, bad, warn, add, del, userMsg lipgloss.Style
	border, accentBorder, warnBorder lipgloss.Color
	light                            bool
}

func newTheme(cfg config.Config) theme {
	accent := cfg.AccentColor
	if accent == "" {
		accent = defaultAccent
	}
	t := theme{light: cfg.Theme == "light"}
	dim, faint, ok, bad, warn := "245", "240", "#4eba65", "#ff6b80", "#ffc107"
	if t.light {
		dim, faint, ok, bad, warn = "242", "250", "#2c7a39", "#ab2b3f", "#966c1e"
	}
	t.accent = lipgloss.NewStyle().Foreground(lipgloss.Color(accent))
	t.dim = lipgloss.NewStyle().Foreground(lipgloss.Color(dim))
	t.faint = lipgloss.NewStyle().Foreground(lipgloss.Color(faint))
	t.bold = lipgloss.NewStyle().Bold(true)
	t.ok = lipgloss.NewStyle().Foreground(lipgloss.Color(ok))
	t.bad = lipgloss.NewStyle().Foreground(lipgloss.Color(bad))
	t.warn = lipgloss.NewStyle().Foreground(lipgloss.Color(warn))
	t.add = t.ok
	t.del = t.bad
	t.userMsg = t.dim
	t.border = lipgloss.Color(faint)
	t.accentBorder = lipgloss.Color(accent)
	t.warnBorder = lipgloss.Color(warn)
	return t
}

// RenderBanner returns the banner art for cfg, or "" when hidden. It fails on
// an unknown font so typos surface at startup.
func RenderBanner(b config.Banner) (art string, err error) {
	if b.Show != nil && !*b.Show {
		return "", nil
	}
	if strings.TrimSpace(b.Art) != "" {
		return trimArt(b.Art), nil
	}
	text, font := b.Text, b.Font
	if text == "" {
		text = defaultBannerText
	}
	if font == "" {
		font = defaultBannerFont
	}
	defer func() {
		if recover() != nil {
			art, err = "", fmt.Errorf("unknown banner font %q (try standard, slant, small, big, doom)", font)
		}
	}()
	return trimArt(figure.NewFigure(text, font, false).String()), nil
}

// trimArt drops blank leading/trailing lines and trailing spaces.
func trimArt(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// newMarkdown builds a glamour renderer with no outer margin so replies line
// up under the message bullet.
func newMarkdown(light bool, width int) *glamour.TermRenderer {
	style := styles.DarkStyleConfig
	if light {
		style = styles.LightStyleConfig
	}
	zero := uint(0)
	doc := style.Document
	doc.Margin = &zero
	doc.BlockPrefix, doc.BlockSuffix = "", ""
	style.Document = doc
	r, err := glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(width))
	if err != nil {
		return nil
	}
	return r
}

func renderMarkdown(r *glamour.TermRenderer, text string) (string, bool) {
	if r == nil {
		return "", false
	}
	out, err := r.Render(text)
	if err != nil {
		return "", false
	}
	lines := strings.Split(out, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n"), true
}
