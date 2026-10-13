// Package highlight は chroma でコードをハイライトした HTML を書き出す。
// Markdown のコードブロックと、1 ファイル丸ごとのコード表示で配色と書き出し方を揃える。
package highlight

import (
	"bytes"
	"html"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// MaxColorSize を超えるファイルは色を付けない（chroma の字句解析は数 MB で重くなり、メモリもファイルの数倍使う）。
const MaxColorSize = 1 << 20

// chroma のクラス名で色を当てる（配色はライト・ダークの 2 つを CSS で出し分ける）
var (
	formatter  = chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(false))
	lightStyle = styles.Get("github")
	darkStyle  = styles.Get("github-dark")
)

// Code は lang（Markdown のコードブロックの言語名）でハイライトした HTML を w に書く。
// 知らない言語名なら色を付けない。
func Code(w io.Writer, lang, code string) error {
	return write(w, lexers.Get(lang), code)
}

// File は r の中身（1 ファイル丸ごと）をハイライトした HTML を w に書く。言語はファイル名（拡張子・Makefile など）から決める。
// 言語が分からないときは色を付けない。MaxColorSize を超えるときは色を付けず、
// エスケープしながら少しずつ書き出す（大きなログなどをまるごとメモリに載せないため）。
func File(w io.Writer, name string, r io.Reader) error {
	src, err := io.ReadAll(io.LimitReader(r, MaxColorSize+1))
	if err != nil {
		return err
	}
	if len(src) > MaxColorSize {
		return plain(w, io.MultiReader(bytes.NewReader(src), r))
	}
	return write(w, lexers.Match(name), string(src))
}

// plain は r を色なしで書く。chroma の出力と同じ pre・code で包むので、見た目は色なしのハイライトと揃う。
func plain(w io.Writer, r io.Reader) error {
	if _, err := io.WriteString(w, `<pre class="chroma"><code>`); err != nil {
		return err
	}
	if _, err := io.Copy(escaper{w}, r); err != nil {
		return err
	}
	_, err := io.WriteString(w, "</code></pre>")
	return err
}

// escaper は書き込まれたバイト列を HTML エスケープして w に書く。
// エスケープする文字はすべて 1 バイトなので、どこで区切って書き込まれても結果は変わらない。
type escaper struct{ w io.Writer }

func (e escaper) Write(p []byte) (int, error) {
	if _, err := io.WriteString(e.w, html.EscapeString(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func write(w io.Writer, lexer chroma.Lexer, code string) error {
	if lexer == nil {
		lexer = lexers.Fallback
	}
	iter, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return err
	}
	return formatter.Format(w, lightStyle, iter)
}

// StyleCSS は chroma の配色を CSS で返す。ダークモードはメディアクエリで出し分ける。
func StyleCSS() (string, error) {
	light, err := styleCSS(lightStyle)
	if err != nil {
		return "", err
	}
	dark, err := styleCSS(darkStyle)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(light)
	b.WriteString("\n@media (prefers-color-scheme: dark) {\n")
	b.WriteString(dark)
	// ダーク側で定義されていないトークンは、ライトの色が残って背景に沈む。
	// その分だけ既定の文字色に戻す
	for _, class := range missingClasses(light, dark) {
		b.WriteString(".chroma ." + class + " { color: inherit; background-color: transparent; }\n")
	}
	b.WriteString("}\n")
	return b.String(), nil
}

func styleCSS(style *chroma.Style) (string, error) {
	var b strings.Builder
	if err := formatter.WriteCSS(&b, style); err != nil {
		return "", err
	}
	return b.String(), nil
}

var tokenClass = regexp.MustCompile(`\.chroma \.([\w-]+)`)

// missingClasses は light にあって dark に無いトークンのクラス名を返す。
func missingClasses(light, dark string) []string {
	inDark := make(map[string]bool)
	for _, m := range tokenClass.FindAllStringSubmatch(dark, -1) {
		inDark[m[1]] = true
	}
	seen := make(map[string]bool)
	var out []string
	for _, m := range tokenClass.FindAllStringSubmatch(light, -1) {
		if !inDark[m[1]] && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}
