package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Parse([]byte("roots:\n  - name: notes\n    path: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if !strings.HasSuffix(cfg.DataDir, "notereader") {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
}

func TestParseErrors(t *testing.T) {
	dir := t.TempDir()
	for name, src := range map[string]string{
		"roots が空":              "roots: []\n",
		"name が無い":              "roots:\n  - path: " + dir + "\n",
		"name に不正な文字":           "roots:\n  - name: my notes\n    path: " + dir + "\n",
		"name の重複":              "roots:\n  - name: a\n    path: " + dir + "\n  - name: a\n    path: " + dir + "\n",
		"存在しないパス":               "roots:\n  - name: a\n    path: " + dir + "/nope\n",
		"予約語の name":             "roots:\n  - name: api\n    path: " + dir + "\n",
		"不正な exclude":           "roots:\n  - name: a\n    path: " + dir + "\nexclude: ['[']\n",
		"負の reconcile_interval": "roots:\n  - name: a\n    path: " + dir + "\nreconcile_interval: -5m\n",
		"ループバック以外の listen":      "listen: 0.0.0.0:8787\nroots:\n  - name: a\n    path: " + dir + "\n",
		"ホストの無い listen":         "listen: :8787\nroots:\n  - name: a\n    path: " + dir + "\n",
		"別のルートに同じパス":            "roots:\n  - name: a\n    path: " + dir + "\n  - name: b\n    path: " + dir + "\n",
		"code に Markdown":       "roots:\n  - name: a\n    path: " + dir + "\ncode: [.md]\n",
		"code にパス":              "roots:\n  - name: a\n    path: " + dir + "\ncode: [Makefile.d/x]\n",
		"code に二重の拡張子":          "roots:\n  - name: a\n    path: " + dir + "\ncode: [.d.ts]\n",
		"code に空":               "roots:\n  - name: a\n    path: " + dir + "\ncode: ['']\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: エラーにならない", name)
		}
	}
}

func TestCode(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Parse([]byte("roots:\n  - name: a\n    path: " + dir + "\ncode: [.go, sh, .YAML, go]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".go", ".sh", ".yaml"}; !slices.Equal(cfg.Code, want) {
		t.Errorf("Code = %q, want %q", cfg.Code, want)
	}
	for path, want := range map[string]bool{
		"main.go":         true,
		"dir/run.SH":      true,
		"conf.yaml":       true,
		"conf.yml":        false,
		"Makefile":        false,
		"note.md":         false,
		"dir.go/readme":   false,
		"archive/main.go": true,
	} {
		if got := cfg.IsCode(path); got != want {
			t.Errorf("IsCode(%q) = %v, want %v", path, got, want)
		}
	}

	// 省略時は空（HTML / Markdown だけ）
	cfg, err = Parse([]byte("roots:\n  - name: a\n    path: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Code) != 0 || cfg.IsCode("main.go") {
		t.Errorf("Code = %q", cfg.Code)
	}
}

func TestMultiplePaths(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{".scratch", "docs"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Parse([]byte("roots:\n  - name: my-tools\n    paths:\n      - " + base + "/.scratch\n      - " + base + "/docs\n"))
	if err != nil {
		t.Fatal(err)
	}
	dirs := cfg.Roots[0].Dirs
	if len(dirs) != 2 {
		t.Fatalf("Dirs = %+v", dirs)
	}
	// 先頭のドットは落としてプレフィックスにする
	if dirs[0].Prefix != "scratch" || dirs[1].Prefix != "docs" {
		t.Errorf("Prefix = %q, %q", dirs[0].Prefix, dirs[1].Prefix)
	}
	if got := dirs[0].Rel("board.html"); got != "scratch/board.html" {
		t.Errorf("Rel = %q", got)
	}
	if d, ok := cfg.Roots[0].DirByPrefix("docs"); !ok || d.Path != filepath.Join(base, "docs") {
		t.Errorf("DirByPrefix = %+v, %v", d, ok)
	}
}

func TestLoopbackListen(t *testing.T) {
	dir := t.TempDir()
	for _, listen := range []string{"127.0.0.1:9000", "localhost:9000", "[::1]:9000"} {
		if _, err := Parse([]byte("listen: \"" + listen + "\"\nroots:\n  - name: a\n    path: " + dir + "\n")); err != nil {
			t.Errorf("%s: %v", listen, err)
		}
	}
}

func TestSinglePathKeepsNoPrefix(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Parse([]byte("roots:\n  - name: notes\n    path: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Roots[0].Dirs
	if len(d) != 1 || d[0].Prefix != "" || d[0].Path != dir {
		t.Errorf("Dirs = %+v", d)
	}
	// paths で 1 つだけ書いた場合も同じ扱い
	cfg, err = Parse([]byte("roots:\n  - name: notes\n    paths:\n      - " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Roots[0].Dirs; len(d) != 1 || d[0].Prefix != "" {
		t.Errorf("Dirs = %+v", d)
	}
}

func TestMultiplePathsErrors(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"a/scratch", "b/scratch", "a/scratch/inner"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, src := range map[string]string{
		"path と paths の併用": "roots:\n  - name: x\n    path: " + base + "\n    paths:\n      - " + base + "/a\n",
		"どちらも無い":           "roots:\n  - name: x\n",
		"入れ子":              "roots:\n  - name: x\n    paths:\n      - " + base + "/a/scratch\n      - " + base + "/a/scratch/inner\n",
		"存在しない":            "roots:\n  - name: x\n    paths:\n      - " + base + "/nope\n",
		"同じパス":             "roots:\n  - name: x\n    paths:\n      - " + base + "/a\n      - " + base + "/a/\n",
		"同じパスをラベル違いで":      "roots:\n  - name: x\n    paths:\n      - {path: " + base + "/a, label: p}\n      - {path: " + base + "/a, label: q}\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: エラーにならない", name)
		}
	}
}

func TestPrefixFallsBackToParent(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"my-tools/.scratch", "dotfiles/.scratch", "other/.scratch"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Parse([]byte("roots:\n  - name: boards\n    paths:\n      - " + base + "/my-tools/.scratch\n      - " +
		base + "/dotfiles/.scratch\n      - {path: " + base + "/other/.scratch, label: zzz}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, d := range cfg.Roots[0].Dirs {
		got = append(got, d.Prefix)
	}
	// 末尾が同じ .scratch なので親まで遡って区別する。label 指定はそのまま使う
	want := []string{"my-tools-scratch", "dotfiles-scratch", "zzz"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Prefix = %v, want %v", got, want)
		}
	}
}

func TestDuplicateLabel(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := "roots:\n  - name: x\n    paths:\n      - {path: " + base + "/a, label: same}\n      - {path: " + base + "/b, label: same}\n"
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("label 重複がエラーにならない")
	}
}

func TestExcluded(t *testing.T) {
	cfg := &Config{Exclude: []string{"dist", "*.bak.html", "archive/old"}}
	for rel, want := range map[string]bool{
		"notes":          false,
		"a/.git":         true,
		"a/node_modules": true,
		".scratch":       false,
		"a/.env":         true,
		"a/.env.local":   true,
		"a/.next":        true,
		"a/dist":         true,
		"a/b.bak.html":   true,
		"archive/old":    true,
		"archive/new":    false,
		"a/distribution": false,
	} {
		if got := cfg.Excluded(rel); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestExcludedPath(t *testing.T) {
	cfg := &Config{}
	for rel, want := range map[string]bool{
		".scratch/board.html":     false,
		"a/.git/x.html":           true,
		"node_modules/pkg/a.html": true,
		"a/b/.env":                true,
		"a/b/c.html":              false,
	} {
		if got := cfg.ExcludedPath(rel); got != want {
			t.Errorf("ExcludedPath(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestArchived(t *testing.T) {
	tests := []struct {
		name    string
		archive string
		want    map[string]bool
	}{
		{"省略時は archive", "", map[string]bool{
			"archive/a.html":        true,
			"x/y/archive/a.md":      true,
			"archive/sub/a.html":    true,
			"a.html":                false,
			"archive.html":          false,
			"archived/a.html":       false,
			"x/myarchive/a.html":    false,
			"x/archive":             false, // ファイル名には当てない
			"app-scratch/archive/a": true,  // 複数ディレクトリのルート
		}},
		{"空のリストで無効", "archive: []\n", map[string]bool{
			"archive/a.html": false,
		}},
		{"書くと置き換え", "archive:\n  - old\n  - \"*.bak\"\n", map[string]bool{
			"old/a.html":       true,
			"x/old/a.html":     true,
			"x.bak/a.html":     true,
			"archive/a.html":   false,
			"x/a.bak":          false,
			"x/y.bak.d/a.html": false,
		}},
		{"相対パスのパターン", "archive:\n  - notes/archive\n", map[string]bool{
			"notes/archive/a.html":   true,
			"notes/archive/b/c.html": true,
			"x/notes/archive/a.html": false,
			"archive/a.html":         false,
		}},
		{"文字クラスと打ち消し", "archive:\n  - \"[ab]rc\"\n  - \"x[^0-9]\"\n  - \"\\\\d\"\n", map[string]bool{
			"arc/a.html": true,
			"brc/a.html": true,
			"crc/a.html": false,
			"xa/a.html":  true,
			"x1/a.html":  false,
			"d/a.html":   true,
			"5/a.html":   false,
		}},
	}
	dir := t.TempDir()
	for _, tc := range tests {
		cfg, err := Parse([]byte("roots:\n  - name: a\n    path: " + dir + "\n" + tc.archive))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		re := regexp.MustCompile("^(?:" + cfg.ArchiveRegexp() + ")$")
		for rel, want := range tc.want {
			if got := cfg.Archived(rel); got != want {
				t.Errorf("%s: Archived(%q) = %v, want %v", tc.name, rel, got, want)
			}
			if cfg.ArchiveRegexp() != "" && re.MatchString(rel) != want {
				t.Errorf("%s: ArchiveRegexp が %q で Archived と食い違う", tc.name, rel)
			}
		}
	}
}

func TestArchiveDefaultsWithoutParse(t *testing.T) {
	if !(&Config{}).Archived("archive/a.html") {
		t.Error("Parse を通さない Config で既定の archive が効かない")
	}
}

func TestArchiveErrors(t *testing.T) {
	dir := t.TempDir()
	for _, pat := range []string{`"[a"`, `"a\\"`} {
		if _, err := Parse([]byte("roots:\n  - name: a\n    path: " + dir + "\narchive:\n  - " + pat + "\n")); err == nil {
			t.Errorf("archive %s がエラーにならない", pat)
		}
	}
}
