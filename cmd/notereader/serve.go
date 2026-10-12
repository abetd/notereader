package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/indexer"
	"github.com/abetd/notereader/internal/server"
	"github.com/abetd/notereader/internal/watcher"
)

// ServeCmd は前面でサーバーを動かす。start も子プロセスとしてこれを起動する。
type ServeCmd struct{}

func (c *ServeCmd) Run(cli *CLI) error {
	cfg, err := config.Load(cli.Path())
	if err != nil {
		return err
	}

	// 同じ data_dir の notereader が動いていると、ここでインデックスのロックを無期限に待つ。
	// 待っている間のシグナルは既定の動作で終了させたいので、NotifyContext はこの後に呼ぶ
	idx, err := indexer.Open(cfg)
	if err != nil {
		return err
	}
	defer idx.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.WatchEnabled() {
		w, err := watcher.New(cfg, idx)
		if err != nil {
			return err
		}
		go func() {
			if err := w.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("監視が止まりました", "err", err)
			}
		}()
	}

	srv, err := server.New(ctx, cfg, idx)
	if err != nil {
		return err
	}
	defer srv.Close()

	// 既存インデックスで即検索できる状態のまま、停止中の変更を裏で取り込む
	go func() {
		res, err := idx.Sync(ctx)
		if err != nil {
			// 同期の途中で止めたときは失敗として記録しない
			if ctx.Err() == nil {
				slog.Error("起動時の同期に失敗しました", "err", err)
			}
			return
		}
		slog.Info("起動時の同期完了", "result", res)
	}()

	slog.Info("起動しました", "url", "http://"+cfg.Listen, "data_dir", cfg.DataDir,
		"watch", cfg.WatchEnabled(), "reconcile_interval", cfg.ReconcileInterval())
	return srv.Run(ctx, cfg.Listen)
}
