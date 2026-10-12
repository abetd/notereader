// Package indexer は監視対象ディレクトリを走査して bleve インデックスに投入する。
//
// インデックスは再生成可能なキャッシュとして扱う。スキーマ（マッピング・アナライザー）を
// 変えたら SchemaVersion を上げること。起動時に meta.json と比較し、不一致なら作り直す。
package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/abetd/notereader/internal/config"
)

// SchemaVersion はインデックスの構造のバージョン。
// 抽出の仕方を変えたときも上げる（差分同期は署名が同じファイルを抽出し直さないため）。
// 2: HTML の抽出でインライン要素の境目に空白を入れないようにした
const SchemaVersion = 2

// インデックスのフィールド名。
const (
	FieldType     = "type"
	FieldRoot     = "root"
	FieldRelPath  = "rel_path"
	FieldPath     = "path"
	FieldFilename = "filename"
	FieldTitle    = "title"
	FieldBody     = "body"
	FieldSummary  = "summary"
	FieldMtime    = "mtime"
	FieldSig      = "sig"
)

type meta struct {
	SchemaVersion int    `json:"schema_version"`
	Analyzer      string `json:"analyzer"`
}

var currentMeta = meta{SchemaVersion: SchemaVersion, Analyzer: cjk.AnalyzerName}

// Index は bleve インデックスと同期状態をまとめたもの。
type Index struct {
	cfg *config.Config
	idx bleve.Index

	// syncMu は Sync の多重実行と、Sync 中の Close を防ぐ。
	syncMu sync.Mutex
	closed bool

	mu     sync.RWMutex
	status SyncStatus
}

// SyncStatus は直近の同期の状態。
type SyncStatus struct {
	Syncing  bool       `json:"syncing"`
	LastSync *time.Time `json:"last_sync,omitempty"`
	Last     SyncResult `json:"last_result"`
	Error    string     `json:"error,omitempty"`
}

// DocID はルート名とルートからの相対パス（スラッシュ区切り）からファイル ID を作る。
// ルートの物理パスを含めないので、ルートディレクトリを移動しても ID は変わらない。
func DocID(rootName, relPath string) string {
	sum := sha256.Sum256([]byte(rootName + "\x00" + relPath))
	return hex.EncodeToString(sum[:8])
}

// Open は cfg.DataDir 配下のインデックスを開く。無い・スキーマが古い・壊れている場合は作り直す。
// 同じ data_dir のインデックスをほかのプロセスが開いていると、bleve.Open のロックが空くまで待つ。
// meta.json はロックを取ってから比べる（ロックを持たずに比べて作り直すと、動いている側のインデックスを消してしまう）。
func Open(cfg *config.Config) (*Index, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	indexPath := filepath.Join(cfg.DataDir, "index.bleve")
	metaPath := filepath.Join(cfg.DataDir, "meta.json")

	idx, err := bleve.Open(indexPath)
	if err == nil {
		if err = checkMeta(metaPath); err == nil {
			return &Index{cfg: cfg, idx: idx}, nil
		}
		idx.Close()
	}
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		slog.Info("インデックスを新規作成します", "path", indexPath)
	} else {
		slog.Warn("インデックスを作り直します", "path", indexPath, "reason", err)
	}
	if err := os.RemoveAll(indexPath); err != nil {
		return nil, err
	}
	idx, err = bleve.New(indexPath, buildMapping())
	if err != nil {
		return nil, fmt.Errorf("インデックスを作成できません: %w", err)
	}
	if err := writeMeta(metaPath); err != nil {
		idx.Close()
		return nil, err
	}
	return &Index{cfg: cfg, idx: idx}, nil
}

// checkMeta は meta.json が今のスキーマと同じかを確かめる。
func checkMeta(metaPath string) error {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return err
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m != currentMeta {
		return fmt.Errorf("スキーマが変わっています (%+v → %+v)", m, currentMeta)
	}
	return nil
}

// writeMeta は meta.json を書く。途中で止まっても壊れた meta.json が残らないよう、一時ファイルから置き換える。
func writeMeta(path string) error {
	data, err := json.Marshal(currentMeta)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func buildMapping() mapping.IndexMapping {
	text := func() *mapping.FieldMapping {
		fm := bleve.NewTextFieldMapping()
		fm.Analyzer = cjk.AnalyzerName
		fm.Store = true
		// フレーズ検索とハイライトに位置情報が要る
		fm.IncludeTermVectors = true
		return fm
	}
	kw := func() *mapping.FieldMapping {
		fm := bleve.NewKeywordFieldMapping()
		fm.Analyzer = keyword.Name
		fm.Store = true
		fm.IncludeInAll = false
		return fm
	}
	storedOnly := func() *mapping.FieldMapping {
		fm := bleve.NewKeywordFieldMapping()
		fm.Index = false
		fm.Store = true
		fm.IncludeInAll = false
		return fm
	}

	dm := bleve.NewDocumentStaticMapping()
	dm.AddFieldMappingsAt(FieldType, kw())
	dm.AddFieldMappingsAt(FieldRoot, kw())
	dm.AddFieldMappingsAt(FieldRelPath, kw())
	dm.AddFieldMappingsAt(FieldPath, kw())
	dm.AddFieldMappingsAt(FieldFilename, text())
	dm.AddFieldMappingsAt(FieldTitle, text())
	dm.AddFieldMappingsAt(FieldBody, text())
	dm.AddFieldMappingsAt(FieldSummary, storedOnly())
	dm.AddFieldMappingsAt(FieldSig, storedOnly())
	mtime := bleve.NewDateTimeFieldMapping()
	mtime.Store = true
	dm.AddFieldMappingsAt(FieldMtime, mtime)

	im := bleve.NewIndexMapping()
	im.DefaultMapping = dm
	im.DefaultAnalyzer = cjk.AnalyzerName
	return im
}

// Search は bleve の検索をそのまま実行する。
func (x *Index) Search(req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	return x.idx.Search(req)
}

// Lookup は ID からルート名と相対パスを引く。
func (x *Index) Lookup(id string) (root, relPath string, ok bool, err error) {
	req := bleve.NewSearchRequestOptions(bleve.NewDocIDQuery([]string{id}), 1, 0, false)
	req.Fields = []string{FieldRoot, FieldRelPath}
	res, err := x.idx.Search(req)
	if err != nil {
		return "", "", false, err
	}
	if len(res.Hits) == 0 {
		return "", "", false, nil
	}
	f := res.Hits[0].Fields
	root, _ = f[FieldRoot].(string)
	relPath, _ = f[FieldRelPath].(string)
	return root, relPath, root != "" && relPath != "", nil
}

// DocCount はインデックス済みのドキュメント数を返す。
func (x *Index) DocCount() (uint64, error) {
	return x.idx.DocCount()
}

// RootCounts はルートごとのドキュメント数を返す。
func (x *Index) RootCounts() (map[string]int, error) {
	return x.rootCounts(bleve.NewMatchAllQuery())
}

// ArchivedRootCounts はルートごとの、アーカイブ内のドキュメント数を返す（アーカイブが無効なら空）。
func (x *Index) ArchivedRootCounts() (map[string]int, error) {
	expr := x.cfg.ArchiveRegexp()
	if expr == "" {
		return map[string]int{}, nil
	}
	q := bleve.NewRegexpQuery(expr)
	q.SetField(FieldRelPath)
	return x.rootCounts(q)
}

func (x *Index) rootCounts(q query.Query) (map[string]int, error) {
	req := bleve.NewSearchRequestOptions(q, 0, 0, false)
	req.AddFacet("roots", bleve.NewFacetRequest(FieldRoot, 100))
	res, err := x.idx.Search(req)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	if f := res.Facets["roots"]; f != nil {
		for _, t := range f.Terms.Terms() {
			out[t.Term] = t.Count
		}
	}
	return out, nil
}

// Status は直近の同期状態を返す。
func (x *Index) Status() SyncStatus {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.status
}

// Close はインデックスを閉じる。実行中の Sync の完了を待つ。2 回目以降の呼び出しは何もしない。
func (x *Index) Close() error {
	x.syncMu.Lock()
	defer x.syncMu.Unlock()
	if x.closed {
		return nil
	}
	x.closed = true
	return x.idx.Close()
}

// ErrSyncing は同期が既に実行中であることを表す。
var ErrSyncing = errors.New("同期を実行中です")
