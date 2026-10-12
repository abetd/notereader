// Package daemon は notereader をバックグラウンドで起動・停止する。
// PID ファイルとログファイルは data_dir 配下に置く。
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/abetd/notereader/internal/indexer"
)

const (
	pidName = "notereader.pid"
	logName = "notereader.log"

	// StartTimeout は start がサーバーの応答を待つ上限。
	StartTimeout = 10 * time.Second
	// StopTimeout は stop が終了を待つ上限。超えても強制終了はしない。
	StopTimeout = 10 * time.Second

	pollInterval = 100 * time.Millisecond
	logTailLines = 20
)

// ErrNotRunning はバックグラウンドのサーバーが動いていないことを表す。
var ErrNotRunning = errors.New("動いていません")

// ErrBadResponse は /api/status に HTTP の応答はあったが、正常な状態を返さなかったことを表す。
var ErrBadResponse = errors.New("応答が異常です")

var errBadPID = errors.New("PID ファイルの中身が不正です")

// Paths は data_dir 配下の PID ファイルとログファイルの場所。
type Paths struct {
	PID string
	Log string
}

// PathsFor は data_dir から Paths を求める。
func PathsFor(dataDir string) Paths {
	return Paths{PID: filepath.Join(dataDir, pidName), Log: filepath.Join(dataDir, logName)}
}

// Running は PID ファイルのプロセスが notereader として動いていればその PID を返す。
// 動いていなければ ErrNotRunning を返す（PID ファイルが古くても消さない）。
// 中身が壊れた PID ファイルも、古いものと同じく動いていない扱いにする（start で消して起動し直せる）。
func (p Paths) Running() (int, error) {
	pid, err := readPID(p.PID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errBadPID) {
		return 0, ErrNotRunning
	}
	if err != nil {
		return 0, err
	}
	if !alive(pid) {
		return 0, ErrNotRunning
	}
	return pid, nil
}

// StartOptions は Start の引数。
type StartOptions struct {
	// Exe と Args は子プロセスとして起動するコマンド（notereader serve ...）。
	Exe  string
	Args []string
	// Listen は応答を待つ待ち受けアドレス。
	Listen string
	// Timeout は応答を待つ上限。0 なら StartTimeout。
	Timeout time.Duration
}

// StartError は起動に失敗したときのエラー。ログの末尾を持つ。
type StartError struct {
	Reason  string
	LogPath string
	LogTail string
}

func (e *StartError) Error() string {
	msg := fmt.Sprintf("起動できませんでした: %s（ログ: %s）", e.Reason, e.LogPath)
	if e.LogTail != "" {
		msg += "\n--- ログの末尾 ---\n" + e.LogTail
	}
	return msg
}

// Start は子プロセスを端末から切り離して起動し、Listen が応答するまで待つ。
// 既に動いていれば何もせず、その PID と started=false を返す。
// 古い PID ファイルは消し、前回のログは 1 世代だけ .1 に回す。
func (p Paths) Start(opts StartOptions) (pid int, started bool, err error) {
	if pid, err := p.Running(); err == nil {
		return pid, false, nil
	} else if !errors.Is(err, ErrNotRunning) {
		return 0, false, err
	}
	// serve で前面起動しているなどで既に応答があるなら起こさない。
	// 同じ data_dir だと子はインデックスのロック待ちで止まってしまうため、先に断る
	if other, err := Ping(opts.Listen); err == nil {
		return 0, false, fmt.Errorf("%s で別の notereader（PID %d）が応答しています。notereader serve で前面起動していないか確かめてください", BaseURL(opts.Listen), other)
	}
	if err := os.MkdirAll(filepath.Dir(p.PID), 0o755); err != nil {
		return 0, false, err
	}
	if err := os.Remove(p.PID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, false, err
	}
	if err := rotateLog(p.Log); err != nil {
		return 0, false, err
	}
	logFile, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, false, err
	}
	defer logFile.Close()

	cmd := exec.Command(opts.Exe, opts.Args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, false, err
	}
	pid = cmd.Process.Pid
	if err := os.WriteFile(p.PID, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return 0, false, err
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = StartTimeout
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	// lastErr は最後の確認で応答が得られなかった理由（時間切れの理由に添える）
	var lastErr error
	for {
		select {
		case werr := <-exited:
			_ = os.Remove(p.PID)
			reason := "すぐに終了しました"
			if werr != nil {
				reason = fmt.Sprintf("すぐに終了しました（%v）", werr)
			}
			return 0, false, p.startError(reason)
		case <-deadline:
			// 動いたまま応答しない子は残さない
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = os.Remove(p.PID)
			reason := fmt.Sprintf("%s 以内に %s が応答しませんでした", timeout, BaseURL(opts.Listen))
			if lastErr != nil {
				reason += fmt.Sprintf("（最後の確認: %v）", lastErr)
			}
			reason += "。同じ data_dir を使う notereader が動いていると、インデックスのロック待ちで止まります"
			return 0, false, p.startError(reason)
		case <-tick.C:
			// 同じアドレスで別の notereader（serve など）が応答しても、自分の子でなければ待ち続ける。
			// data_dir が違えば子はポートを取れずに終了するので、exited の側で失敗として拾える。
			// 同じ data_dir なら子はインデックスのロック待ちで止まり、deadline の側で失敗になる
			got, err := Ping(opts.Listen)
			switch {
			case err != nil:
				lastErr = err
			case got != pid:
				lastErr = fmt.Errorf("別のプロセス（PID %d）が応答しています", got)
			default:
				return pid, true, nil
			}
		}
	}
}

func (p Paths) startError(reason string) error {
	return &StartError{Reason: reason, LogPath: p.Log, LogTail: tail(p.Log, logTailLines)}
}

// StopError は待っても止まらなかったときのエラー。
type StopError struct {
	PID     int
	Timeout time.Duration
}

func (e *StopError) Error() string {
	return fmt.Sprintf("PID %d が %s 以内に止まりませんでした。強制終了するなら kill -9 %d", e.PID, e.Timeout, e.PID)
}

// Stop は SIGTERM を送り、終了するまで timeout だけ待つ。強制終了はしない。
// 動いていなければ ErrNotRunning を返す（古い PID ファイルは消す）。
func (p Paths) Stop(timeout time.Duration) (int, error) {
	pid, err := p.Running()
	if errors.Is(err, ErrNotRunning) {
		_ = os.Remove(p.PID)
		return 0, ErrNotRunning
	}
	if err != nil {
		return 0, err
	}
	if timeout == 0 {
		timeout = StopTimeout
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return pid, err
	}
	deadline := time.Now().Add(timeout)
	for alive(pid) {
		if time.Now().After(deadline) {
			return pid, &StopError{PID: pid, Timeout: timeout}
		}
		time.Sleep(pollInterval)
	}
	_ = os.Remove(p.PID)
	return pid, nil
}

// BaseURL は待ち受けアドレスからブラウザで開く URL を作る（listen は設定の検証でループバックに限っている）。
func BaseURL(listen string) string {
	return "http://" + listen
}

// Status は /api/status の応答のうち、コマンドから使う部分。
type Status struct {
	PID       int `json:"pid"`
	Documents int `json:"documents"`
	Roots     []struct {
		Name      string `json:"name"`
		Documents int    `json:"documents"`
	} `json:"roots"`
	Sync indexer.SyncStatus `json:"sync"`
}

// FetchStatus は /api/status を取得する。応答はあったが 200 でない・形式が違うときは ErrBadResponse を包んで返す。
func FetchStatus(listen string) (*Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL(listen)+"/api/status", nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w（/api/status が %d を返しました）", ErrBadResponse, res.StatusCode)
	}
	var st Status
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("%w（/api/status を読めません: %w）", ErrBadResponse, err)
	}
	return &st, nil
}

// Reindex は動いているサーバーに再インデックスを頼み、頼んだ後に始まった同期が終わるまで待って、その状態を返す。
// 既に同期中なら、その同期は頼む前に始まっていて今の変更を拾わないかもしれないので、終わるのを待ってから頼み直す。
func Reindex(ctx context.Context, listen string) (*indexer.SyncStatus, error) {
	for {
		st, err := FetchStatus(listen)
		if err != nil {
			return nil, err
		}
		if !st.Sync.Syncing {
			started, err := postReindex(ctx, listen)
			if err != nil {
				return nil, err
			}
			if started {
				// 頼む直前に同期中でなかったので、この後に終わる同期は頼んだ後に始まったもの
				return waitSync(ctx, listen, st.Sync.LastSync)
			}
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return nil, err
		}
	}
}

// postReindex は POST /api/reindex を送る。始まれば true、既に同期中（409）なら false を返す。
func postReindex(ctx context.Context, listen string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL(listen)+"/api/reindex", nil)
	if err != nil {
		return false, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusAccepted:
		return true, nil
	case http.StatusConflict:
		return false, nil
	}
	return false, fmt.Errorf("%w（/api/reindex が %d を返しました）", ErrBadResponse, res.StatusCode)
}

// waitSync は、最後の同期の終了時刻が before から変わり、同期中でなくなるまで待つ。
func waitSync(ctx context.Context, listen string, before *time.Time) (*indexer.SyncStatus, error) {
	for {
		if err := sleep(ctx, pollInterval); err != nil {
			return nil, err
		}
		st, err := FetchStatus(listen)
		if err != nil {
			return nil, err
		}
		if !st.Sync.Syncing && !sameTime(st.Sync.LastSync, before) {
			return &st.Sync, nil
		}
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Ping は /api/status に応答したプロセスの PID を返す。
func Ping(listen string) (int, error) {
	st, err := FetchStatus(listen)
	if err != nil {
		return 0, err
	}
	return st.PID, nil
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, errBadPID
	}
	return pid, nil
}

// alive は pid のプロセスが生きていて、notereader であるかを返す。
// PID ファイルが古いまま別のプロセスに番号が再利用されていても、そのプロセスにシグナルを送らないため。
// 名前に notereader を含むか、いま動いている自分の実行ファイルと同じ名前なら notereader とみなす（別名でインストールした場合）。
// ps を実行できない環境（サンドボックスなど）では、生きているかだけで判断する。
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return false
	}
	if err != nil {
		return true
	}
	name := filepath.Base(strings.TrimSpace(string(out)))
	return strings.Contains(name, "notereader") || name == selfName
}

// selfName は自分の実行ファイルの名前。分からなければ空（notereader を含む名前だけを認める）。
var selfName = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Base(exe)
}()

// rotateLog は前回のログを .1 に回す（1 世代だけ残す）。
func rotateLog(path string) error {
	err := os.Rename(path, path+".1")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// tail はファイルの末尾 n 行を返す。読めなければ空。
func tail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
