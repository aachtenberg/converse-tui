package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutside(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, "here", "there"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(root, "escape")); err != nil {
		t.Log("symlinks unavailable:", err)
	}

	for _, p := range []string{"", ".", "here", "./here/there/somewhere", "new/dir/file.txt", "here/../x"} {
		if r.Outside(map[string]any{"path": p}) {
			t.Errorf("%q should be inside", p)
		}
	}
	for _, p := range []string{"..", "../other", base, "escape/x", "here/../../x"} {
		if !r.Outside(map[string]any{"path": p}) {
			t.Errorf("%q should be outside", p)
		}
	}
}

func TestOutsideNeedsApproval(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	os.Mkdir(root, 0o755)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("s"), 0o644)
	r, _ := New(root)

	in := map[string]any{"path": "../secret.txt"}
	if _, err := r.Run("read_file", in, false); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("want outside error, got %v", err)
	}
	if out, err := r.Run("read_file", in, true); err != nil || out != "s" {
		t.Fatalf("approved read: %q %v", out, err)
	}
}

func TestEditFile(t *testing.T) {
	r, _ := New(t.TempDir())
	p := map[string]any{"path": "a/b.txt", "content": "one\ntwo\ntwo\n"}
	if _, err := r.Run("write_file", p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("edit_file", map[string]any{"path": "a/b.txt", "old_text": "two", "new_text": "x"}, false); err == nil {
		t.Fatal("ambiguous edit should fail")
	}
	if _, err := r.Run("edit_file", map[string]any{"path": "a/b.txt", "old_text": "one", "new_text": "1"}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Run("read_file", map[string]any{"path": "a/b.txt"}, false)
	if got != "1\ntwo\ntwo\n" {
		t.Fatalf("got %q", got)
	}
}
