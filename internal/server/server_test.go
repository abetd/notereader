package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/highlight"
	"github.com/abetd/notereader/internal/indexer"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "dir", "note.html"), `<title>メモ</title><link rel="stylesheet" href="style.css"><p>本文</p>`)
	mustWrite(t, filepath.Join(root, "dir", "style.css"), `body{}`)
	mustWrite(t, filepath.Join(root, ".git", "x.html"), `git`)
	mustWrite(t, filepath.Join(root, "dir", ".env"), `SECRET=1`)
	mustWrite(t, filepath.Join(root, ".scratch", "board.html"), `<p>board</p>`)
	mustWrite(t, filepath.Join(root, ".scratch", "archive", "old.html"), `<p>old board</p>`)
	outside := filepath.Join(t.TempDir(), "outside.html")
	mustWrite(t, outside, `outside`)
	if err := os.Symlink(outside, filepath.Join(root, "link.html")); err != nil {
		t.Fatal(err)
	}

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
	s, err := New(context.Background(), cfg, idx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
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

func get(s *Server, host, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestViewRedirectsToRootScopedURL(t *testing.T) {
	s := newTestServer(t)
	id := indexer.DocID("notes", "dir/note.html")

	rec := get(s, "localhost:8787", "/view/"+id)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/files/notes/dir/note.html" {
		t.Fatalf("Location = %q", loc)
	}

	rec = get(s, "localhost:8787", loc)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "本文") {
		t.Fatalf("note: code = %d body = %q", rec.Code, rec.Body.String())
	}
	// 相対参照の CSS が同じディレクトリから引ける
	rec = get(s, "localhost:8787", "/files/notes/dir/style.css")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("css: code = %d type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	if rec := get(s, "localhost:8787", "/view/0000000000000000"); rec.Code != http.StatusNotFound {
		t.Errorf("存在しない ID: code = %d", rec.Code)
	}
}

func TestFileServingRestrictions(t *testing.T) {
	s := newTestServer(t)
	for _, target := range []string{
		"/files/notes/.git/x.html",   // 既定の除外ディレクトリ
		"/files/notes/dir/.env",      // 秘密情報
		"/files/notes/link.html",     // ルート外への symlink
		"/files/notes/dir",           // ディレクトリ
		"/files/other/dir/note.html", // 未設定のルート
		"/files/notes/../x.html",     // パストラバーサル
	} {
		rec := get(s, "localhost:8787", target)
		if rec.Code == http.StatusOK {
			t.Errorf("%s: 配信されてしまった (%d)", target, rec.Code)
		}
	}
}

func TestServesScratchDir(t *testing.T) {
	s := newTestServer(t)
	if rec := get(s, "localhost:8787", "/files/notes/.scratch/board.html"); rec.Code != http.StatusOK {
		t.Fatalf(".scratch: code = %d", rec.Code)
	}
}

func TestRootScopedPage(t *testing.T) {
	s := newTestServer(t)
	for target, want := range map[string]int{
		"/":        http.StatusOK,
		"/notes":   http.StatusOK,
		"/notes/":  http.StatusOK,
		"/unknown": http.StatusNotFound,
		"/api":     http.StatusNotFound,
	} {
		if rec := get(s, "localhost:8787", target); rec.Code != want {
			t.Errorf("%s: code = %d, want %d", target, rec.Code, want)
		}
	}
	// 画面はどのルートに絞り込んでいるかを body に持つ
	if body := get(s, "localhost:8787", "/notes").Body.String(); !strings.Contains(body, `data-scope="notes"`) {
		t.Error("data-scope が出ていない")
	}
	if body := get(s, "localhost:8787", "/").Body.String(); !strings.Contains(body, `data-scope=""`) {
		t.Error("全体表示で data-scope が空になっていない")
	}
}

func TestSearchRootFilter(t *testing.T) {
	s := newTestServer(t)
	rec := get(s, "localhost:8787", "/api/search?q=&root=notes")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"root":"notes"`) {
		t.Errorf("root=notes: code = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := get(s, "localhost:8787", "/api/search?root=zzz"); rec.Code != http.StatusBadRequest {
		t.Errorf("不明なルート: code = %d", rec.Code)
	}
}

func TestStatusRootCounts(t *testing.T) {
	s := newTestServer(t)
	body := get(s, "localhost:8787", "/api/status").Body.String()
	if !strings.Contains(body, `"name":"notes"`) || !strings.Contains(body, `"documents":3,"archived":1}`) ||
		!strings.Contains(body, `"documents":3,"archived":1,"roots"`) ||
		!strings.Contains(body, fmt.Sprintf(`"pid":%d`, os.Getpid())) {
		t.Errorf("status = %s", body)
	}
}

func TestSearchArchive(t *testing.T) {
	s := newTestServer(t)
	body := get(s, "localhost:8787", "/api/search?q=").Body.String()
	if strings.Contains(body, "archive/old.html") || !strings.Contains(body, `"total":2`) {
		t.Errorf("既定でアーカイブが出る: %s", body)
	}
	body = get(s, "localhost:8787", "/api/search?q=&archive=1").Body.String()
	if !strings.Contains(body, `"rel_path":".scratch/archive/old.html"`) || !strings.Contains(body, `"archived":true`) ||
		!strings.Contains(body, `"total":3`) {
		t.Errorf("archive=1 でアーカイブが出ない: %s", body)
	}
	// 一覧から隠すだけで、ファイルは開ける
	if rec := get(s, "localhost:8787", "/files/notes/.scratch/archive/old.html"); rec.Code != http.StatusOK {
		t.Errorf("アーカイブのファイルが開けない: code = %d", rec.Code)
	}
}

// 1 つの name に複数ディレクトリを紐づけたとき、
// 同じ相対パスのファイルが両方にあっても取り違えないこと
func TestMultiDirRoot(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, ".scratch", "same.html"), `<title>スクラッチ側</title><p>scratch の中身</p>`)
	mustWrite(t, filepath.Join(base, "docs", "same.html"), `<title>ドキュメント側</title><p>docs の中身</p>`)
	mustWrite(t, filepath.Join(base, "docs", "style.css"), `body{}`)
	mustWrite(t, filepath.Join(base, "docs", "drafts", "x.html"), `<p>下書き</p>`)

	// exclude の相対パスはディレクトリから数える（プレフィックスの docs を含めない）
	cfg, err := config.Parse([]byte("data_dir: " + t.TempDir() + "\nexclude: [drafts/*]" +
		"\nroots:\n  - name: my-tools\n    paths:\n      - " + base + "/.scratch\n      - " + base + "/docs\n"))
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
	s, err := New(context.Background(), cfg, idx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// 2 件とも別のドキュメントとして入る（ID は相対パスの先頭にディレクトリ名が付く）
	body := get(s, "localhost:8787", "/api/search?q=&root=my-tools").Body.String()
	for _, want := range []string{`"rel_path":"scratch/same.html"`, `"rel_path":"docs/same.html"`} {
		if !strings.Contains(body, want) {
			t.Errorf("%s が無い: %s", want, body)
		}
	}

	// それぞれの実ファイルが引ける
	for _, tt := range []struct{ path, want string }{
		{"/files/my-tools/scratch/same.html", "scratch の中身"},
		{"/files/my-tools/docs/same.html", "docs の中身"},
	} {
		rec := get(s, "localhost:8787", tt.path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tt.want) {
			t.Errorf("%s: code = %d body = %q", tt.path, rec.Code, rec.Body.String())
		}
	}

	// /view/{id} からも正しい方に飛ぶ
	rec := get(s, "localhost:8787", "/view/"+indexer.DocID("my-tools", "docs/same.html"))
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || loc != "/files/my-tools/docs/same.html" {
		t.Errorf("view: code = %d loc = %q", rec.Code, loc)
	}

	// 索引から外したファイルは配信もしない
	if strings.Contains(body, "drafts/x.html") {
		t.Errorf("除外したファイルが索引にある: %s", body)
	}

	// ディレクトリ名を指定しない・知らない名前・除外したファイルは 404
	for _, target := range []string{"/files/my-tools/same.html", "/files/my-tools/other/same.html", "/files/my-tools/docs/drafts/x.html"} {
		if rec := get(s, "localhost:8787", target); rec.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d", target, rec.Code)
		}
	}

	// status は両方のパスを返す
	st := get(s, "localhost:8787", "/api/status").Body.String()
	if !strings.Contains(st, `"documents":2`) || !strings.Contains(st, ".scratch") || !strings.Contains(st, "docs") {
		t.Errorf("status = %s", st)
	}
}

func TestServeMarkdown(t *testing.T) {
	root := t.TempDir()
	md := "---\ntitle: 設計メモ\n---\n\n# 見出し\n\n本文です。\n\n```go\nfunc Hello() {}\n```\n\n```mermaid\ngraph TD; A-->B;\n```\n"
	mustWrite(t, filepath.Join(root, "note.md"), md)
	mustWrite(t, filepath.Join(root, "plain.md"), "# ただのメモ\n")

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
	s, err := New(context.Background(), cfg, idx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// 変換した HTML が返る
	rec := get(s, "localhost:8787", "/files/notes/note.md")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("code = %d type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{"<title>設計メモ</title>", "<h1", "見出し", `class="chroma"`, `<pre class="mermaid">`, "/static/vendor/mermaid.min.js", "/static/md.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q が無い: %s", want, body)
		}
	}

	// mermaid が無いファイルでは mermaid.js を読み込まない
	if b := get(s, "localhost:8787", "/files/notes/plain.md").Body.String(); strings.Contains(b, "mermaid.min.js") {
		t.Error("mermaid が無いのに読み込んでいる")
	}

	// ?raw=1 なら元のまま返す
	rec = get(s, "localhost:8787", "/files/notes/note.md?raw=1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "```mermaid") {
		t.Errorf("raw: code = %d body = %q", rec.Code, rec.Body.String())
	}

	// 検索でも引ける（タイトルは frontmatter、本文にコードも入る）
	search := get(s, "localhost:8787", "/api/search?q=Hello").Body.String()
	if !strings.Contains(search, `"title":"設計メモ"`) || !strings.Contains(search, `"rel_path":"note.md"`) {
		t.Errorf("search = %s", search)
	}

	// chroma の CSS が配信される
	css := get(s, "localhost:8787", "/static/chroma.css")
	if css.Code != http.StatusOK || !strings.Contains(css.Body.String(), ".chroma") {
		t.Errorf("chroma.css: code = %d", css.Code)
	}
}

func TestServeCode(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "PROJ-1", "main.go"), "package main\n\nfunc Hello() {}\n")
	mustWrite(t, filepath.Join(root, "run.sh"), "echo <hi>\n")
	mustWrite(t, filepath.Join(root, "style.css"), "body{}")
	mustWrite(t, filepath.Join(root, "icon.svg"), `<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	mustWrite(t, filepath.Join(root, "data.bin"), "abc\x00def")
	mustWrite(t, filepath.Join(root, "latin1.txt"), "caf\xe9")
	mustWrite(t, filepath.Join(root, "page.html"), "<p>page</p>")
	// 先頭の判定（8 KB）を超えるもの、色を付ける上限（1 MB）を超えるもの
	mid := "package main\n" + strings.Repeat("// x\n", 4<<10) + "func Mid() {}\n"
	mustWrite(t, filepath.Join(root, "mid.go"), mid)
	big := "package main\n" + strings.Repeat("// <x>\n", highlight.MaxColorSize/7+1) + "func Big() {}\n"
	mustWrite(t, filepath.Join(root, "big.go"), big)

	cfg, err := config.Parse([]byte("data_dir: " + t.TempDir() + "\nroots:\n  - name: notes\n    path: " + root + "\ncode: [go]\n"))
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
	s, err := New(context.Background(), cfg, idx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	fetch := func(target, dest string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "localhost:8787"
		if dest != "" {
			req.Header.Set("Sec-Fetch-Dest", dest)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	isCode := func(rec *httptest.ResponseRecorder) bool {
		return strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") && strings.Contains(rec.Body.String(), `<pre class="chroma">`)
	}

	for name, tt := range map[string]struct {
		target, dest string
		code         bool
	}{
		"新しいタブ":              {"/files/notes/PROJ-1/main.go", "document", true},
		"プレビューの iframe":      {"/files/notes/PROJ-1/main.go", "iframe", true},
		"索引の対象外でもテキストなら":     {"/files/notes/run.sh", "document", true},
		"HTML から読み込まれる CSS":  {"/files/notes/style.css", "style", false},
		"fetch":              {"/files/notes/PROJ-1/main.go", "empty", false},
		"Sec-Fetch-Dest が無い": {"/files/notes/PROJ-1/main.go", "", false},
		"?raw=1":             {"/files/notes/PROJ-1/main.go?raw=1", "document", false},
		"SVG は画像のまま":         {"/files/notes/icon.svg", "document", false},
		"NUL を含む":            {"/files/notes/data.bin", "document", false},
		"UTF-8 でない":          {"/files/notes/latin1.txt", "document", false},
		"HTML はそのまま":         {"/files/notes/page.html", "document", false},
	} {
		rec := fetch(tt.target, tt.dest)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code = %d", name, rec.Code)
			continue
		}
		if got := isCode(rec); got != tt.code {
			t.Errorf("%s: コード表示 = %v, want %v (type = %q)", name, got, tt.code, rec.Header().Get("Content-Type"))
		}
	}

	// コード表示の中身: ディレクトリからのパスがタイトル、色付き、エスケープ済み
	body := fetch("/files/notes/PROJ-1/main.go", "document").Body.String()
	for _, want := range []string{"<title>PROJ-1/main.go</title>", "/static/chroma.css", `<span class="kd">func</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("%q が無い: %s", want, body)
		}
	}
	if b := fetch("/files/notes/run.sh", "document").Body.String(); strings.Contains(b, "<hi>") {
		t.Errorf("エスケープされていない: %s", b)
	}
	// 8 KB を超えるものも最後まで色付きで出す
	if b := fetch("/files/notes/mid.go", "document").Body.String(); !strings.Contains(b, `<span class="nf">Mid</span>`) {
		t.Errorf("mid.go の末尾が色付きで出ていない: %.200s", b[max(0, len(b)-200):])
	}
	// 1 MB を超えるものは色なしで、エスケープして最後まで出す
	b := fetch("/files/notes/big.go", "document").Body.String()
	if strings.Contains(b, `<span class="kd">`) || strings.Contains(b, "<x>") {
		t.Errorf("big.go に色が付いているか、エスケープされていない: %.200s", b)
	}
	if !strings.Contains(b, "<title>big.go</title>") || !strings.HasSuffix(strings.TrimSpace(b), "</html>") ||
		strings.Count(b, "// &lt;x&gt;") != highlight.MaxColorSize/7+1 || !strings.Contains(b, "func Big() {}\n</code></pre>") {
		t.Errorf("big.go のページが欠けている（len = %d）: %.200s", len(b), b[max(0, len(b)-200):])
	}
	// テキストでないファイルは読み位置を戻して元のまま返す
	if b := fetch("/files/notes/data.bin", "document").Body.String(); b != "abc\x00def" {
		t.Errorf("data.bin = %q", b)
	}
	// ヘッダで中身が変わるので、キャッシュが取り違えないようにする
	if v := fetch("/files/notes/style.css", "style").Header().Get("Vary"); v != "Sec-Fetch-Dest" {
		t.Errorf("Vary = %q", v)
	}

	// code に書いた拡張子は検索でも引ける（タイトルはディレクトリからのパス、種別は code）
	search := get(s, "localhost:8787", "/api/search?q=Hello").Body.String()
	if !strings.Contains(search, `"title":"PROJ-1/main.go"`) || !strings.Contains(search, `"type":"code"`) {
		t.Errorf("search = %s", search)
	}
	if search := get(s, "localhost:8787", "/api/search?q=echo").Body.String(); strings.Contains(search, "run.sh") {
		t.Errorf("code に無い拡張子が索引に入っている: %s", search)
	}
}

// paths のルートでも、コードのタイトルはディレクトリからのパス（付いた名前は含めない）
func TestCodeTitleInMultiDirRoot(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "tickets", "PROJ-1", "fix.sql"), "select 1;\n")
	mustWrite(t, filepath.Join(base, "notes", "a.md"), "# a\n")
	cfg, err := config.Parse([]byte("data_dir: " + t.TempDir() + "\ncode: [sql]" +
		"\nroots:\n  - name: work\n    paths:\n      - " + base + "/tickets\n      - " + base + "/notes\n"))
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
	s, err := New(context.Background(), cfg, idx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	search := get(s, "localhost:8787", "/api/search?q=select").Body.String()
	if !strings.Contains(search, `"title":"PROJ-1/fix.sql"`) || !strings.Contains(search, `"rel_path":"tickets/PROJ-1/fix.sql"`) {
		t.Errorf("search = %s", search)
	}

	req := httptest.NewRequest(http.MethodGet, "/files/work/tickets/PROJ-1/fix.sql", nil)
	req.Host = "localhost:8787"
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "<title>PROJ-1/fix.sql</title>") {
		t.Errorf("code page = %s", rec.Body.String())
	}
}

func TestIsText(t *testing.T) {
	for name, tt := range map[string]struct {
		b     string
		whole bool
		want  bool
	}{
		"ASCII":  {"hello", true, true},
		"日本語":    {"こんにちは", true, true},
		"空":      {"", true, true},
		"NUL":    {"a\x00b", true, false},
		"不正なバイト": {"caf\xe9", true, false},
		"途中で切れた文字（途中まで）":   {"ab\xe3\x81", false, true},
		"途中で切れた文字（ファイル全体）": {"ab\xe3\x81", true, false},
	} {
		if got := isText([]byte(tt.b), tt.whole); got != tt.want {
			t.Errorf("%s: isText = %v, want %v", name, got, tt.want)
		}
	}
}

// 画面・Markdown・コード表示のどれも、同じファビコンを参照する
func TestFavicon(t *testing.T) {
	s := newTestServer(t)
	rec := get(s, "localhost:8787", "/static/favicon.svg")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("code = %d type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, name := range []string{"index.html", "markdown.html", "code-head"} {
		var b strings.Builder
		if err := s.tmpl.ExecuteTemplate(&b, name, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(b.String(), `<link rel="icon" href="/static/favicon.svg"`) {
			t.Errorf("%s がファビコンを参照していない", name)
		}
	}
}

func TestHostCheck(t *testing.T) {
	s := newTestServer(t)
	for host, want := range map[string]int{
		"localhost:8787":  http.StatusOK,
		"127.0.0.1:8787":  http.StatusOK,
		"[::1]:8787":      http.StatusOK,
		"evil.example":    http.StatusForbidden,
		"evil.example:80": http.StatusForbidden,
	} {
		if rec := get(s, host, "/api/status"); rec.Code != want {
			t.Errorf("%s: code = %d, want %d", host, rec.Code, want)
		}
	}
}
