// Package server は検索画面・検索 API・HTML 配信の HTTP ハンドラを提供する。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/extract"
	"github.com/abetd/notereader/internal/highlight"
	"github.com/abetd/notereader/internal/indexer"
	"github.com/abetd/notereader/internal/markdown"
	"github.com/abetd/notereader/internal/search"
	"github.com/abetd/notereader/web"
)

// Server は HTTP ハンドラ一式。
type Server struct {
	cfg   *config.Config
	index *indexer.Index
	// roots はルート名 → （ディレクトリのプレフィックス → 開いたディレクトリ）。
	// 単一ディレクトリのルートではプレフィックスは空文字。
	roots map[string]map[string]*os.Root
	tmpl  *template.Template
	// chromaCSS はコードハイライトの配色（起動時に組み立てる）
	chromaCSS string
	mux       *http.ServeMux
	// syncCtx は手動 reindex を実行するときの親コンテキスト（終了時にキャンセルされる）
	syncCtx context.Context
}

// New はルートディレクトリを開き、ルーティングを組み立てる。
func New(ctx context.Context, cfg *config.Config, index *indexer.Index) (*Server, error) {
	roots := make(map[string]map[string]*os.Root, len(cfg.Roots))
	for _, r := range cfg.Roots {
		dirs := make(map[string]*os.Root, len(r.Dirs))
		for _, d := range r.Dirs {
			// os.Root 経由で開くと、ルートの外へ出るパスや symlink は拒否される（ルートの中を指す symlink は辿る）
			root, err := os.OpenRoot(d.Path)
			if err != nil {
				return nil, err
			}
			dirs[d.Prefix] = root
		}
		roots[r.Name] = dirs
	}
	tmpl, err := template.ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	chromaCSS, err := highlight.StyleCSS()
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, index: index, roots: roots, tmpl: tmpl, mux: http.NewServeMux(), syncCtx: ctx, chromaCSS: chromaCSS}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /api/search", s.handleSearch)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("POST /api/reindex", s.handleReindex)
	s.mux.HandleFunc("GET /view/{id}", s.handleView)
	s.mux.HandleFunc("GET /files/{root}/{path...}", s.handleFile)
	// ルート単位の画面。設定にある名前だけを登録する
	// （ワイルドカードにすると /static/ などのパターンと衝突する）
	for _, r := range cfg.Roots {
		s.mux.HandleFunc("GET /"+r.Name, s.handleIndex)
		s.mux.HandleFunc("GET /"+r.Name+"/{$}", s.handleIndex)
	}
	s.mux.HandleFunc("GET /static/chroma.css", s.handleChromaCSS)
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	return s, nil
}

// Close は開いているルートを閉じる。
func (s *Server) Close() {
	for _, dirs := range s.roots {
		for _, r := range dirs {
			r.Close()
		}
	}
}

// ServeHTTP は Host ヘッダを検証してからルーティングする。
// 外部サイトからの DNS リバインディングでローカルのファイルを読まれないようにするため。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowedHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func allowedHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// /{name} で開かれたときは、その名前が絞り込み対象になる
	scope := strings.Trim(r.URL.Path, "/")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", struct{ Scope string }{scope}); err != nil {
		slog.Error("テンプレートの描画に失敗しました", "err", err)
	}
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	sort := search.Sort(q.Get("sort"))
	if sort != search.SortScore {
		sort = search.DefaultSort
	}
	root := q.Get("root")
	if root != "" {
		if _, ok := s.roots[root]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不明なルート: " + root})
			return
		}
	}
	res, err := search.Do(s.index, search.Request{
		Query: q.Get("q"), Sort: sort, Limit: limit, Root: root,
		Archive: s.cfg, IncludeArchive: q.Get("archive") == "1",
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type rootStatus struct {
	Name      string   `json:"name"`
	Paths     []string `json:"paths"`
	Documents int      `json:"documents"`
	// Archived は Documents のうちアーカイブ内のもの。
	Archived int `json:"archived"`
}

type statusResponse struct {
	// PID は notereader start が、応答したのが自分の起こしたプロセスかを確かめるのに使う。
	PID       int    `json:"pid"`
	Documents uint64 `json:"documents"`
	// Archived は Documents のうちアーカイブ内のもの（全ルートの合計）。
	Archived int                `json:"archived"`
	Roots    []rootStatus       `json:"roots"`
	Sync     indexer.SyncStatus `json:"sync"`
	Watching bool               `json:"watching"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	n, err := s.index.DocCount()
	if err != nil {
		writeError(w, err)
		return
	}
	counts, err := s.index.RootCounts()
	if err != nil {
		writeError(w, err)
		return
	}
	archived, err := s.index.ArchivedRootCounts()
	if err != nil {
		writeError(w, err)
		return
	}
	roots := make([]rootStatus, 0, len(s.cfg.Roots))
	total := 0
	for _, r := range s.cfg.Roots {
		paths := make([]string, 0, len(r.Dirs))
		for _, d := range r.Dirs {
			paths = append(paths, d.Path)
		}
		roots = append(roots, rootStatus{Name: r.Name, Paths: paths, Documents: counts[r.Name], Archived: archived[r.Name]})
		total += archived[r.Name]
	}
	writeJSON(w, http.StatusOK, statusResponse{
		PID:       os.Getpid(),
		Documents: n,
		Archived:  total,
		Roots:     roots,
		Sync:      s.index.Status(),
		Watching:  s.cfg.WatchEnabled(),
	})
}

func (s *Server) handleReindex(w http.ResponseWriter, _ *http.Request) {
	if s.index.Status().Syncing {
		writeJSON(w, http.StatusConflict, map[string]string{"status": "already syncing"})
		return
	}
	go func() {
		res, err := s.index.TrySync(s.syncCtx)
		if errors.Is(err, indexer.ErrSyncing) {
			// 確認の後に別の同期が始まった。その同期が取り込むので、ここでは何もしない
			slog.Info("同期中のため再インデックスは走らせませんでした")
			return
		}
		if err != nil {
			slog.Error("再インデックスに失敗しました", "err", err)
			return
		}
		slog.Info("再インデックス完了", "result", res)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleView は ID を実ファイルの URL にリダイレクトする。
// ルート単位の URL に移ることで、HTML 内の相対参照（CSS・画像）がブラウザ側でそのまま解決される。
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	root, rel, ok, err := s.index.Lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	u := "/files/" + url.PathEscape(root) + "/" + escapePath(rel)
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	dirs, ok := s.roots[r.PathValue("root")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	rel := r.PathValue("path")
	if !fs.ValidPath(rel) {
		http.NotFound(w, r)
		return
	}
	// 複数ディレクトリのルートでは、先頭の要素がどのディレクトリかを表す
	dir, ok := dirs[""]
	if !ok {
		prefix, rest, found := strings.Cut(rel, "/")
		if !found {
			http.NotFound(w, r)
			return
		}
		if dir, ok = dirs[prefix]; !ok {
			http.NotFound(w, r)
			return
		}
		rel = rest
	}
	// 索引・監視と同じく、ディレクトリからの相対パス（プレフィックスを外した後）で判定する
	if s.cfg.ExcludedPath(rel) {
		http.NotFound(w, r)
		return
	}
	f, err := dir.Open(rel)
	if err != nil {
		notFound(w, r, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		notFound(w, r, err)
		return
	}
	if info.IsDir() {
		http.NotFound(w, r)
		return
	}
	raw := r.URL.Query().Get("raw") != ""
	// Markdown は HTML に変換して返す（?raw=1 なら元のまま）
	if extract.IsMarkdown(rel) && !raw {
		s.serveMarkdown(w, f, rel)
		return
	}
	// それ以外のテキストファイルは、ページとして開かれたときだけコード表示にする（?raw=1 なら元のまま）
	if codeCandidate(rel) {
		w.Header().Add("Vary", "Sec-Fetch-Dest")
		if !raw && openedAsPage(r) {
			served, err := s.serveCode(w, r, f, rel)
			if err != nil {
				writeError(w, err)
				return
			}
			if served {
				return
			}
		}
	}
	http.ServeContent(w, r, path.Base(rel), info.ModTime(), f)
}

// codeCandidate はコード表示にしうるファイルかを返す。
// HTML / Markdown と、拡張子から見て image/* のものは除く（中身がテキストの SVG を画像のまま返すため）。
func codeCandidate(rel string) bool {
	if _, ok := extract.For(rel); ok {
		return false
	}
	return !strings.HasPrefix(mime.TypeByExtension(path.Ext(rel)), "image/")
}

// openedAsPage は、ページとして（新しいタブ・プレビューの iframe で）開かれたかを返す。
// HTML から読み込まれる CSS・JS・fetch と、Sec-Fetch-Dest を付けないクライアント（curl など）では false。
// 元のまま返して、HTML の読み込みを壊さないため。
func openedAsPage(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe":
		return true
	}
	return false
}

// sniffLen はテキストかどうかを判定するために読む先頭の長さ。
const sniffLen = 8 << 10

// serveCode は中身がテキストならコード表示を返して true を返す。
// テキストでなければ何も書かずに読み位置を先頭に戻し、false を返す（呼び出し側が元のまま返す）。
// 本文はページの前後の間に流し込む（大きなファイルをまるごとメモリに載せないため）。
// 書き始めた後の失敗は 500 にできないので、ログに残してそこで終える。
func (s *Server) serveCode(w http.ResponseWriter, r *http.Request, f *os.File, rel string) (bool, error) {
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	head = head[:n]
	if !isText(head, n < sniffLen) {
		_, err := f.Seek(0, io.SeekStart)
		return false, err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// タイトルは索引と同じく、ディレクトリからのパス（rel は複数ディレクトリのルートのプレフィックスを外した後）
	if err := s.tmpl.ExecuteTemplate(w, "code-head", codePage{Title: rel}); err != nil {
		logCodeError(r, rel, "head", err)
		return true, nil
	}
	if err := highlight.File(w, path.Base(rel), io.MultiReader(bytes.NewReader(head), f)); err != nil {
		logCodeError(r, rel, "body", err)
		return true, nil
	}
	if err := s.tmpl.ExecuteTemplate(w, "code-foot", nil); err != nil {
		logCodeError(r, rel, "foot", err)
	}
	return true, nil
}

// logCodeError はコード表示の途中の失敗をログに残す。stage はどこまで書いたか（head・body・foot）。
// ブラウザが接続を切った（プレビューで別のファイルに移った など）ための書き込みの失敗は、よくあることなので残さない。
func logCodeError(r *http.Request, rel, stage string, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.Error("コードの描画に失敗しました", "path", rel, "stage", stage, "err", err)
}

type codePage struct {
	Title string
}

// isText は先頭 b がテキストか（NUL を含まず、UTF-8 として読めるか）を返す。
// whole が false（ファイルの途中まで）のときは、末尾で切れた文字を見ない。
func isText(b []byte, whole bool) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if !whole {
		for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
			if utf8.RuneStart(b[i]) {
				if !utf8.FullRune(b[i:]) {
					b = b[:i]
				}
				break
			}
		}
	}
	return utf8.Valid(b)
}

type markdownPage struct {
	Title      string
	Body       template.HTML
	HasMermaid bool
}

func (s *Server) serveMarkdown(w http.ResponseWriter, f io.Reader, rel string) {
	src, err := io.ReadAll(f)
	if err != nil {
		writeError(w, err)
		return
	}
	doc, err := markdown.Render(src)
	if err != nil {
		writeError(w, err)
		return
	}
	title := doc.Title
	if title == "" {
		title = path.Base(rel)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := markdownPage{Title: title, Body: template.HTML(doc.HTML), HasMermaid: doc.HasMermaid} //nolint:gosec // 自分のローカルファイルを描画する
	if err := s.tmpl.ExecuteTemplate(w, "markdown.html", page); err != nil {
		slog.Error("Markdown の描画に失敗しました", "path", rel, "err", err)
	}
}

func (s *Server) handleChromaCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	if _, err := io.WriteString(w, s.chromaCSS); err != nil {
		slog.Error("CSS の書き込みに失敗しました", "err", err)
	}
}

// notFound は開けなかったファイルを 404 にする。無いファイル以外（権限・ルート外への symlink・I/O など）は理由をログに残す。
// 無いファイルは HTML 内の相対参照でもよく起きるので、ログに出さない。
func notFound(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("ファイルを開けません", "path", r.URL.Path, "err", err)
	}
	http.NotFound(w, r)
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("JSON の書き込みに失敗しました", "err", err)
	}
}

// writeError はエラーを記録して 500 を返す。
func writeError(w http.ResponseWriter, err error) {
	slog.Error("リクエスト処理に失敗しました", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// Run は addr で待ち受け、ctx がキャンセルされたら停止する。
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
