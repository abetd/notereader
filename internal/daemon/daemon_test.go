package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseURL(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:8787": "http://127.0.0.1:8787",
		"[::1]:9000":     "http://[::1]:9000",
		"localhost:8787": "http://localhost:8787",
	} {
		if got := BaseURL(listen); got != want {
			t.Errorf("BaseURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestRotateLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notereader.log")
	// 無くてもエラーにしない
	if err := rotateLog(path); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"1 回目\n", "2 回目\n"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := rotateLog(path); err != nil {
			t.Fatal(err)
		}
	}
	// 1 世代だけ残る
	got, err := os.ReadFile(path + ".1")
	if err != nil || string(got) != "2 回目\n" {
		t.Errorf(".1 = %q, %v", got, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("回したあとも元のログが残っている: %v", err)
	}
}

func TestTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	var b strings.Builder
	for i := range 30 {
		b.WriteString(strings.Repeat("x", i) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(tail(path, 5), "\n")
	if len(lines) != 5 || lines[4] != strings.Repeat("x", 29) {
		t.Errorf("tail = %q", lines)
	}
	if tail(filepath.Join(t.TempDir(), "nope"), 5) != "" {
		t.Error("無いファイルで空にならない")
	}
}

func TestRunningWithoutPIDFile(t *testing.T) {
	p := PathsFor(t.TempDir())
	if _, err := p.Running(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v", err)
	}
	if _, err := p.Stop(0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Stop: err = %v", err)
	}
}

func TestRunningWithBrokenPIDFile(t *testing.T) {
	p := PathsFor(t.TempDir())
	if err := os.WriteFile(p.PID, []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Running(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v", err)
	}
}
