package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/extract"
)

const (
	batchSize   = 100
	maxFileSize = 20 << 20 // これより大きいファイルは先頭だけ読む
	summaryLen  = 160
)

// SyncResult は 1 回の同期で何件動いたか。
type SyncResult struct {
	Indexed   int           `json:"indexed"`
	Unchanged int           `json:"unchanged"`
	Deleted   int           `json:"deleted"`
	Failed    int           `json:"failed"`
	Duration  time.Duration `json:"duration_ns"`
}

type entry struct {
	sig  string
	root string
	rel  string
}

// errWrite は索引への書き込みの失敗（ディスク不足など）を表す。走査の失敗と違い、同期そのものを失敗にする。
var errWrite = errors.New("インデックスに書き込めません")

// TrySync は同期が実行中でなければ Sync を実行する。実行中なら ErrSyncing を返す。
func (x *Index) TrySync(ctx context.Context) (SyncResult, error) {
	if !x.syncMu.TryLock() {
		return SyncResult{}, ErrSyncing
	}
	defer x.syncMu.Unlock()
	return x.sync(ctx)
}

// Sync は全ルートを走査し、インデックスをファイルシステムの状態に合わせる。
// 更新日時とサイズが変わっていないファイルは読み直さない。
func (x *Index) Sync(ctx context.Context) (SyncResult, error) {
	x.syncMu.Lock()
	defer x.syncMu.Unlock()
	return x.sync(ctx)
}

func (x *Index) sync(ctx context.Context) (res SyncResult, err error) {
	start := time.Now()
	x.setStatus(func(s *SyncStatus) { s.Syncing = true })
	defer func() {
		res.Duration = time.Since(start)
		x.setStatus(func(s *SyncStatus) {
			s.Syncing = false
			now := time.Now()
			s.LastSync = &now
			s.Last = res
			s.Error = ""
			if err != nil {
				s.Error = err.Error()
			}
		})
	}()

	existing, err := x.entries()
	if err != nil {
		return res, fmt.Errorf("既存エントリを取得できません: %w", err)
	}

	batch := x.idx.NewBatch()
	flush := func() error {
		if batch.Size() == 0 {
			return nil
		}
		if err := x.idx.Batch(batch); err != nil {
			return err
		}
		batch.Reset()
		return nil
	}

	seen := make(map[string]bool, len(existing))
	unreachable := make(map[string]bool) // 走査できなかったルート。配下を消さない
	// skipped は読めずに飛ばしたディレクトリ（ルートごとの、ルートから見た相対パス）。配下を消さない
	skipped := make(map[string][]string)
	for _, root := range x.cfg.Roots {
		for _, dir := range root.Dirs {
			if err := x.syncDir(ctx, root, dir, existing, seen, skipped, &res); err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return res, err
				}
				if errors.Is(err, errWrite) {
					return res, err
				}
				slog.Warn("走査が途中で失敗しました", "root", root.Name, "dir", dir.Path, "err", err)
				unreachable[root.Name] = true
			}
		}
	}

	for id, e := range existing {
		if seen[id] || unreachable[e.root] || underAny(e.rel, skipped[e.root]) {
			continue
		}
		batch.Delete(id)
		res.Deleted++
	}
	if err := flush(); err != nil {
		return res, fmt.Errorf("%w: %w", errWrite, err)
	}
	return res, nil
}

// underAny は rel が dirs のどれかの配下にあるかを返す。
func underAny(rel string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// syncDir は 1 ディレクトリを走査して、変わったファイルだけを batch に積む。
func (x *Index) syncDir(ctx context.Context, root config.Root, dir config.Dir,
	existing map[string]entry, seen map[string]bool, skipped map[string][]string, res *SyncResult,
) error {
	if _, err := os.Stat(dir.Path); err != nil {
		return err
	}
	batch := x.idx.NewBatch()
	flush := func() error {
		if batch.Size() == 0 {
			return nil
		}
		if err := x.idx.Batch(batch); err != nil {
			return fmt.Errorf("%w: %w", errWrite, err)
		}
		batch.Reset()
		return nil
	}
	walkErr := filepath.WalkDir(dir.Path, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// ディレクトリ自体が読めなければ、そのディレクトリの分は走査できなかったものとして扱う（索引を消さない）
			if p == dir.Path {
				return err
			}
			// 権限エラー等はその部分だけ飛ばす。読めなかったディレクトリの配下は索引に残す
			slog.Warn("走査エラー", "path", p, "err", err)
			if d != nil && d.IsDir() {
				if rel, relErr := filepath.Rel(dir.Path, p); relErr == nil {
					skipped[root.Name] = append(skipped[root.Name], dir.Rel(filepath.ToSlash(rel)))
				}
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir.Path, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if x.cfg.Excluded(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// symlink は辿らない（WalkDir もディレクトリの symlink は辿らない）
		if !d.Type().IsRegular() {
			return nil
		}
		ext, ok := extract.For(p)
		if !ok && x.cfg.IsCode(p) {
			ext, ok = extract.Code{}, true
		}
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // 走査の途中で消えたファイルは飛ばす
		}

		// ルート全体から見た相対パス（複数ディレクトリのときは先頭にディレクトリ名が付く）
		rootRel := dir.Rel(rel)
		id := DocID(root.Name, rootRel)
		seen[id] = true
		sig := signature(info)
		if e, ok := existing[id]; ok && e.sig == sig {
			res.Unchanged++
			return nil
		}
		doc, err := buildDoc(root, rootRel, rel, p, info, ext)
		if err != nil {
			// 読めなかったファイルは既存のエントリを残す
			slog.Warn("抽出に失敗しました", "path", p, "err", err)
			res.Failed++
			return nil
		}
		if err := batch.Index(id, doc); err != nil {
			return fmt.Errorf("%w: %w", errWrite, err)
		}
		res.Indexed++
		if batch.Size() >= batchSize {
			return flush()
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	return flush()
}

// entries はインデックス済みの全ドキュメントの署名を取得する。
func (x *Index) entries() (map[string]entry, error) {
	n, err := x.idx.DocCount()
	if err != nil {
		return nil, err
	}
	out := make(map[string]entry, n)
	if n == 0 {
		return out, nil
	}
	req := bleve.NewSearchRequestOptions(bleve.NewMatchAllQuery(), int(n), 0, false)
	req.Fields = []string{FieldSig, FieldRoot, FieldRelPath}
	res, err := x.idx.Search(req)
	if err != nil {
		return nil, err
	}
	for _, h := range res.Hits {
		sig, _ := h.Fields[FieldSig].(string)
		root, _ := h.Fields[FieldRoot].(string)
		rel, _ := h.Fields[FieldRelPath].(string)
		out[h.ID] = entry{sig: sig, root: root, rel: rel}
	}
	return out, nil
}

// signature は変更検知に使う値。更新日時（ns）とサイズの組。
func signature(info fs.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// rootRel はルート全体から見た相対パス、dirRel はディレクトリ（path / paths の 1 件）からの相対パス。
func buildDoc(root config.Root, rootRel, dirRel, path string, info fs.FileInfo, ext extract.Extractor) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	d, err := ext.Extract(io.LimitReader(f, maxFileSize))
	if err != nil {
		return nil, err
	}
	filename := filepath.Base(path)
	title := d.Title
	if _, isCode := ext.(extract.Code); isCode {
		// コードはディレクトリからのパス（チケットのディレクトリに置いたコードなどを、どこにあるかで見分けられるように）
		title = dirRel
	} else if title == "" {
		title = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	return map[string]any{
		FieldType:     ext.Type(),
		FieldRoot:     root.Name,
		FieldRelPath:  rootRel,
		FieldPath:     path,
		FieldFilename: filename,
		FieldTitle:    title,
		FieldBody:     d.Body,
		FieldSummary:  truncate(d.Body, summaryLen),
		FieldMtime:    info.ModTime(),
		FieldSig:      signature(info),
	}, nil
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func (x *Index) setStatus(fn func(*SyncStatus)) {
	x.mu.Lock()
	defer x.mu.Unlock()
	fn(&x.status)
}
