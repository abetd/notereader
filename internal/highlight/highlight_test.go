package highlight

import (
	"strings"
	"testing"
)

func TestFile(t *testing.T) {
	for name, tt := range map[string]struct {
		file, src string
		colored   bool
	}{
		"拡張子から言語を決める":   {"main.go", "package main\n\nfunc main() {}\n", true},
		"拡張子の無いファイル名":   {"Makefile", "all:\n\tgo build\n", true},
		"言語が分からなければ色なし": {"app.log", "2026-10-13 起動しました\n", false},
		"大きすぎれば色なし":     {"big.go", "package main\n" + strings.Repeat("// x\n", MaxColorSize/5), false},
	} {
		var b strings.Builder
		if err := File(&b, tt.file, strings.NewReader(tt.src)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		html := b.String()
		if !strings.Contains(html, `<pre class="chroma">`) {
			t.Errorf("%s: chroma の pre が無い: %.200s", name, html)
		}
		// 色はトークンのクラス（<span class="kd"> など）で付く。行の span（line・cl）だけなら色なし
		colored := strings.Contains(strings.NewReplacer(`<span class="line">`, "", `<span class="cl">`, "").Replace(html), `<span class="`)
		if colored != tt.colored {
			t.Errorf("%s: 色付き = %v, want %v: %.300s", name, colored, tt.colored, html)
		}
	}
}

func TestFileEscapes(t *testing.T) {
	// 色を付ける大きさと、少しずつ書き出す大きさの両方で確かめる
	for _, pad := range []int{0, MaxColorSize} {
		var b strings.Builder
		if err := File(&b, "a.txt", strings.NewReader(strings.Repeat(" ", pad)+"<script>alert(1)</script>")); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "<script>") || !strings.Contains(b.String(), "&lt;script&gt;") {
			t.Errorf("pad %d: エスケープされていない: %.200s", pad, b.String()[max(0, b.Len()-200):])
		}
	}
}

// 大きなファイルは中身を欠かさずに書き出す（読み込みの区切りで落ちたり重なったりしない）
func TestFileLarge(t *testing.T) {
	src := strings.Repeat("0123456789abcde\n", MaxColorSize/16*3) + "末尾"
	var b strings.Builder
	if err := File(&b, "big.log", strings.NewReader(src)); err != nil {
		t.Fatal(err)
	}
	want := `<pre class="chroma"><code>` + src + "</code></pre>"
	if b.String() != want {
		t.Errorf("len = %d, want %d", b.Len(), len(want))
	}
}

func TestStyleCSS(t *testing.T) {
	css, err := StyleCSS()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(css, ".chroma") || !strings.Contains(css, "prefers-color-scheme: dark") {
		t.Errorf("CSS = %.200s", css)
	}
	// ダーク側に無いトークンは文字色を戻す（ライトの濃い色が残ると背景に沈む）
	light, dark, _ := strings.Cut(css, "@media")
	for _, m := range tokenClass.FindAllStringSubmatch(light, -1) {
		if !strings.Contains(dark, ".chroma ."+m[1]+" ") && !strings.Contains(dark, ".chroma ."+m[1]+"{") {
			t.Errorf("ダークに %s の指定が無い", m[1])
		}
	}
}
