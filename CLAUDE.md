# CLAUDE.md

ローカルの HTML / Markdown を横断検索・閲覧するサーバー notereader の公開リポジトリ（MIT）。Go の単一バイナリで、CLI は kong。

## 構成

- `cmd/notereader/`: サブコマンド。`main.go` が kong の CLI 定義、`serve.go`（前面起動）、`lifecycle.go`（start / stop / restart / status / reindex）、`config_cmd.go`（config init / show / edit / add / remove）
- `internal/daemon/`: バックグラウンド起動・停止。PID ファイル（`notereader.pid`）とログ（`notereader.log`、1 世代を `.1` に回す）は `data_dir` 配下。status・reindex が動いているサーバーに問い合わせる処理もここ
- `internal/config/`: 設定の読み込みと検証（アーカイブの判定と、bleve 用の正規表現の組み立てもここ）。`LoadRuntime` は `listen` と `data_dir` だけを読む（roots が壊れていても stop / status できるように）。`edit.go` が config add / remove の書き戻し（yaml.v3 の Node でコメントを保つ）
- `internal/config/config.example.yaml`: `config init` の雛形。`internal/config/example.go` で埋め込み、そのまま書き出す
- それ以外（indexer・search・server・watcher・extract・highlight・markdown・web）は README の「開発」を参照

## 決めてあること

次を変えるときは、理由ごと見直す（手元に `.scratch/notereader-lifecycle-commands.html` があれば設計の経緯が残っている。リポジトリには含めない）。

- バックグラウンド起動は自前で子プロセス（`notereader serve`）を切り離す。launchd などは使わない。ログイン時の自動起動は持たない
- `stop` は SIGTERM を送って待つだけで、強制終了しない。停止用の HTTP エンドポイントは作らない（ブラウザで開いた別サイトからの POST で止められてしまうため）
- `reindex` は動いているサーバーに `POST /api/reindex` で頼んで待つだけで、索引を直接開かない（サーバーが索引のロックを持っている。止まっている間の変更は起動時の同期が取り込む）。画面には再インデックスのボタンを置かない（普段は監視と定期照合で足りる）
- `/api/status` の `pid` は、`start` が応答したのが自分の起こした子かを確かめるのに使っている。消さない
- インデックスのロック待ちにタイムアウトを付けない。`indexer.Open` は既存インデックスを開けないと作り直すので、ロック待ちの失敗で動いている側のインデックスを消してしまう
- `config add` / `remove` はルート単位だけ。`paths` の中身は `config edit` で手で書く
- アーカイブ（設定の `archive`、既定 `archive/`）は索引に入れたまま、検索のたびに `rel_path` の正規表現で一覧から除く。索引に印を持たせない（差分同期は署名が同じファイルを書き直さないので、`archive` を変えたときに古い印が残る）。画面のバッジ（`Config.Archived`）と除外（`Config.ArchiveRegexp`）は同じ正規表現から作り、食い違わないようにしている
- アーカイブに移すとファイル ID が変わり、古い `/view/{id}` は 404 になる。移動先を追いかける仕組みは持たない
- `exclude` の相対パスは各ディレクトリから数え（`paths` で付く名前を含めない。索引・監視・`/files` の配信で揃える）、`archive` はルートから数える（付いた名前を含む。ラベルが `archive` のディレクトリを丸ごとアーカイブにできるように）
- `listen` はループバックに限る。検証は `Config.normalize` だけで、`LoadRuntime` では見ない（stop・status は listen が何でも動くように）。Host ヘッダの検査は DNS リバインディング対策で、LAN から直接つながれるのは防げないため
- HTML / Markdown 以外のテキストファイルのコード表示は、`/files` へのリクエストの `Sec-Fetch-Dest` が `document` / `iframe` のときだけ返す（`.scratch/notereader-code-view.html` に経緯）。`/files` は HTML から読み込まれる CSS・JS・`fetch` も配信しているので、拡張子で決めるとその読み込みが壊れる。同じ URL でも返すものが変わるので `Vary: Sec-Fetch-Dest` を消さない
- コードを索引に入れるかは設定の `code` に書いた拡張子で決め（既定は空）、表示するかは中身（NUL を含まない・UTF-8）で決める。索引まで中身で決めると、HTML の付属ファイル（CSS・JS・SVG）や lock ファイルで一覧が埋まる。SVG（`image/*`）は画像のまま返す
- 抽出の仕方（`internal/extract`）を変えたら `indexer.SchemaVersion` を上げる。差分同期は署名が同じファイルを抽出し直さないので、上げないと既存の索引に反映されない

## 変更したら揃えるもの

- コマンド・フラグ・設定キー・既定値を変えたら、`README.md`・`internal/config/config.example.yaml`・このファイルを揃える
- `internal/config/config.example.yaml` は `config init` の雛形でもある。`roots:` は空のまま（例はコメントに書く）にしておく。例のルートを有効にすると、存在しないディレクトリで検証が通らず、`config init` の直後の `config add` が失敗する（`internal/config/edit_test.go` の `TestAddRootToExample` で確かめている）
- `web/` を変えたら、README の「開発」の手順でブラウザで確かめる（自動テストは無い）

## テスト

- `make test`（`go test ./...`）
- `make lint`（golangci-lint v2、設定は `.golangci.yml`）。指摘は 0 件のまま保つ。意図して握りつぶすエラーは `//nolint:nilerr // 理由` のように理由を書く
- `cmd/notereader` のテストはバイナリをビルドし、`start` / `stop` / `config` を実際のプロセスとして動かす。空いているポートと一時ディレクトリを使う
- サンドボックスなどで `ps` を実行できないと、PID の生存確認はシグナル 0 だけになる（`internal/daemon` の `alive`）

## 書かないこと

公開リポジトリなので、個人の絶対パス・業務固有の記述（社内のプロジェクト名・チケット接頭辞など）を入れない。例のパスは `~/notes`・`~/projects/app` のような一般的なものにする。
