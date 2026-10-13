package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abetd/notereader/internal/config"
)

func newConfig(t *testing.T, root string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("data_dir: " + t.TempDir() + "\nroots:\n  - name: notes\n    path: " + root + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func docCount(t *testing.T, x *Index) uint64 {
	t.Helper()
	n, err := x.DocCount()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// 読めないディレクトリ（ルート自体も含む）の分は、索引から消さない
func TestSyncKeepsUnreadableDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root では権限で読めなくならない")
	}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.html"), "<p>a</p>")
	mustWrite(t, filepath.Join(root, "sub", "b.html"), "<p>b</p>")
	x, err := Open(newConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	if _, err := x.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := docCount(t, x); n != 2 {
		t.Fatalf("DocCount = %d", n)
	}

	for _, dir := range []string{filepath.Join(root, "sub"), root} {
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		res, err := x.Sync(t.Context())
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		if res.Deleted != 0 || docCount(t, x) != 2 {
			t.Errorf("%s が読めないと消える: %+v", dir, res)
		}
	}

	// 読めるようになってから消したものは消える
	if err := os.Remove(filepath.Join(root, "sub", "b.html")); err != nil {
		t.Fatal(err)
	}
	if res, err := x.Sync(t.Context()); err != nil || res.Deleted != 1 {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

// スキーマが同じなら索引をそのまま開き、違えば作り直す
func TestOpenChecksMeta(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.html"), "<p>a</p>")
	cfg := newConfig(t, root)
	x, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	x.Close()

	x, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := docCount(t, x); n != 1 {
		t.Errorf("開き直すと DocCount = %d", n)
	}
	x.Close()

	mustWrite(t, filepath.Join(cfg.DataDir, "meta.json"), `{"schema_version":0}`)
	x, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	if n := docCount(t, x); n != 0 {
		t.Errorf("スキーマが違うのに作り直さない: DocCount = %d", n)
	}
	if err := checkMeta(filepath.Join(cfg.DataDir, "meta.json")); err != nil {
		t.Errorf("作り直した後の meta.json: %v", err)
	}
}

// 設定の code に書いた拡張子だけをコードとして索引に入れ、外せば次の同期で消える
func TestSyncCode(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.html"), "<p>a</p>")
	mustWrite(t, filepath.Join(root, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(root, "run.SH"), "echo hi\n")
	mustWrite(t, filepath.Join(root, "app.log"), "log\n")
	dataDir := t.TempDir()
	parse := func(extra string) *config.Config {
		cfg, err := config.Parse([]byte("data_dir: " + dataDir + "\nroots:\n  - name: notes\n    path: " + root + "\n" + extra))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	x, err := Open(parse("code: [go, .sh]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{"a.html": true, "main.go": true, "run.SH": true, "app.log": false} {
		if _, _, ok, err := x.Lookup(DocID("notes", rel)); err != nil || ok != want {
			t.Errorf("%s: 索引にある = %v, want %v (err = %v)", rel, ok, want, err)
		}
	}
	x.Close()

	x, err = Open(parse(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	if res, err := x.Sync(t.Context()); err != nil || res.Deleted != 2 {
		t.Errorf("code を外しても消えない: res = %+v, err = %v", res, err)
	}
}
