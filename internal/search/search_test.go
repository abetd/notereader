package search_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/indexer"
	"github.com/abetd/notereader/internal/search"
)

func writeFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (string, *config.Config, *indexer.Index) {
	t.Helper()
	root := t.TempDir()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(root, "a.html"),
		`<html><head><title>全文検索の設計</title></head><body><p>bleve で日本語の全文検索を行う。</p></body></html>`, base)
	writeFile(t, filepath.Join(root, "sub", "b.html"),
		`<html><body><p>fsnotify によるファイル監視の注意点。</p></body></html>`, base.Add(time.Hour))
	writeFile(t, filepath.Join(root, "node_modules", "x.html"), `<p>全文検索</p>`, base)
	writeFile(t, filepath.Join(root, ".next", "y.html"), `<p>全文検索</p>`, base)
	writeFile(t, filepath.Join(root, "c.txt"), `全文検索`, base)
	writeFile(t, filepath.Join(root, ".scratch", "wb.html"), `<p>ホワイトボード</p>`, base.Add(2*time.Hour))
	// a.html より新しく、本文だけが「全文検索」に一致する（タイトル一致の a.html よりスコアは低い）
	writeFile(t, filepath.Join(root, "newer.html"), `<p>全文検索をあとで足したメモ</p>`, base.Add(3*time.Hour))

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
	return root, cfg, idx
}

func ids(res *search.Response) []string {
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.RelPath)
	}
	return out
}

func TestSearch(t *testing.T) {
	_, _, idx := setup(t)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"日本語の部分一致（既定は更新日時順）", "全文検索", []string{"newer.html", "a.html"}},
		{".scratch は対象", "ホワイトボード", []string{".scratch/wb.html"}},
		{"本文中の語", "監視", []string{"sub/b.html"}},
		{"AND 検索", "日本語 bleve", []string{"a.html"}},
		{"AND で絞り込まれ 0 件", "日本語 監視", []string{}},
		{"英字の typo 許容", "fsnotfy", []string{"sub/b.html"}},
		{"英字の前方一致", "fsno", []string{"sub/b.html"}},
		{"ファイル名に一致", "b.html", []string{"sub/b.html"}},
		{"空クエリは新しい順の全件", "", []string{"newer.html", ".scratch/wb.html", "sub/b.html", "a.html"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := search.Do(idx, search.Request{Query: tt.query})
			if err != nil {
				t.Fatal(err)
			}
			got := ids(res)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestHitType(t *testing.T) {
	root, _, idx := setup(t)
	writeFile(t, filepath.Join(root, "memo.md"), "# メモ\n\n日本語の本文\n", time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	if _, err := idx.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := search.Do(idx, search.Request{Query: "日本語"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, h := range res.Hits {
		got[h.RelPath] = h.Type
	}
	if got["memo.md"] != "markdown" || got["a.html"] != "html" {
		t.Errorf("Type = %v", got)
	}
}

func TestSearchSnippetAndSort(t *testing.T) {
	_, _, idx := setup(t)
	res, err := search.Do(idx, search.Request{Query: "日本語", Sort: search.SortMtime})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %v", ids(res))
	}
	h := res.Hits[0]
	if h.Title != "全文検索の設計" || h.URL != "/view/"+h.ID {
		t.Errorf("hit = %+v", h)
	}
	if want := "<mark>日本語</mark>"; !strings.Contains(h.Snippet, want) {
		t.Errorf("snippet = %q", h.Snippet)
	}
}

func TestSortOrder(t *testing.T) {
	_, _, idx := setup(t)
	for _, tt := range []struct {
		name string
		sort search.Sort
		want []string
	}{
		{"既定は更新日時順", "", []string{"newer.html", "a.html"}},
		{"明示的に更新日時順", search.SortMtime, []string{"newer.html", "a.html"}},
		{"スコア順はタイトル一致が先", search.SortScore, []string{"a.html", "newer.html"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res, err := search.Do(idx, search.Request{Query: "全文検索", Sort: tt.sort})
			if err != nil {
				t.Fatal(err)
			}
			got := ids(res)
			if len(got) != len(tt.want) || got[0] != tt.want[0] || got[1] != tt.want[1] {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncDiff(t *testing.T) {
	root, _, idx := setup(t)

	// 変更なし → 読み直さない
	res, err := idx.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 0 || res.Unchanged != 4 || res.Deleted != 0 {
		t.Fatalf("変更なしの同期: %+v", res)
	}

	// 更新・追加・削除
	later := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(root, "a.html"), `<title>差し替え</title><p>kagome の比較</p>`, later)
	writeFile(t, filepath.Join(root, "new", "c.html"), `<p>新しいメモ</p>`, later)
	if err := os.Remove(filepath.Join(root, "sub", "b.html")); err != nil {
		t.Fatal(err)
	}
	res, err = idx.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 2 || res.Unchanged != 2 || res.Deleted != 1 {
		t.Fatalf("差分同期: %+v", res)
	}

	// a.html は差し替えたので消え、newer.html だけが「全文検索」に残る
	for q, want := range map[string]int{"全文検索": 1, "kagome": 1, "新しい": 1, "監視": 0} {
		r, err := search.Do(idx, search.Request{Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Hits) != want {
			t.Errorf("%q: hits = %v, want %d 件", q, ids(r), want)
		}
	}
}

func TestReopenKeepsIndex(t *testing.T) {
	_, cfg, idx := setup(t)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	idx2, err := indexer.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	// 同期前でも検索できる
	res, err := search.Do(idx2, search.Request{Query: "全文検索"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("再オープン後の検索: %v", ids(res))
	}
}

func TestArchive(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(root, "a.html"), `<p>議事録 いま</p>`, base)
	writeFile(t, filepath.Join(root, "archive", "old.html"), `<p>議事録 むかし</p>`, base.Add(time.Hour))
	writeFile(t, filepath.Join(root, "x", "archive", "y", "older.md"), "議事録 もっとむかし\n", base.Add(2*time.Hour))
	writeFile(t, filepath.Join(root, "archived.html"), `<p>議事録 名前が似ているだけ</p>`, base.Add(3*time.Hour))

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

	for _, q := range []string{"", "議事録"} {
		res, err := search.Do(idx, search.Request{Query: q, Archive: cfg})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(ids(res), ","); got != "archived.html,a.html" {
			t.Errorf("q=%q 既定: %s", q, got)
		}
		if res.Total != 2 {
			t.Errorf("q=%q 既定: Total = %d", q, res.Total)
		}

		res, err = search.Do(idx, search.Request{Query: q, Archive: cfg, IncludeArchive: true, Root: "notes"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(ids(res), ","); got != "archived.html,x/archive/y/older.md,archive/old.html,a.html" {
			t.Errorf("q=%q 含める: %s", q, got)
		}
		for _, h := range res.Hits {
			if want := strings.Contains(h.RelPath, "archive/"); h.Archived != want {
				t.Errorf("q=%q %s: Archived = %v", q, h.RelPath, h.Archived)
			}
		}
	}

	// Archive を渡さなければ従来どおり全部を返す
	res, err := search.Do(idx, search.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 {
		t.Errorf("Archive なし: Total = %d", res.Total)
	}

	counts, err := idx.ArchivedRootCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts["notes"] != 2 {
		t.Errorf("ArchivedRootCounts = %v", counts)
	}
}
