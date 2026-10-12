package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/daemon"
)

// StartCmd はバックグラウンドで起動する。
type StartCmd struct{}

func (c *StartCmd) Run(cli *CLI) error {
	return start(cli.Path())
}

// StopCmd はバックグラウンドのサーバーを止める。
type StopCmd struct{}

func (c *StopCmd) Run(cli *CLI) error {
	rt, err := config.LoadRuntime(cli.Path())
	if err != nil {
		return err
	}
	return stop(rt)
}

// RestartCmd は止めてから起動する。止まっていれば起動だけ行う。
type RestartCmd struct{}

func (c *RestartCmd) Run(cli *CLI) error {
	rt, err := config.LoadRuntime(cli.Path())
	if err != nil {
		return err
	}
	if err := stop(rt); err != nil {
		return err
	}
	return start(cli.Path())
}

// StatusCmd は動いているかを表示する。
// 終了コードは、動いていて応答があれば 0、止まっていれば 1、プロセスはいる（または応答はある）が正常に応答しなければ 2。
type StatusCmd struct{}

func (c *StatusCmd) Run(cli *CLI) error {
	rt, err := config.LoadRuntime(cli.Path())
	if err != nil {
		return err
	}
	url := daemon.BaseURL(rt.Listen)
	pid, err := daemon.PathsFor(rt.DataDir).Running()
	if errors.Is(err, daemon.ErrNotRunning) {
		// PID ファイルが無くても、serve で前面起動していれば応答はある
		st, err := daemon.FetchStatus(rt.Listen)
		switch {
		case err == nil:
			fmt.Printf("前面で動いています（PID %d、notereader serve）: %s\n", st.PID, url)
			printCounts(st)
			return nil
		case errors.Is(err, daemon.ErrBadResponse):
			fmt.Printf("バックグラウンドでは動いていませんが、%s が応答しています。%v\n", url, err)
			return exitError(2)
		}
		fmt.Println("停止中")
		if !errors.Is(err, syscall.ECONNREFUSED) {
			fmt.Printf("（%s に接続を試みましたが、応答がありません: %v）\n", url, err)
		}
		return exitError(1)
	}
	if err != nil {
		return err
	}
	st, err := daemon.FetchStatus(rt.Listen)
	if err != nil {
		fmt.Printf("動いています（PID %d）が、%s が正常に応答しません: %v\n", pid, url, err)
		return exitError(2)
	}
	fmt.Printf("動いています（PID %d）: %s\n", pid, url)
	printCounts(st)
	return nil
}

// ReindexCmd は動いているサーバーに差分の再インデックスを頼み、終わるまで待って結果を出す。
// 止まっていれば終了コード 1（索引は直接触らない。止まっている間の変更は次の起動時の同期が取り込む）、応答が異常なら 2。
type ReindexCmd struct{}

func (c *ReindexCmd) Run(cli *CLI) error {
	rt, err := config.LoadRuntime(cli.Path())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	url := daemon.BaseURL(rt.Listen)
	s, err := daemon.Reindex(ctx, rt.Listen)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		fmt.Println("待つのをやめました（同期はサーバーで続きます）")
		return exitError(1)
	case errors.Is(err, daemon.ErrBadResponse):
		fmt.Printf("%s が正常に応答しません: %v\n", url, err)
		return exitError(2)
	default:
		fmt.Println("停止中")
		if !errors.Is(err, syscall.ECONNREFUSED) {
			fmt.Printf("（%s に接続を試みましたが、応答がありません: %v）\n", url, err)
		}
		return exitError(1)
	}
	if s.Error != "" {
		fmt.Printf("再インデックスに失敗しました: %s\n", s.Error)
		return exitError(1)
	}
	r := s.Last
	fmt.Printf("再インデックスしました（取り込み %d、変化なし %d、削除 %d、%s）\n", r.Indexed, r.Unchanged, r.Deleted, r.Duration.Round(time.Millisecond))
	if r.Failed > 0 {
		fmt.Printf("読めないファイルが %d 件あります（理由はサーバーのログにあります）\n", r.Failed)
	}
	return nil
}

func printCounts(st *daemon.Status) {
	fmt.Printf("ファイル数: %d\n", st.Documents)
	for _, r := range st.Roots {
		fmt.Printf("  %s: %d\n", r.Name, r.Documents)
	}
}

// start は設定を検証してから、notereader serve を子プロセスとして起動する。
func start(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// 子プロセスに任せる前に検証し、設定の誤りはその場で返す
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	paths := daemon.PathsFor(cfg.DataDir)
	pid, started, err := paths.Start(daemon.StartOptions{
		Exe:    exe,
		Args:   []string{"serve", "--config", path},
		Listen: cfg.Listen,
	})
	if err != nil {
		return err
	}
	url := daemon.BaseURL(cfg.Listen)
	if !started {
		fmt.Printf("既に動いています（PID %d）: %s\n", pid, url)
		return nil
	}
	fmt.Printf("起動しました（PID %d）: %s\n", pid, url)
	fmt.Printf("ログ: %s\n", paths.Log)
	return nil
}

// stop は rt の data_dir にある PID のサーバーを止める。動いていなければその旨を出すだけ。
func stop(rt *config.Runtime) error {
	pid, err := daemon.PathsFor(rt.DataDir).Stop(daemon.StopTimeout)
	if errors.Is(err, daemon.ErrNotRunning) {
		fmt.Println("動いていません")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("停止しました（PID %d）\n", pid)
	return nil
}
