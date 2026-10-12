// notereader はローカルの HTML / Markdown ファイルを横断検索・閲覧するサーバー。
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kong"

	"github.com/abetd/notereader/internal/config"
)

// CLI はコマンド全体。--config は全サブコマンドで効く。
type CLI struct {
	ConfigPath string `name:"config" short:"c" type:"path" placeholder:"PATH" help:"設定ファイルのパス（既定: ${default_config}）"`

	Serve   ServeCmd   `cmd:"" help:"前面で起動する（Ctrl-C で停止）"`
	Start   StartCmd   `cmd:"" help:"バックグラウンドで起動する"`
	Stop    StopCmd    `cmd:"" help:"バックグラウンドのサーバーを止める"`
	Restart RestartCmd `cmd:"" help:"バックグラウンドのサーバーを起動し直す"`
	Status  StatusCmd  `cmd:"" help:"動いているかを表示する（止まっていれば終了コード 1、動いているが正常に応答しなければ 2）"`
	Reindex ReindexCmd `cmd:"" help:"動いているサーバーで差分の再インデックスを行い、終わるまで待つ（止まっていれば終了コード 1）"`
	Config  ConfigCmd  `cmd:"" help:"設定ファイルを扱う"`
}

// Path は使う設定ファイルのパス。
func (c *CLI) Path() string {
	if c.ConfigPath != "" {
		return c.ConfigPath
	}
	return config.DefaultPath()
}

// exitError はメッセージを出さずに終了コードだけを返すためのエラー。
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func main() {
	var cli CLI
	parser := kong.Must(&cli,
		kong.Name("notereader"),
		kong.Description("ローカルの HTML / Markdown ファイルを横断検索・閲覧するサーバー。"),
		kong.UsageOnError(),
		kong.Vars{"default_config": config.DefaultPath()},
	)
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"--help"}
	}
	ctx, err := parser.Parse(args)
	parser.FatalIfErrorf(err)

	err = ctx.Run(&cli)
	if code, ok := errors.AsType[exitError](err); ok {
		os.Exit(int(code))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "notereader:", err)
		os.Exit(1)
	}
}
