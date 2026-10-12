package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/indexer"
	"github.com/abetd/notereader/internal/search"
	"github.com/abetd/notereader/internal/watcher"
)

func setup(t *testing.T) (string, *indexer.Index) {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "first.html"), `<p>最初のメモ</p>`)

	cfg, err := config.Parse([]byte("data_dir: " + t.TempDir() + "\nroots:\n  - name: notes\n    path: " + root + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := indexer.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	if _, err := idx.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	w, err := watcher.New(cfg, idx)
	if err != nil {
		t.Fatal(err)
	}
	w.Debounce = 20 * time.Millisecond
	w.Interval = 0 // 定期照合に助けられていないことを確かめるため切る
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	// 監視の登録が終わるまで少し待つ
	time.Sleep(100 * time.Millisecond)
	return root, idx
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitHits は query の件数が want になるまで待つ。
func waitHits(t *testing.T, idx *indexer.Index, query string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		res, err := search.Do(idx, search.Request{Query: query})
		if err != nil {
			t.Fatal(err)
		}
		got = len(res.Hits)
		if got == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%q: hits = %d, want %d", query, got, want)
}

func TestWatchAddUpdateDelete(t *testing.T) {
	root, idx := setup(t)

	// 追加
	write(t, filepath.Join(root, "added.html"), `<title>あとから足したメモ</title><p>本文</p>`)
	waitHits(t, idx, "あとから足した", 1)

	// 更新
	write(t, filepath.Join(root, "added.html"), `<title>書き換えたメモ</title><p>本文</p>`)
	waitHits(t, idx, "書き換えた", 1)
	waitHits(t, idx, "あとから足した", 0)

	// 削除
	if err := os.Remove(filepath.Join(root, "added.html")); err != nil {
		t.Fatal(err)
	}
	waitHits(t, idx, "書き換えた", 0)
}

func TestWatchNewSubdirectory(t *testing.T) {
	root, idx := setup(t)

	// 起動後に作られたディレクトリの中身も拾う
	write(t, filepath.Join(root, "sub", "deep.html"), `<p>新しい階層のメモ</p>`)
	waitHits(t, idx, "新しい階層", 1)

	// さらにその下に作られたディレクトリも監視対象になる
	write(t, filepath.Join(root, "sub", "deeper", "more.html"), `<p>さらに深いメモ</p>`)
	waitHits(t, idx, "さらに深い", 1)
}

func TestWatchIgnoresExcludedDir(t *testing.T) {
	root, idx := setup(t)

	write(t, filepath.Join(root, "node_modules", "x.html"), `<p>除外されるメモ</p>`)
	write(t, filepath.Join(root, "ok.html"), `<p>拾われるメモ</p>`)
	waitHits(t, idx, "拾われる", 1)
	waitHits(t, idx, "除外される", 0)
}

func TestReconcileInterval(t *testing.T) {
	// fsnotify のイベントが来なくても、定期照合で追いつくこと
	root := t.TempDir()
	cfg, err := config.Parse([]byte("data_dir: " + t.TempDir() + "\nreconcile_interval: 50ms\nroots:\n  - name: notes\n    path: " + root + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := indexer.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })

	w, err := watcher.New(cfg, idx)
	if err != nil {
		t.Fatal(err)
	}
	if w.Interval != 50*time.Millisecond {
		t.Fatalf("Interval = %v", w.Interval)
	}
	// 監視対象に入らない状態でファイルを置く（イベントは期待しない）
	ctx := t.Context()
	go func() { _ = w.Run(ctx) }()
	write(t, filepath.Join(root, "late.html"), `<p>定期照合で拾うメモ</p>`)
	waitHits(t, idx, "定期照合で拾う", 1)
}
