package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadGuideReturnsBoundedUTF8Markdown(t *testing.T) {
	root := t.TempDir()
	body := "# Widget\n\nUse the circle key.\n"
	if err := os.WriteFile(filepath.Join(root, GuideFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadGuide(root)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("guide = %q", got)
	}
}

func TestReadGuideMissingIsNotAnInstallError(t *testing.T) {
	_, err := ReadGuide(t.TempDir())
	if !errors.Is(err, ErrGuideNotFound) {
		t.Fatalf("missing guide error = %v", err)
	}
}

func TestReadGuideRejectsUnsafeAndUnreadableFiles(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"empty": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(" \n\t"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"invalid utf8": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte{0xff, 0xfe, 0x0a}, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("a", MaxGuideBytes+1)), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, path string) {
			outside := filepath.Join(t.TempDir(), "outside.md")
			if err := os.WriteFile(outside, []byte("# secret\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			prepare(t, filepath.Join(root, GuideFile))
			got, err := ReadGuide(root)
			if err == nil {
				t.Fatalf("accepted unsafe guide: %q", got)
			}
			if errors.Is(err, ErrGuideNotFound) {
				t.Fatalf("unsafe guide reported as missing: %v", err)
			}
		})
	}
}