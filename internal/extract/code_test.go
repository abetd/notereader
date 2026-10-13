package extract

import (
	"strings"
	"testing"
)

func TestCodeExtract(t *testing.T) {
	src := "package main\n\n// 起動する\nfunc main() {\n\tprintln(\"hi\")\n}\n\xff"
	doc, err := Code{}.Extract(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "" {
		t.Errorf("Title = %q", doc.Title)
	}
	if want := `package main // 起動する func main() { println("hi") }`; doc.Body != want {
		t.Errorf("Body = %q, want %q", doc.Body, want)
	}
}
