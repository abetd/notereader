// Package watcher はファイルの追加・更新・削除を検知してインデックスを同期する。
//
// fsnotify のイベントは「どこかが変わった」という合図としてだけ使い、種別は解釈しない。
// rename・tmp からの差し替え・サブディレクトリの追加など、イベントの出方は OS や
// エディタによって変わるため、変化があったらインデックス全体を stat で照合し直す。
// 取りこぼしの保険として、一定間隔でも照合する。
package watcher

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/indexer"
)

const (
	// DefaultDebounce はイベントが止んでから同期するまでの待ち時間。
	DefaultDebounce = 500 * time.Millisecond
	// maxDelay は書き込みが続いているときでも、これ以上は同期を遅らせない上限。
	maxDelay = 3 * time.Second
)

// Watcher は監視対象ディレクトリの変化を見てインデックスを同期する。
type Watcher struct {
	cfg *config.Config
	idx *indexer.Index

	// Debounce はイベントをまとめる待ち時間。Run の前に変更できる。
	Debounce time.Duration
	// Interval は保険の定期照合の間隔。0 なら定期照合しない。
	Interval time.Duration

	fsw *fsnotify.Watcher
}

// New は Watcher を作る。Run を呼ぶまで監視は始まらない。
func New(cfg *config.Config, idx *indexer.Index) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		cfg:      cfg,
		idx:      idx,
		Debounce: DefaultDebounce,
		Interval: cfg.ReconcileInterval(),
		fsw:      fsw,
	}, nil
}

// Run は ctx がキャンセルされるまで監視を続ける。
func (w *Watcher) Run(ctx context.Context) error {
	defer w.fsw.Close()
	for _, root := range w.cfg.Roots {
		for _, dir := range root.Dirs {
			w.addTree(dir.Path, dir)
		}
	}

	// debounce 用のタイマー。発火待ちでないときは停止している
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	pending := false
	var firstEvent time.Time
	// schedule は同期を予約する。書き込みが続く間は待つが、maxDelay を超えたら待たない
	schedule := func() {
		if !pending {
			pending = true
			firstEvent = time.Now()
		}
		delay := min(w.Debounce, max(maxDelay-time.Since(firstEvent), 0))
		stopTimer(timer)
		timer.Reset(delay)
	}

	var tick <-chan time.Time
	if w.Interval > 0 {
		t := time.NewTicker(w.Interval)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			// 新しいディレクトリは監視に加える。watch を足す前に作られた
			// ファイルを拾うため、この後の同期で中身ごと取り込む
			if ev.Has(fsnotify.Create) {
				// Lstat なので symlink のディレクトリは辿らない
				if st, err := os.Lstat(ev.Name); err == nil && st.IsDir() {
					if dir, ok := w.dirOf(ev.Name); ok {
						w.addTree(ev.Name, dir)
					}
				}
			}
			schedule()

		case <-timer.C:
			pending = false
			if w.sync(ctx, "変更を検知") {
				// 実行中の同期は、このディレクトリを既に通り過ぎているかもしれない。終わってから照合し直す
				schedule()
			}

		case <-tick:
			if !pending {
				w.sync(ctx, "定期照合")
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			// イベントのあふれ（inotify の ErrEventOverflow）などで変更を取りこぼしているかもしれないので、照合し直す
			slog.Warn("監視エラー", "err", err)
			schedule()
		}
	}
}

// sync は同期を 1 回走らせる。ほかの同期が実行中で走らせなかったときは busy=true を返す。
func (w *Watcher) sync(ctx context.Context, reason string) (busy bool) {
	res, err := w.idx.TrySync(ctx)
	switch {
	case errors.Is(err, indexer.ErrSyncing):
		return true
	case err != nil:
		if ctx.Err() == nil {
			slog.Error("同期に失敗しました", "reason", reason, "err", err)
		}
		return false
	}
	if res.Indexed > 0 || res.Deleted > 0 {
		slog.Info("インデックスを更新しました", "reason", reason, "result", res)
	}
	return false
}

// addTree は base 以下のディレクトリを監視に加える。
func (w *Watcher) addTree(base string, dir config.Dir) {
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // 読めない場所は飛ばす
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir.Path, p)
		if err != nil {
			return nil //nolint:nilerr // 監視に加えられない場所は飛ばす
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && w.cfg.Excluded(rel) {
			return fs.SkipDir
		}
		if err := w.fsw.Add(p); err != nil {
			slog.Warn("監視に追加できません", "path", p, "err", err)
		}
		return nil
	})
	if err != nil {
		slog.Warn("監視の追加に失敗しました", "dir", base, "err", err)
	}
}

// dirOf は path がどの監視対象ディレクトリに属するかを返す。
func (w *Watcher) dirOf(path string) (config.Dir, bool) {
	for _, r := range w.cfg.Roots {
		for _, d := range r.Dirs {
			if rel, err := filepath.Rel(d.Path, path); err == nil && !filepath.IsAbs(rel) && rel != ".." && !hasDotDotPrefix(rel) {
				return d, true
			}
		}
	}
	return config.Dir{}, false
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
