package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/abetd/notereader/internal/config"
	"github.com/abetd/notereader/internal/daemon"
)

// ConfigCmd は設定ファイルを扱うサブコマンドの束。
type ConfigCmd struct {
	Init   ConfigInitCmd   `cmd:"" help:"設定ファイルの雛形を書き出す"`
	Show   ConfigShowCmd   `cmd:"" help:"設定ファイルの場所と、既定値を埋めた内容を表示する"`
	Edit   ConfigEditCmd   `cmd:"" help:"エディタで設定ファイルを開く（閉じたら検証し、バックグラウンドで動いていれば再起動）"`
	Add    ConfigAddCmd    `cmd:"" help:"ルートを足す（検証し、バックグラウンドで動いていれば再起動）"`
	Remove ConfigRemoveCmd `cmd:"" help:"ルートを丸ごと消す（検証し、バックグラウンドで動いていれば再起動）"`
}

// ConfigInitCmd は config.example.yaml を設定ファイルとして書き出す。
type ConfigInitCmd struct {
	Force bool `help:"既にあっても上書きする"`
}

func (c *ConfigInitCmd) Run(cli *CLI) error {
	path := cli.Path()
	if _, err := os.Stat(path); err == nil && !c.Force {
		return fmt.Errorf("既にあります: %s（上書きするなら --force）", path)
	}
	if err := writeExample(path); err != nil {
		return err
	}
	fmt.Printf("書き出しました: %s\n", path)
	fmt.Println("roots を自分のディレクトリに書き換えてください（notereader config edit / config add）")
	return nil
}

func writeExample(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, config.Example, 0o644)
}

// ConfigShowCmd は検証・正規化した設定を表示する。
type ConfigShowCmd struct{}

type showConfig struct {
	Listen            string     `yaml:"listen"`
	DataDir           string     `yaml:"data_dir"`
	Watch             bool       `yaml:"watch"`
	ReconcileInterval string     `yaml:"reconcile_interval"`
	Roots             []showRoot `yaml:"roots"`
	Exclude           []string   `yaml:"exclude,omitempty"`
	Archive           []string   `yaml:"archive"`
}

type showRoot struct {
	Name string    `yaml:"name"`
	Dirs []showDir `yaml:"dirs"`
}

type showDir struct {
	Prefix string `yaml:"prefix,omitempty"`
	Path   string `yaml:"path"`
}

func (c *ConfigShowCmd) Run(cli *CLI) error {
	path := cli.Path()
	fmt.Printf("# 設定ファイル: %s\n", path)
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	paths := daemon.PathsFor(cfg.DataDir)
	fmt.Printf("# PID ファイル: %s\n# ログ: %s\n", paths.PID, paths.Log)

	view := showConfig{
		Listen:            cfg.Listen,
		DataDir:           cfg.DataDir,
		Watch:             cfg.WatchEnabled(),
		ReconcileInterval: cfg.ReconcileInterval().String(),
		Exclude:           cfg.Exclude,
		Archive:           cfg.Archive,
	}
	for _, r := range cfg.Roots {
		sr := showRoot{Name: r.Name}
		for _, d := range r.Dirs {
			sr.Dirs = append(sr.Dirs, showDir{Prefix: d.Prefix, Path: d.Path})
		}
		view.Roots = append(view.Roots, sr)
	}
	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	if err := enc.Encode(view); err != nil {
		return err
	}
	return enc.Close()
}

// ConfigEditCmd はエディタで設定ファイルを開く。
type ConfigEditCmd struct{}

func (c *ConfigEditCmd) Run(cli *CLI) error {
	path := cli.Path()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := writeExample(path); err != nil {
			return err
		}
		fmt.Printf("雛形を書き出しました: %s\n", path)
	}
	// 変更前の data_dir で動いているサーバーを止められるよう、先に控えておく
	before, beforeErr := config.LoadRuntime(path)

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// "code -w" のように引数付きでも動くよう、シェルに渡す
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("エディタが失敗しました: %w", err)
	}
	if _, err := config.Load(path); err != nil {
		return fmt.Errorf("設定に誤りがあります（動いているサーバーには反映しません）: %w", err)
	}
	return applyChange(before, beforeErr, path)
}

// ConfigAddCmd はルートを足す。
type ConfigAddCmd struct {
	Name string `arg:"" help:"ルート名（URL とファイル ID に使う。英数字・_・-）"`
	Dir  string `arg:"" type:"path" help:"再帰的に走査するディレクトリ"`
}

func (c *ConfigAddCmd) Run(cli *CLI) error {
	info, err := os.Stat(c.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s はディレクトリではありません", c.Dir)
	}
	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		return err
	}
	return rewrite(cli.Path(), func(src []byte) ([]byte, error) {
		return config.AddRoot(src, c.Name, dir)
	}, fmt.Sprintf("ルート %s を足しました: %s", c.Name, dir))
}

// ConfigRemoveCmd はルートを丸ごと消す。
type ConfigRemoveCmd struct {
	Name string `arg:"" help:"消すルート名"`
}

func (c *ConfigRemoveCmd) Run(cli *CLI) error {
	return rewrite(cli.Path(), func(src []byte) ([]byte, error) {
		return config.RemoveRoot(src, c.Name)
	}, fmt.Sprintf("ルート %s を消しました", c.Name))
}

// rewrite は設定ファイルを change で書き換える。change が検証に通らなければファイルは触らない。
func rewrite(path string, change func([]byte) ([]byte, error), done string) error {
	src, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("設定ファイルがありません: %s（notereader config init で作れます）", path)
	}
	if err != nil {
		return err
	}
	before, beforeErr := config.LoadRuntime(path)
	out, err := change(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, info.Mode().Perm()); err != nil {
		return err
	}
	fmt.Println(done)
	return applyChange(before, beforeErr, path)
}

// applyChange は変更前の設定（before）で動いているサーバーがあれば、止めて新しい設定で起動し直す。
// listen や data_dir が変わっても、変更前の PID ファイルを見て止める。
// 変更前の設定が読めなかったとき（beforeErr。壊れた YAML を直したときなど）は、変更後の設定の data_dir で探す。
func applyChange(before *config.Runtime, beforeErr error, path string) error {
	if before == nil {
		after, err := config.LoadRuntime(path)
		if err != nil {
			return err
		}
		fmt.Printf("変更前の設定を読めなかったので、変更後の data_dir で動いているサーバーを探します（%v）\n", beforeErr)
		before = after
	}
	_, err := daemon.PathsFor(before.DataDir).Running()
	if errors.Is(err, daemon.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("設定は書き換えましたが、サーバーが動いているか確かめられません: %w", err)
	}
	fmt.Println("動いているサーバーを再起動します")
	if err := stop(before); err != nil {
		return err
	}
	return start(path)
}
