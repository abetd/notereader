package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// AddRoot は設定ファイルの中身 src に、path 形式のルート（name と dir）を末尾に足したものを返す。
// 書き戻しは yaml.Node 経由で行い、手書きのコメントと並びを保つ。
// 結果は Parse で検証し、通らなければエラーを返す（呼び出し側はファイルを書き換えない）。
func AddRoot(src []byte, name, dir string) ([]byte, error) {
	doc, err := parseDoc(src)
	if err != nil {
		return nil, err
	}
	roots, err := rootsNode(doc, true)
	if err != nil {
		return nil, err
	}
	if rootIndex(roots, name) >= 0 {
		return nil, fmt.Errorf("ルート %q は既にあります", name)
	}
	roots.Content = append(roots.Content, &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalar("name"), scalar(name),
			scalar("path"), scalar(contractHome(dir)),
		},
	})
	return encodeChecked(doc)
}

// RemoveRoot は src から name のルートを丸ごと消したものを返す。検証は AddRoot と同じ。
func RemoveRoot(src []byte, name string) ([]byte, error) {
	doc, err := parseDoc(src)
	if err != nil {
		return nil, err
	}
	roots, err := rootsNode(doc, false)
	if err != nil {
		return nil, err
	}
	i := rootIndex(roots, name)
	if i < 0 {
		return nil, fmt.Errorf("ルート %q はありません", name)
	}
	roots.Content = slices.Delete(roots.Content, i, i+1)
	return encodeChecked(doc)
}

func parseDoc(src []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("設定ファイルの形式が不正です: %w", err)
	}
	if doc.Kind == 0 {
		// 空のファイル
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("設定ファイルの最上位がマッピングではありません")
	}
	return &doc, nil
}

// rootsNode は roots のシーケンスを返す。create なら無いときに作る。
func rootsNode(doc *yaml.Node, create bool) (*yaml.Node, error) {
	top := doc.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "roots" {
			continue
		}
		v := top.Content[i+1]
		switch {
		case v.Kind == yaml.SequenceNode:
			return v, nil
		case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
			// "roots:" だけ書かれた状態（config init の直後）。値に付いたコメントは残す
			*v = yaml.Node{Kind: yaml.SequenceNode, HeadComment: v.HeadComment, LineComment: v.LineComment, FootComment: v.FootComment}
			return v, nil
		default:
			return nil, errors.New("roots がシーケンスではありません")
		}
	}
	if !create {
		return nil, errors.New("roots がありません")
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	top.Content = append(top.Content, scalar("roots"), seq)
	return seq, nil
}

func rootIndex(roots *yaml.Node, name string) int {
	for i, r := range roots.Content {
		if r.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(r.Content); j += 2 {
			if r.Content[j].Value == "name" && r.Content[j+1].Value == name {
				return i
			}
		}
	}
	return -1
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func encodeChecked(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if _, err := Parse(buf.Bytes()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// contractHome はホームディレクトリ配下の絶対パスを ~/ 始まりにする（設定ファイルを別のマシンでも読めるように）。
func contractHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}
