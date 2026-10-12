package extract

import (
	"strings"
	"testing"
)

func TestHTMLExtract(t *testing.T) {
	src := `<!doctype html><html><head><title> 設計  メモ </title>
<style>body{color:red}</style><script>var x = "秘密";</script></head>
<body><h1>見出し</h1><p>本文<b>です</b>。</p><noscript>無効</noscript><p>next</p></body></html>`

	doc, err := HTML{}.Extract(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "設計 メモ" {
		t.Errorf("Title = %q", doc.Title)
	}
	for _, want := range []string{"見出し", "本文", "です", "next"} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("Body に %q が含まれない: %q", want, doc.Body)
		}
	}
	for _, ng := range []string{"color", "秘密", "無効", "設計"} {
		if strings.Contains(doc.Body, ng) {
			t.Errorf("Body に %q が含まれている: %q", ng, doc.Body)
		}
	}
}

// インライン要素の境目では区切らず、ブロック要素と改行では区切る
func TestHTMLExtractWordBoundaries(t *testing.T) {
	src := `<p>これは<b>重要</b>です</p><p>次の段落</p>行1<br>行2<br/>行3<a href="x">リンク</a>先`
	doc, err := HTML{}.Extract(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if want := "これは重要です 次の段落 行1 行2 行3リンク先"; doc.Body != want {
		t.Errorf("Body = %q, want %q", doc.Body, want)
	}
}

func TestFor(t *testing.T) {
	if _, ok := For("a/b/C.HTML"); !ok {
		t.Error(".HTML が対象にならない")
	}
	if _, ok := For("a/b/c.md"); !ok {
		t.Error(".md が対象にならない")
	}
	if _, ok := For("a/b/c.txt"); ok {
		t.Error(".txt が対象になっている")
	}
}

func TestMarkdownExtract(t *testing.T) {
	src := "---\ntitle: 設計メモ\n---\n\n# 見出し\n\n本文です。\n\n```go\nfunc Hello() {}\n```\n"
	doc, err := Markdown{}.Extract(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "設計メモ" {
		t.Errorf("doc = %+v", doc)
	}
	// 記法は落ちるが、コードブロックの中身は本文に残す
	for _, want := range []string{"見出し", "本文です。", "func Hello"} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("Body に %q が無い: %q", want, doc.Body)
		}
	}
	for _, ng := range []string{"```", "title: 設計メモ"} {
		if strings.Contains(doc.Body, ng) {
			t.Errorf("Body に %q が残っている: %q", ng, doc.Body)
		}
	}
}

func TestForMarkdown(t *testing.T) {
	for _, path := range []string{"a/b.md", "a/b.MARKDOWN"} {
		if !IsMarkdown(path) {
			t.Errorf("%s が Markdown 扱いにならない", path)
		}
	}
	if IsMarkdown("a/b.html") {
		t.Error("html が Markdown 扱いになっている")
	}
}
