package extract

import (
	"errors"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// HTML は HTML ファイルの Extractor。
type HTML struct{}

// Type は "html" を返す。
func (HTML) Type() string { return "html" }

// skipTags の中身は本文として扱わない。
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true,
}

// breakTags は前後で語を区切るタグ（ブロック要素と改行）。
// インライン要素（b・a・span など）では区切らない。「これは<b>重要</b>です」に空白が入ると、
// 空白をまたがない bigram の索引で「は重要」「重要です」が引けなくなる。
var breakTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "body": true, "br": true,
	"caption": true, "dd": true, "details": true, "dialog": true, "div": true, "dl": true, "dt": true,
	"fieldset": true, "figcaption": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "head": true, "header": true,
	"hr": true, "html": true, "img": true, "li": true, "main": true, "nav": true, "ol": true, "option": true,
	"p": true, "pre": true, "section": true, "summary": true, "table": true, "tbody": true, "td": true,
	"tfoot": true, "th": true, "thead": true, "title": true, "tr": true, "ul": true,
}

// Extract は <title> と、タグを除いた本文テキストを取り出す。
func (HTML) Extract(r io.Reader) (Document, error) {
	z := html.NewTokenizer(r)
	var title, body strings.Builder
	var inTitle bool
	skipDepth := 0

	for {
		switch z.Next() {
		case html.ErrorToken:
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return Document{}, err
			}
			return Document{
				Title: collapseSpace(title.String()),
				Body:  collapseSpace(body.String()),
			}, nil
		case html.StartTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipTags[tag]:
				skipDepth++
			case tag == "title":
				inTitle = true
			}
			// ブロック境界で語がくっつかないよう区切りを入れる
			if breakTags[tag] || skipTags[tag] {
				body.WriteByte(' ')
			}
		case html.SelfClosingTagToken:
			// <br/> など
			if name, _ := z.TagName(); breakTags[string(name)] {
				body.WriteByte(' ')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipTags[tag] && skipDepth > 0:
				skipDepth--
			case tag == "title":
				inTitle = false
			}
			if breakTags[tag] || skipTags[tag] {
				body.WriteByte(' ')
			}
		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			text := string(z.Text())
			if inTitle {
				title.WriteString(text)
				continue
			}
			body.WriteString(text)
		}
	}
}
