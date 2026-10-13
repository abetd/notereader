package markdown

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	src := `---
title: 設計メモ
---

# 見出し

本文です。

| 列 | 値 |
|----|----|
| a  | 1  |

- [ ] タスク
`
	doc, err := Render([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "設計メモ" {
		t.Errorf("Title = %q", doc.Title)
	}
	html := string(doc.HTML)
	for _, want := range []string{"<h1", "見出し", "<table>", "<li>", "checkbox"} {
		if !strings.Contains(html, want) {
			t.Errorf("%q が無い: %s", want, html)
		}
	}
	if strings.Contains(html, "title: 設計メモ") {
		t.Errorf("frontmatter が本文に残っている: %s", html)
	}
	if doc.HasMermaid {
		t.Error("mermaid が無いのに HasMermaid = true")
	}
}

func TestTitleFallback(t *testing.T) {
	for name, tt := range map[string]struct{ src, want string }{
		"frontmatter 優先": {"---\ntitle: FM\n---\n\n# 見出し\n", "FM"},
		"最初の見出し":         {"本文\n\n## 二番目でも先頭なら使う\n", "二番目でも先頭なら使う"},
		"見出しが無い":         {"ただの本文\n", ""},
		// YAML として読めない場合は本文として扱う。
		// その結果 "---" の直前行は Markdown の setext 見出しになる
		"frontmatter として読めない": {"---\n読めない: [\n---\n# 見出し\n", "読めない: ["},
	} {
		doc, err := Render([]byte(tt.src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if doc.Title != tt.want {
			t.Errorf("%s: Title = %q, want %q", name, doc.Title, tt.want)
		}
	}
}

func TestCodeAndMermaid(t *testing.T) {
	src := "```go\nfunc main() { println(\"hi\") }\n```\n\n```mermaid\ngraph TD; A-->B;\n```\n"
	doc, err := Render([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	html := string(doc.HTML)
	// Go のコードはサーバー側でハイライト（chroma のクラスが付く）
	if !strings.Contains(html, `class="chroma"`) || !strings.Contains(html, "<span") {
		t.Errorf("ハイライトされていない: %s", html)
	}
	// mermaid はそのままの内容で出す（描画はブラウザ側）
	if !strings.Contains(html, `<pre class="mermaid">graph TD; A--&gt;B;`) {
		t.Errorf("mermaid ブロックが期待と違う: %s", html)
	}
	if !doc.HasMermaid {
		t.Error("HasMermaid = false")
	}
}
