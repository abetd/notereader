// Package web はテンプレートと静的ファイルをバイナリに埋め込む。
package web

import "embed"

//go:embed templates static
var FS embed.FS
