package extract

import (
	"bytes"
	"io"

	"github.com/abetd/notereader/internal/markdown"
)

// Markdown は Markdown ファイルの Extractor。
type Markdown struct{}

// Type は "markdown" を返す。
func (Markdown) Type() string { return "markdown" }

// Extract はタイトルと、記法を除いた本文テキストを取り出す。
// いったん HTML に変換してからタグを落とすので、表示側と同じ解釈になる。
// コードブロックの中身も本文に含める（識別子やコマンドを検索したいため）。
func (Markdown) Extract(r io.Reader) (Document, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return Document{}, err
	}
	rendered, err := markdown.Render(src)
	if err != nil {
		return Document{}, err
	}
	text, err := HTML{}.Extract(bytes.NewReader(rendered.HTML))
	if err != nil {
		return Document{}, err
	}
	return Document{
		Title: rendered.Title,
		Body:  text.Body,
	}, nil
}
