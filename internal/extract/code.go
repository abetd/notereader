package extract

import (
	"io"
	"strings"
)

// Code は設定の code に書いた拡張子のファイル（ソースコード・設定ファイル・ログなど）の Extractor。
// どの拡張子を対象にするかは設定で決まるので、For では返さない（indexer が config.IsCode で選ぶ）。
type Code struct{}

// Type は "code" を返す。
func (Code) Type() string { return "code" }

// Extract はファイル全体を本文として取り出す。タイトルは持たない（索引ではディレクトリからのパスになる）。
func (Code) Extract(r io.Reader) (Document, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return Document{}, err
	}
	return Document{Body: collapseSpace(strings.ToValidUTF8(string(src), ""))}, nil
}
