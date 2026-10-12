package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// バイナリをビルドして、start / status / stop / restart と config の各コマンドを通しで動かす。
// start は自分自身を serve として起動し直すので、go test のプロセスでは確かめられない。

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "notereader-bin-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "notereader")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t       *testing.T
	config  string
	dataDir string
	root    string
	// editor は config edit で使うエディタ（空なら何もしない true）
	editor string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, config: filepath.Join(base, "config.yaml"), dataDir: filepath.Join(base, "data"), root: filepath.Join(base, "notes")}
	mustWrite(t, filepath.Join(e.root, "a.md"), "# メモ\n")
	mustWrite(t, e.config, "# 手書きのコメント\nlisten: "+freeAddr(t)+"\ndata_dir: "+e.dataDir+"\nroots:\n  - name: notes\n    path: "+e.root+"\n")
	t.Cleanup(func() { _ = exec.Command(bin, "--config", e.config, "stop").Run() })
	return e
}

// run は notereader を実行し、出力と終了コードを返す。
func (e *env) run(args ...string) (string, int) {
	e.t.Helper()
	cmd := exec.Command(bin, append([]string{"--config", e.config}, args...)...)
	editor := e.editor
	if editor == "" {
		editor = "true"
	}
	cmd.Env = append(os.Environ(), "EDITOR="+editor, "VISUAL=")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return string(out), code
}

func (e *env) expect(wantCode int, wantOut string, args ...string) string {
	e.t.Helper()
	out, code := e.run(args...)
	if code != wantCode || !strings.Contains(out, wantOut) {
		e.t.Fatalf("notereader %s: code=%d（期待 %d）, 出力に %q が無い:\n%s", strings.Join(args, " "), code, wantCode, wantOut, out)
	}
	return out
}

func TestLifecycle(t *testing.T) {
	e := newEnv(t)

	e.expect(1, "停止中", "status")
	e.expect(0, "動いていません", "stop")
	e.expect(0, "起動しました", "start")
	e.expect(0, "既に動いています", "start")
	e.expect(0, "ファイル数: 1", "status")

	e.expect(0, "停止しました", "restart")
	if _, err := os.Stat(filepath.Join(e.dataDir, "notereader.log.1")); err != nil {
		t.Errorf("前回のログが .1 に回っていない: %v", err)
	}
	e.expect(0, "ファイル数: 1", "status")

	e.expect(0, "停止しました", "stop")
	e.expect(1, "停止中", "status")
	if _, err := os.Stat(filepath.Join(e.dataDir, "notereader.pid")); !os.IsNotExist(err) {
		t.Errorf("PID ファイルが残っている: %v", err)
	}
}

// 監視を止めた状態で、足したファイルが reindex で取り込まれる
func TestReindex(t *testing.T) {
	e := newEnv(t)
	src, _ := os.ReadFile(e.config)
	mustWrite(t, e.config, string(src)+"watch: false\nreconcile_interval: \"0\"\n")

	e.expect(1, "停止中", "reindex")
	e.expect(0, "起動しました", "start")
	mustWrite(t, filepath.Join(e.root, "b.md"), "# 追加\n")
	e.expect(0, "取り込み 1、変化なし 1、削除 0", "reindex")
	e.expect(0, "ファイル数: 2", "status")
}

func TestStartFailsWhenPortInUse(t *testing.T) {
	e := newEnv(t)
	src, _ := os.ReadFile(e.config)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mustWrite(t, e.config, strings.Replace(string(src), "listen: ", "listen: "+ln.Addr().String()+"\n# ", 1))

	e.expect(1, "ログの末尾", "start")
	e.expect(1, "停止中", "status")
}

func TestStalePIDFile(t *testing.T) {
	e := newEnv(t)
	// 終了済みのプロセスの PID を残しておく
	done := exec.Command("true")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(e.dataDir, "notereader.pid"), strconv.Itoa(done.Process.Pid)+"\n")
	e.expect(1, "停止中", "status")
	e.expect(0, "起動しました", "start")
}

func TestConfigCommands(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(filepath.Dir(e.config), "boards")
	mustWrite(t, filepath.Join(other, "b.html"), "<title>ボード</title>")

	if out := e.expect(0, "name: notes", "config", "show"); !strings.Contains(out, "archive:\n  - archive") {
		t.Errorf("config show に既定の archive が出ない:\n%s", out)
	}
	e.expect(1, "既にあります", "config", "init")

	// 止まっているときは再起動しない
	out := e.expect(0, "ルート boards を足しました", "config", "add", "boards", other)
	if strings.Contains(out, "再起動") {
		t.Errorf("止まっているのに再起動した:\n%s", out)
	}
	src, _ := os.ReadFile(e.config)
	if !strings.Contains(string(src), "# 手書きのコメント") || !strings.Contains(string(src), "name: boards") {
		t.Errorf("書き戻し:\n%s", src)
	}
	e.expect(1, "既にあります", "config", "add", "boards", other)
	e.expect(1, "no such file", "config", "add", "x", filepath.Join(other, "nope"))

	// 動いているときは再起動して反映する
	e.expect(0, "起動しました", "start")
	e.expect(0, "ファイル数: 2", "status")
	e.expect(0, "再起動します", "config", "remove", "boards")
	e.expect(0, "ファイル数: 1", "status")
	e.expect(1, "ありません", "config", "remove", "boards")

	// edit はエディタ（ここでは true）を閉じたあと検証して再起動する
	e.expect(0, "再起動します", "config", "edit")

	// 誤った設定にしたら再起動しない
	mustWrite(t, e.config, string(src)+"reconcile_interval: abc\n")
	e.expect(1, "反映しません", "config", "edit")
	e.expect(0, "動いています", "status")

	// 壊れた YAML をエディタで直したときも、変更後の data_dir で探して再起動する
	good := filepath.Join(t.TempDir(), "good.yaml")
	mustWrite(t, good, string(src))
	mustWrite(t, e.config, string(src)+"roots: [\n")
	e.editor = "cp " + good
	out = e.expect(0, "再起動します", "config", "edit")
	if !strings.Contains(out, "変更前の設定を読めなかった") {
		t.Errorf("変更前の設定を読めなかったことを伝えていない:\n%s", out)
	}
	e.expect(0, "動いています", "status")
}

// PID ファイルが無くても、待ち受けているサーバーが異常な応答を返すなら「停止中」と区別する
func TestStatusBadResponse(t *testing.T) {
	e := newEnv(t)
	src, _ := os.ReadFile(e.config)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	mustWrite(t, e.config, strings.Replace(string(src), "listen: ", "listen: "+ln.Addr().String()+"\n# ", 1))

	e.expect(2, "500", "status")
	e.expect(2, "500", "reindex")
}

func TestConfigInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	out, err := exec.Command(bin, "--config", path, "config", "init").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	src, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(src), "roots:") {
		t.Fatalf("雛形: %v\n%s", err, src)
	}
	if err := exec.Command(bin, "--config", path, "config", "init", "--force").Run(); err != nil {
		t.Errorf("--force で上書きできない: %v", err)
	}
}

func TestNoArgsShowsHelp(t *testing.T) {
	out, err := exec.Command(bin).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Commands:") {
		t.Errorf("%v:\n%s", err, out)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
