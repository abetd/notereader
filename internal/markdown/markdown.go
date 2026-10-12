// Package markdown は Markdown を HTML に変換する。
//
// コードブロックは chroma でサーバー側ハイライトし、mermaid のブロックだけは
// そのままの内容を <pre class="mermaid"> として出す（描画はブラウザ側の mermaid.js）。
package markdown

import (
	"bytes"
	"html/template"
	"regexp"
	"sort"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"gopkg.in/yaml.v3"
)

// MermaidLang は mermaid として扱う言語名。
const MermaidLang = "mermaid"

// chroma のクラス名で色を当てる（配色はライト・ダークの 2 つを CSS で出し分ける）
var (
	formatter  = chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(false))
	lightStyle = styles.Get("github")
	darkStyle  = styles.Get("github-dark")
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Footnote, extension.DefinitionList),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(
		gmhtml.WithUnsafe(), // ローカルの自分のファイルなので HTML の直書きを許す
		renderer.WithNodeRenderers(util.Prioritized(&codeRenderer{}, 100)),
	),
)

// Document は変換結果。
type Document struct {
	// Title は frontmatter の title → 最初の見出し → 空、の順で決まる。
	Title string
	HTML  []byte
	// HasMermaid が true のときだけ、表示側で mermaid.js を読み込めばよい。
	HasMermaid bool
}

// Render は Markdown を HTML に変換する。
func Render(src []byte) (Document, error) {
	body, fmTitle := splitFrontMatter(src)

	doc := md.Parser().Parse(text.NewReader(body))
	var out bytes.Buffer
	if err := md.Renderer().Render(&out, body, doc); err != nil {
		return Document{}, err
	}

	title := fmTitle
	if title == "" {
		title = firstHeading(doc, body)
	}
	return Document{
		Title:      title,
		HTML:       out.Bytes(),
		HasMermaid: bytes.Contains(out.Bytes(), []byte(`<pre class="mermaid">`)),
	}, nil
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

// splitFrontMatter は YAML frontmatter を取り除き、本文と title を返す。
func splitFrontMatter(src []byte) (body []byte, title string) {
	if !bytes.HasPrefix(src, []byte("---\n")) && !bytes.HasPrefix(src, []byte("---\r\n")) {
		return src, ""
	}
	rest := src[bytes.IndexByte(src, '\n')+1:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return src, ""
	}
	var meta struct {
		Title string `yaml:"title"`
	}
	if err := yaml.Unmarshal(rest[:end], &meta); err != nil {
		return src, "" // frontmatter として読めないならそのまま本文にする
	}
	after := rest[end+1:]
	if nl := bytes.IndexByte(after, '\n'); nl >= 0 {
		after = after[nl+1:]
	} else {
		after = nil
	}
	return after, strings.TrimSpace(meta.Title)
}

// firstHeading は最初の見出しのテキストを返す。
func firstHeading(doc ast.Node, src []byte) string {
	var title string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || title != "" {
			return ast.WalkContinue, nil
		}
		if h, ok := n.(*ast.Heading); ok {
			title = strings.TrimSpace(string(h.Lines().Value(src)))
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return title
}

// codeRenderer はフェンス付きコードブロックだけを自前で描画する。
type codeRenderer struct{}

func (r *codeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, r.renderFenced)
}

func (r *codeRenderer) renderFenced(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.FencedCodeBlock)
	lang := string(n.Language(src))
	code := lines(n, src)

	if lang == MermaidLang {
		// mermaid.js が読む要素。中身はそのまま（エスケープのみ）
		// 書き込みのエラーは BufWriter に残り、描画の最後の Flush で返る
		_, _ = w.WriteString(`<pre class="mermaid">`)
		template.HTMLEscape(w, code)
		_, _ = w.WriteString("</pre>\n")
		return ast.WalkSkipChildren, nil
	}

	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	iter, err := chroma.Coalesce(lexer).Tokenise(nil, string(code))
	if err != nil {
		return ast.WalkStop, err
	}
	if err := formatter.Format(w, lightStyle, iter); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}

// lines はコードブロックの中身を取り出す。
func lines(n *ast.FencedCodeBlock, src []byte) []byte {
	var b bytes.Buffer
	l := n.Lines()
	for i := 0; i < l.Len(); i++ {
		line := l.At(i)
		b.Write(line.Value(src))
	}
	return b.Bytes()
}
