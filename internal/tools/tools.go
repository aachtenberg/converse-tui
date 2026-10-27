// Package tools implements local file tools the model can call. All paths are
// confined to a root directory.
package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxReadBytes   = 256 * 1024
	maxDirEntries  = 500
	previewLines   = 20
	previewLineLen = 200
)

// Tool describes one callable tool.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	// Mutating tools change files and need user approval.
	Mutating bool
	run      func(r *Runner, in map[string]any) (string, error)
}

// Runner executes tools. Paths inside Root (at any depth) are allowed;
// paths outside it are only allowed when the caller passes allowOutside,
// i.e. after the user explicitly approved that call.
type Runner struct {
	Root  string
	tools []Tool

	allowOutside bool // set for the duration of one Run call
}

// New returns a runner confined to root (made absolute).
func New(root string) (*Runner, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("workdir %s is not a directory", abs)
	}
	return &Runner{Root: abs, tools: all}, nil
}

// Tools returns the tool definitions.
func (r *Runner) Tools() []Tool { return r.tools }

// Get looks up a tool by name.
func (r *Runner) Get(name string) (Tool, bool) {
	for _, t := range r.tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Run executes a tool. Errors are meant to be returned to the model.
// allowOutside permits a path outside Root; pass true only after the user
// approved this specific call.
func (r *Runner) Run(name string, in map[string]any, allowOutside bool) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	r.allowOutside = allowOutside
	defer func() { r.allowOutside = false }()
	return t.run(r, in)
}

// Outside reports whether a call's path lies outside Root (including via
// symlinks). Such calls always need explicit approval.
func (r *Runner) Outside(in map[string]any) bool {
	_, outside, err := r.locate(str(in, "path"))
	return err == nil && outside
}

// Summary is a one-line description of a call, e.g. `edit_file main.go`.
func Summary(name string, in map[string]any) string {
	if p := str(in, "path"); p != "" {
		return name + " " + p
	}
	return name
}

// Preview renders what a mutating call will do, as lines prefixed with
// "+ " (added) or "- " (removed).
func Preview(name string, in map[string]any) []string {
	switch name {
	case "write_file":
		return prefixLines("+ ", str(in, "content"))
	case "edit_file":
		return append(prefixLines("- ", str(in, "old_text")), prefixLines("+ ", str(in, "new_text"))...)
	}
	return nil
}

func prefixLines(prefix, s string) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var out []string
	for i, l := range lines {
		if i == previewLines {
			out = append(out, fmt.Sprintf("  … %d more lines", len(lines)-previewLines))
			break
		}
		if len(l) > previewLineLen {
			l = l[:previewLineLen] + "…"
		}
		out = append(out, prefix+l)
	}
	return out
}

var all = []Tool{
	{
		Name:        "list_dir",
		Description: "List files and directories at a path relative to the working directory. Directories end with '/'.",
		Schema: schema(map[string]any{
			"path": prop("Directory path, relative to the working directory. Use '.' for the root."),
		}),
		run: func(r *Runner, in map[string]any) (string, error) {
			p, err := r.resolve(str(in, "path"))
			if err != nil {
				return "", err
			}
			ents, err := os.ReadDir(p)
			if err != nil {
				return "", err
			}
			names := make([]string, 0, len(ents))
			for _, e := range ents {
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) > maxDirEntries {
				names = append(names[:maxDirEntries], fmt.Sprintf("… %d more", len(ents)-maxDirEntries))
			}
			if len(names) == 0 {
				return "(empty directory)", nil
			}
			return strings.Join(names, "\n"), nil
		},
	},
	{
		Name:        "read_file",
		Description: "Read a text file relative to the working directory.",
		Schema:      schema(map[string]any{"path": prop("File path, relative to the working directory.")}, "path"),
		run: func(r *Runner, in map[string]any) (string, error) {
			p, err := r.resolve(str(in, "path"))
			if err != nil {
				return "", err
			}
			fi, err := os.Stat(p)
			if err != nil {
				return "", err
			}
			if fi.IsDir() {
				return "", errors.New("path is a directory; use list_dir")
			}
			if fi.Size() > maxReadBytes {
				return "", fmt.Errorf("file is %d bytes; limit is %d", fi.Size(), maxReadBytes)
			}
			b, err := os.ReadFile(p)
			return string(b), err
		},
	},
	{
		Name:        "write_file",
		Description: "Create or overwrite a file with the given content. Parent directories are created. Prefer edit_file for changes to existing files.",
		Schema: schema(map[string]any{
			"path":    prop("File path, relative to the working directory."),
			"content": prop("Full file content."),
		}, "path", "content"),
		Mutating: true,
		run: func(r *Runner, in map[string]any) (string, error) {
			p, err := r.resolve(str(in, "path"))
			if err != nil {
				return "", err
			}
			content := str(in, "content")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes", len(content)), nil
		},
	},
	{
		Name:        "edit_file",
		Description: "Replace exactly one occurrence of old_text with new_text in a file. old_text must match the file exactly (including whitespace) and be unique; include surrounding lines if needed.",
		Schema: schema(map[string]any{
			"path":     prop("File path, relative to the working directory."),
			"old_text": prop("Exact text to replace; must occur exactly once."),
			"new_text": prop("Replacement text."),
		}, "path", "old_text", "new_text"),
		Mutating: true,
		run: func(r *Runner, in map[string]any) (string, error) {
			p, err := r.resolve(str(in, "path"))
			if err != nil {
				return "", err
			}
			oldText, newText := str(in, "old_text"), str(in, "new_text")
			if oldText == "" {
				return "", errors.New("old_text is empty")
			}
			fi, err := os.Stat(p)
			if err != nil {
				return "", err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			s := string(b)
			switch n := strings.Count(s, oldText); n {
			case 0:
				return "", errors.New("old_text not found in file")
			case 1:
			default:
				return "", fmt.Errorf("old_text occurs %d times; include more context to make it unique", n)
			}
			s = strings.Replace(s, oldText, newText, 1)
			if err := os.WriteFile(p, []byte(s), fi.Mode().Perm()); err != nil {
				return "", err
			}
			return "edit applied", nil
		},
	},
}

// resolve maps a model-supplied path to an absolute path, rejecting paths
// outside Root unless the current call was approved for that.
func (r *Runner) resolve(p string) (string, error) {
	abs, outside, err := r.locate(p)
	if err != nil {
		return "", err
	}
	if outside && !r.allowOutside {
		return "", fmt.Errorf("path %q is outside the working directory and was not approved", p)
	}
	return abs, nil
}

// locate returns the absolute path for p and whether it lies outside Root,
// either lexically or after following symlinks.
func (r *Runner) locate(p string) (string, bool, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Root, p)
	}
	p = filepath.Clean(p)
	if !within(r.Root, p) {
		return p, true, nil
	}
	rootReal, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		return "", false, err
	}
	// Follow symlinks on the longest existing prefix of the path.
	existing, rest := p, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", false, err
	}
	return p, !within(rootReal, filepath.Join(real, rest)), nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func str(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return s
}
