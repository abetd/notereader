// Package config は notereader の YAML 設定を読み込み、検証する。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/abetd/notereader/internal/extract"
)

// DefaultListen は待ち受けアドレスの既定値。ローカル専用のため 127.0.0.1 に限定する。
const DefaultListen = "127.0.0.1:8787"

// DefaultExclude は常に走査・配信の対象から外す名前（glob）。
// ドットで始まる名前を一律には除外しない（Claude Code の成果物が置かれる .scratch 等を拾うため）。
var DefaultExclude = []string{
	// VCS・依存・ビルド成果物・ツールのキャッシュ
	".git", ".hg", ".svn", "node_modules", ".next", ".nuxt", ".output", ".turbo",
	".cache", ".venv", "__pycache__", ".terraform", ".idea", ".vscode",
	// 秘密情報を含みうるもの
	".env", ".env.*", ".ssh", ".aws", ".gnupg", ".netrc", ".npmrc",
}

// Root は 1 つの名前で扱う監視対象。ディレクトリは複数指定できる。
type Root struct {
	// Name は URL とファイル ID に使う識別子。一度決めたら変えない前提。
	Name string `yaml:"name"`
	// Path は単一ディレクトリの指定。Paths とはどちらか一方だけを書く。
	Path string `yaml:"path"`
	// Paths は複数ディレクトリの指定。文字列でも {path, label} でも書ける。
	Paths []PathEntry `yaml:"paths"`

	// Dirs は検証後の実体。単一指定なら要素 1 つで Prefix は空。
	Dirs []Dir `yaml:"-"`
}

// PathEntry は paths の 1 要素。ラベルを省略すると、パスの末尾から自動で決める。
type PathEntry struct {
	Path  string `yaml:"path"`
	Label string `yaml:"label"`
}

// UnmarshalYAML は "- ~/dir" と "- {path: ~/dir, label: x}" の両方を受け付ける。
func (e *PathEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&e.Path)
	}
	var raw struct {
		Path  string `yaml:"path"`
		Label string `yaml:"label"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	e.Path, e.Label = raw.Path, raw.Label
	return nil
}

// Dir は Root を構成するディレクトリ 1 つ。
type Dir struct {
	// Prefix は複数ディレクトリのときだけ付く、相対パスの先頭要素。
	// 同じ相対パスのファイルが別ディレクトリにあっても ID と URL が衝突しないようにする。
	Prefix string
	Path   string
}

// Rel は Dir 内の相対パス rel を、ルート全体から見た相対パスにする。
func (d Dir) Rel(rel string) string {
	if d.Prefix == "" {
		return rel
	}
	return d.Prefix + "/" + rel
}

// DefaultReconcileInterval は保険の定期照合の間隔。
const DefaultReconcileInterval = 5 * time.Minute

// Config は設定ファイル全体。
type Config struct {
	Runtime `yaml:",inline"`

	Roots []Root `yaml:"roots"`
	// Watch は省略時 true。false にするとファイル監視をせず、
	// 起動時と手動の再インデックスでのみ更新する。
	Watch *bool `yaml:"watch"`
	// Interval は定期照合の間隔（"5m" など）。"0" で定期照合しない。
	Interval string `yaml:"reconcile_interval"`
	// Exclude はディレクトリ名・ファイル名、またはディレクトリからの相対パスに対する glob パターン。
	// paths で複数ディレクトリを書いたルートでも、相対パスはそれぞれのディレクトリから数える（プレフィックスを含めない）。
	Exclude []string `yaml:"exclude"`
	// Archive はアーカイブとみなすディレクトリの glob パターン（ディレクトリ名またはルートからの相対パス）。
	// 相対パスは Exclude と違い、複数ディレクトリのルートではプレフィックスから数える。
	// アーカイブ内のファイルも索引には入れ、検索で既定では除くだけ。
	// 省略時（nil）は DefaultArchive。空のリストを書くとアーカイブを無効にする。
	Archive []string `yaml:"archive"`
	// Code はコードとして索引に入れる拡張子（".go" など）。既定は空で、HTML / Markdown だけを索引に入れる。
	// normalize で小文字・点付きに揃える。
	Code []string `yaml:"code"`

	// archiveExpr・archiveRe は normalize で Archive から組み立てる（archiveReady で済んだかを表す）
	archiveReady bool
	archiveExpr  string
	archiveRe    *regexp.Regexp
}

// DefaultArchive は archive を省略したときに、アーカイブとみなすディレクトリ名。
var DefaultArchive = []string{"archive"}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// invalidInName は自動ラベルに使えない文字。
var invalidInName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// ReservedNames は URL のパスとして使えないルート名。
// ルート単位の画面が /{name} なので、既存のパスと衝突する名前は受け付けない。
var ReservedNames = []string{"api", "view", "files", "static"}

// DefaultPath は設定ファイルの既定の場所を返す。XDG_CONFIG_HOME があれば $XDG_CONFIG_HOME/notereader/config.yaml、
// 無ければ ~/.config/notereader/config.yaml。
func DefaultPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "notereader", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "notereader", "config.yaml")
}

// Load は設定ファイルを読み込み、既定値の補完と検証を行う。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("設定ファイルを読み込めません: %w", err)
	}
	return Parse(data)
}

// Parse は YAML バイト列から設定を組み立てる。
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("設定ファイルの形式が不正です: %w", err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Runtime は起動・停止の制御に要る設定（待ち受けアドレスと data_dir）だけを持つ。
type Runtime struct {
	Listen  string `yaml:"listen"`
	DataDir string `yaml:"data_dir"`
}

// LoadRuntime は設定ファイルから Runtime だけを読む。roots は検証しないので、
// 監視対象のディレクトリが消えていても動いているサーバーを止められる。
func LoadRuntime(path string) (*Runtime, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("設定ファイルを読み込めません: %w", err)
	}
	var rt Runtime
	if err := yaml.Unmarshal(data, &rt); err != nil {
		return nil, fmt.Errorf("設定ファイルの形式が不正です: %w", err)
	}
	if err := rt.normalize(); err != nil {
		return nil, err
	}
	return &rt, nil
}

func (rt *Runtime) normalize() error {
	if rt.Listen == "" {
		rt.Listen = DefaultListen
	}
	if rt.DataDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("キャッシュディレクトリを決定できません: %w", err)
		}
		rt.DataDir = filepath.Join(cache, "notereader")
		return nil
	}
	dir, err := expandPath(rt.DataDir)
	if err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	rt.DataDir = dir
	return nil
}

func (c *Config) normalize() error {
	if err := c.Runtime.normalize(); err != nil {
		return err
	}
	// LoadRuntime（stop・status）では見ない。ループバック以外で動いているサーバーも止められるように
	if err := checkLoopback(c.Listen); err != nil {
		return err
	}

	if len(c.Roots) == 0 {
		return errors.New("roots に監視対象ディレクトリを 1 件以上指定してください")
	}
	seen := make(map[string]bool)
	for i := range c.Roots {
		r := &c.Roots[i]
		if !namePattern.MatchString(r.Name) {
			return fmt.Errorf("roots[%d]: name %q は英数字・_・- のみで指定してください", i, r.Name)
		}
		if slices.Contains(ReservedNames, r.Name) {
			return fmt.Errorf("roots[%d]: name %q は予約語です（%s は使えません）", i, r.Name, strings.Join(ReservedNames, "・"))
		}
		if seen[r.Name] {
			return fmt.Errorf("roots[%d]: name %q が重複しています", i, r.Name)
		}
		seen[r.Name] = true

		if err := r.resolveDirs(i); err != nil {
			return err
		}
	}
	if err := checkDuplicateDirs(c.Roots); err != nil {
		return err
	}

	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil {
			return fmt.Errorf("reconcile_interval %q が不正です: %w", c.Interval, err)
		}
		if d < 0 {
			return fmt.Errorf("reconcile_interval %q は 0 以上で指定してください（\"0\" で定期照合しない）", c.Interval)
		}
	}

	for _, pat := range c.Exclude {
		if _, err := filepath.Match(pat, ""); err != nil {
			return fmt.Errorf("exclude のパターン %q が不正です: %w", pat, err)
		}
	}

	if c.Archive == nil {
		c.Archive = slices.Clone(DefaultArchive)
	}
	for _, pat := range c.Archive {
		if _, err := filepath.Match(pat, ""); err != nil {
			return fmt.Errorf("archive のパターン %q が不正です: %w", pat, err)
		}
	}
	expr, re, err := buildArchive(c.Archive)
	if err != nil {
		return err
	}
	c.archiveReady, c.archiveExpr, c.archiveRe = true, expr, re

	code, err := normalizeCode(c.Code)
	if err != nil {
		return err
	}
	c.Code = code
	return nil
}

var extPattern = regexp.MustCompile(`^\.[a-z0-9_+-]+$`)

// normalizeCode は code の拡張子を小文字・点付きに揃え、重複を除く。
func normalizeCode(exts []string) ([]string, error) {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		ext := strings.ToLower(strings.TrimSpace(e))
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if !extPattern.MatchString(ext) {
			return nil, fmt.Errorf("code の拡張子 %q が不正です（.go や go のように拡張子だけを書く）", e)
		}
		// HTML / Markdown として索引に入る拡張子は書けない（判定は extract の振り分けに任せ、一覧を写さない）
		if _, ok := extract.For("x" + ext); ok {
			return nil, fmt.Errorf("code に %q は書けません（HTML / Markdown として索引に入ります）", e)
		}
		if !slices.Contains(out, ext) {
			out = append(out, ext)
		}
	}
	return out, nil
}

// IsCode は path の拡張子が code に書かれているか（コードとして索引に入れるか）を返す。
func (c *Config) IsCode(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext != "" && slices.Contains(c.Code, ext)
}

// ArchiveRegexp は、アーカイブ内のファイルの相対パス全体に一致する正規表現を返す
// （bleve の rel_path に対する正規表現クエリ用。^ と $ は付けない）。アーカイブが無効なら空文字。
func (c *Config) ArchiveRegexp() string {
	expr, _ := c.archive()
	return expr
}

// Archived はルートからの相対パス rel（スラッシュ区切り）のファイルがアーカイブ内にあるかを判定する。
// 途中のディレクトリのどれかが、名前または rel の先頭からのパスで archive のパターンに一致すればアーカイブ。
// 判定は ArchiveRegexp と同じ正規表現で行う（一覧から除くものとバッジを付けるものを揃えるため）。
func (c *Config) Archived(rel string) bool {
	_, re := c.archive()
	return re != nil && re.MatchString(rel)
}

func (c *Config) archive() (string, *regexp.Regexp) {
	if c.archiveReady {
		return c.archiveExpr, c.archiveRe
	}
	// normalize を通していない Config（テストなど）。省略時の既定もここで補う
	pats := c.Archive
	if pats == nil {
		pats = DefaultArchive
	}
	expr, re, err := buildArchive(pats)
	if err != nil {
		return "", nil
	}
	return expr, re
}

// buildArchive は archive のパターンから、アーカイブ内のファイルの相対パスに一致する正規表現を組み立てる。
// パターンが無ければ空文字と nil を返す。
//   - "/" を含まないパターンはディレクトリ名に当てる: (?:.*/)?名前/.*
//   - "/" を含むパターンはルートからの相対パスに当てる: パス/.*
func buildArchive(pats []string) (string, *regexp.Regexp, error) {
	if len(pats) == 0 {
		return "", nil, nil
	}
	alts := make([]string, 0, len(pats))
	for _, pat := range pats {
		g, err := globToRegexp(pat)
		if err != nil {
			return "", nil, fmt.Errorf("archive のパターン %q が不正です: %w", pat, err)
		}
		if strings.Contains(pat, "/") {
			alts = append(alts, g+"/.*")
		} else {
			alts = append(alts, "(?:.*/)?"+g+"/.*")
		}
	}
	expr := strings.Join(alts, "|")
	re, err := regexp.Compile("^(?:" + expr + ")$")
	if err != nil {
		return "", nil, fmt.Errorf("archive のパターンを組み立てられません: %w", err)
	}
	return expr, re, nil
}

// globToRegexp は filepath.Match の glob を、1 つのパス要素の中だけで一致する正規表現にする。
// * と ? と文字クラスはスラッシュに一致させない。
func globToRegexp(pat string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(pat); i++ {
		switch ch := pat[i]; ch {
		case '*':
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		case '\\':
			if i+1 >= len(pat) {
				return "", filepath.ErrBadPattern
			}
			i++
			b.WriteString(regexp.QuoteMeta(pat[i : i+1]))
		case '[':
			end := classEnd(pat, i+1)
			if end < 0 {
				return "", filepath.ErrBadPattern
			}
			body := classBody(pat[i+1 : end])
			if neg, ok := strings.CutPrefix(body, "^"); ok {
				b.WriteString("[^/" + neg + "]")
			} else {
				b.WriteString("[" + body + "]")
			}
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(pat[i : i+1]))
		}
	}
	return b.String(), nil
}

// classBody は文字クラスの中身を正規表現の書き方にする。glob の \x は文字 x そのものなので、
// 英数字は打ち消しを外し（正規表現では \d などが別の意味になる）、記号だけ \ を残す。
func classBody(body string) string {
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			i++
			if c := body[i]; c < utf8.RuneSelf && !unicode.IsLetter(rune(c)) && !unicode.IsDigit(rune(c)) {
				b.WriteByte('\\')
			}
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

// classEnd は pat[start:] にある文字クラスの閉じ括弧の位置を返す（\ による文字の打ち消しは飛ばす）。
func classEnd(pat string, start int) int {
	for j := start; j < len(pat); j++ {
		switch pat[j] {
		case '\\':
			j++
		case ']':
			if j > start {
				return j
			}
		}
	}
	return -1
}

// resolveDirs は path / paths を検証して Dirs を組み立てる。
func (r *Root) resolveDirs(i int) error {
	raw := r.Paths
	switch {
	case r.Path != "" && len(r.Paths) > 0:
		return fmt.Errorf("roots[%d] (%s): path と paths は同時に指定できません", i, r.Name)
	case r.Path != "":
		raw = []PathEntry{{Path: r.Path}}
	case len(r.Paths) == 0:
		return fmt.Errorf("roots[%d] (%s): path または paths を指定してください", i, r.Name)
	}

	r.Dirs = make([]Dir, 0, len(raw))
	for _, one := range raw {
		p, err := expandPath(one.Path)
		if err != nil {
			return fmt.Errorf("roots[%d] (%s): %w", i, r.Name, err)
		}
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("roots[%d] (%s): %w", i, r.Name, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("roots[%d] (%s): %s はディレクトリではありません", i, r.Name, p)
		}
		if one.Label != "" && !namePattern.MatchString(one.Label) {
			return fmt.Errorf("roots[%d] (%s): label %q は英数字・_・- のみで指定してください", i, r.Name, one.Label)
		}
		for _, d := range r.Dirs {
			if d.Path == p {
				return fmt.Errorf("roots[%d] (%s): %s が 2 回指定されています", i, r.Name, p)
			}
		}
		r.Dirs = append(r.Dirs, Dir{Path: p, Prefix: one.Label})
	}
	// ラベル無しの 1 件だけならプレフィックスを付けない（ID と URL を短く保つ）。
	// 2 件目を足すとプレフィックスが付き、そのルートの ID と URL はすべて変わる
	if len(r.Dirs) == 1 && r.Dirs[0].Prefix == "" {
		return nil
	}

	if err := assignPrefixes(r.Dirs); err != nil {
		return fmt.Errorf("roots[%d] (%s): %w", i, r.Name, err)
	}
	for j, a := range r.Dirs {
		for k, b := range r.Dirs {
			if j != k && strings.HasPrefix(b.Path, a.Path+string(filepath.Separator)) {
				return fmt.Errorf("roots[%d] (%s): %s は %s の中にあります（入れ子のディレクトリは指定できません）", i, r.Name, b.Path, a.Path)
			}
		}
	}
	return nil
}

// checkDuplicateDirs は、同じディレクトリが別のルートにも書かれていないかを確かめる（同じルートの中は resolveDirs で見る）。
// 書かれていると同じファイルが 2 件ずつ索引に入る。
func checkDuplicateDirs(roots []Root) error {
	seen := make(map[string]string)
	for _, r := range roots {
		for _, d := range r.Dirs {
			if prev, ok := seen[d.Path]; ok {
				return fmt.Errorf("%s が 2 回指定されています（%s と %s）", d.Path, prev, r.Name)
			}
			seen[d.Path] = r.Name
		}
	}
	return nil
}

// checkLoopback は listen がループバックのアドレスかを確かめる。
// Host ヘッダの検査（DNS リバインディング対策）は、LAN から直接つながれるのを防げないため。
func checkLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q が不正です: %w", listen, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen %q はループバックのアドレス（127.0.0.1・::1・localhost）で指定してください", listen)
}

// assignPrefixes はラベル未指定のディレクトリに、パスの末尾から一意になる名前を付ける。
// 末尾のディレクトリ名が重なる場合（複数リポジトリの .scratch など）は、
// 全体が区別できる深さまで一律に親を足す（scratch ではなく my-tools-scratch / dotfiles-scratch）。
func assignPrefixes(dirs []Dir) error {
	used := make(map[string]bool)
	auto := make([]int, 0, len(dirs))
	maxDepth := 0
	for i, d := range dirs {
		if d.Prefix != "" {
			if used[d.Prefix] {
				return fmt.Errorf("label %q が重複しています", d.Prefix)
			}
			used[d.Prefix] = true
			continue
		}
		auto = append(auto, i)
		maxDepth = max(maxDepth, len(pathSegments(d.Path)))
	}
	if len(auto) == 0 {
		return nil
	}

	for depth := 1; depth <= maxDepth; depth++ {
		cands := make(map[int]string, len(auto))
		seen := make(map[string]bool, len(auto))
		ok := true
		for _, i := range auto {
			segs := pathSegments(dirs[i].Path)
			cand := strings.Join(segs[max(len(segs)-depth, 0):], "-")
			if cand == "" || used[cand] || seen[cand] {
				ok = false
				break
			}
			seen[cand] = true
			cands[i] = cand
		}
		if !ok {
			continue
		}
		for i, cand := range cands {
			dirs[i].Prefix = cand
		}
		return nil
	}
	return errors.New("ディレクトリ名を決められません（label を指定してください）")
}

// pathSegments は URL に使える形に整えたパスの要素を返す。
func pathSegments(path string) []string {
	var out []string
	for seg := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		seg = strings.TrimPrefix(seg, ".")
		seg = invalidInName.ReplaceAllString(seg, "-")
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// DirByPrefix は prefix に対応するディレクトリを返す。単一ディレクトリのルートでは prefix は空。
func (r *Root) DirByPrefix(prefix string) (Dir, bool) {
	for _, d := range r.Dirs {
		if d.Prefix == prefix {
			return d, true
		}
	}
	return Dir{}, false
}

// WatchEnabled はファイル監視を行うか（省略時は行う）。
func (c *Config) WatchEnabled() bool { return c.Watch == nil || *c.Watch }

// ReconcileInterval は定期照合の間隔を返す。0 なら定期照合しない。
func (c *Config) ReconcileInterval() time.Duration {
	if c.Interval == "" {
		return DefaultReconcileInterval
	}
	d, err := time.ParseDuration(c.Interval)
	if err != nil {
		return DefaultReconcileInterval
	}
	return d
}

// Excluded はディレクトリ（Dir）からの相対パス rel（スラッシュ区切り）のディレクトリ・ファイルを走査から外すかを判定する。
// 複数ディレクトリのルートでも、rel にプレフィックスを含めない（索引・監視・配信で揃える）。
// 末尾の名前が既定の除外名に一致するか、名前または rel が設定の exclude パターンに一致すれば除外する。
// 途中のディレクトリは見ないので、配下すべてを判定したい場合は ExcludedPath を使う。
func (c *Config) Excluded(rel string) bool {
	name := rel[strings.LastIndex(rel, "/")+1:]
	for _, pat := range DefaultExclude {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	for _, pat := range c.Exclude {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// ExcludedPath は rel 自身またはその途中のディレクトリのいずれかが除外対象かを判定する。
func (c *Config) ExcludedPath(rel string) bool {
	for i := 0; i <= len(rel); i++ {
		if (i == len(rel) || rel[i] == '/') && i > 0 && c.Excluded(rel[:i]) {
			return true
		}
	}
	return false
}

func expandPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("パスが空です")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}
