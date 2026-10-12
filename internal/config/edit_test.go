package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	src := "# 先頭のコメント\nlisten: 127.0.0.1:8787\n\nroots:\n  - name: a # 行末のコメント\n    path: " + a + "\n\n# exclude の説明\nexclude:\n  - dist\n"

	out, err := AddRoot([]byte(src), "b", b)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"# 先頭のコメント", "# 行末のコメント", "# exclude の説明", "  - name: b\n    path: " + b + "\n", "exclude:\n  - dist"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q が含まれない:\n%s", want, got)
		}
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Roots) != 2 || cfg.Roots[1].Name != "b" {
		t.Errorf("Roots = %+v", cfg.Roots)
	}
}

func TestAddRootErrors(t *testing.T) {
	a := t.TempDir()
	src := []byte("roots:\n  - name: a\n    path: " + a + "\n")
	for name, tc := range map[string]struct{ name, dir string }{
		"name の重複":    {"a", a},
		"存在しないディレクトリ": {"b", filepath.Join(a, "nope")},
		"予約語の name":   {"api", a},
	} {
		if _, err := AddRoot(src, tc.name, tc.dir); err == nil {
			t.Errorf("%s: エラーにならない", name)
		}
	}
}

func TestAddRootWithoutRoots(t *testing.T) {
	a := t.TempDir()
	for _, src := range []string{"", "# コメントだけ\n", "listen: 127.0.0.1:9000\nroots:\n"} {
		out, err := AddRoot([]byte(src), "a", a)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if _, err := Parse(out); err != nil {
			t.Errorf("%q: %v\n%s", src, err, out)
		}
	}
}

// config init の直後（雛形のまま）でも add でき、雛形のコメントが残ること
func TestAddRootToExample(t *testing.T) {
	src := Example
	if _, err := Parse(src); err == nil {
		t.Error("roots が空の雛形が検証を通る")
	}
	a := t.TempDir()
	out, err := AddRoot(src, "notes", a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#   - name: boards", "# 追加の除外パターン", "# アーカイブとみなすディレクトリ", "#   - notes/old", "roots:\n  - name: notes\n    path: " + a + "\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("%q が含まれない:\n%s", want, out)
		}
	}
}

func TestAddRootContractsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	dir, err := os.MkdirTemp(home, ".notereader-test-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	out, err := AddRoot(nil, "a", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "path: ~/" + filepath.Base(dir); !strings.Contains(string(out), want) {
		t.Errorf("%q が含まれない:\n%s", want, out)
	}
}

func TestRemoveRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	src := "# 先頭のコメント\nroots:\n  - name: a\n    path: " + a + "\n  - name: b\n    paths:\n      - " + b + "\n"

	out, err := RemoveRoot([]byte(src), "b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "name: b") || !strings.Contains(string(out), "# 先頭のコメント") {
		t.Errorf("結果:\n%s", out)
	}

	if _, err := RemoveRoot([]byte(src), "nope"); err == nil {
		t.Error("無い name でエラーにならない")
	}
	// 最後の 1 つを消すと roots が空になり、検証で弾かれる
	if _, err := RemoveRoot(out, "a"); err == nil {
		t.Error("roots が空になってもエラーにならない")
	}
}

func TestLoadRuntime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// roots が壊れていても読める
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:9999\ndata_dir: "+dir+"\nroots:\n  - name: x\n    path: "+dir+"/nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Listen != "127.0.0.1:9999" || rt.DataDir != dir {
		t.Errorf("Runtime = %+v", rt)
	}
}
