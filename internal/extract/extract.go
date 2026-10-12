// Package extract はファイルから検索用のタイトルと本文を取り出す。
// 形式（HTML・Markdown）ごとに Extractor を実装し、拡張子で振り分ける。
package extract

import (
	"io"
	"path/filepath"
	"strings"
)

// Document は抽出結果。
type Document struct {
	Title string
	Body  string
}

// Extractor は 1 形式分の抽出器。
type Extractor interface {
	// Type はドキュメントの形式（"html" など）。インデックスの type フィールドに入る。
	Type() string
	Extract(r io.Reader) (Document, error)
}

var byExt = map[string]Extractor{
	".html":     HTML{},
	".htm":      HTML{},
	".md":       Markdown{},
	".markdown": Markdown{},
}

// For は path の拡張子に対応する Extractor を返す。対象外なら false。
func For(path string) (Extractor, bool) {
	e, ok := byExt[strings.ToLower(filepath.Ext(path))]
	return e, ok
}

// IsMarkdown は path が Markdown として扱われるかを返す。
func IsMarkdown(path string) bool {
	e, ok := For(path)
	if !ok {
		return false
	}
	_, isMD := e.(Markdown)
	return isMD
}

// collapseSpace は連続する空白を 1 つにまとめ、前後を削る。
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
