# ai-working-calendar

[![go](https://github.com/sters/ai-working-calendar/workflows/Go/badge.svg)](https://github.com/sters/ai-working-calendar/actions?query=workflow%3AGo)
[![coverage](docs/coverage.svg)](https://github.com/sters/ai-working-calendar)
[![go-report](https://goreportcard.com/badge/github.com/sters/ai-working-calendar)](https://goreportcard.com/report/github.com/sters/ai-working-calendar)

Claude Code のセッションログ（`~/.claude/projects/*/*.jsonl`）を読み、作業した時間帯をカレンダー形式で表示する localhost 専用の Web ツールです。

## Install

```shell
go install github.com/sters/ai-working-calendar@latest
```

## Usage

```shell
ai-working-calendar
# open http://127.0.0.1:8137
```

| Flag | Default | 内容 |
|---|---|---|
| `-addr` | `127.0.0.1:8137` | listen するアドレス。ログにはプロンプトやコードが含まれるので、ループバック以外を指定しないこと |
| `-dir` | `~/.claude/projects` | セッションログのディレクトリ |
| `-gap` | `30m` | 1 つのセッションを別の予定に分ける無操作時間 |
| `-cache` | `<UserCacheDir>/ai-working-calendar/index.json` | 集計結果のキャッシュ。空文字で無効 |

## 表示の仕様

### 予定の単位

- 1 つのセッションログを、メッセージの間隔が `-gap` を超えた位置で複数の予定（作業ブロック）に分けます。`--resume` で翌日に再開したセッションは、作業した時間帯ごとの予定になります。
- サイドバーの「稼働区間」に切り替えると、Claude が実際に動いていたターンだけを予定として表示します。5 分以内に続いたターンは 1 つにまとめます。
- 予定のタイトルは `ai-title`、なければ最初のプロンプト、それもなければ `last-prompt` です。色はセッション開始時の `cwd` で決まります。
- `entrypoint` が `sdk-cli` のセッション（SDK 経由でプログラムから起動したもの）は既定で非表示です。サイドバーの「SDK 実行も表示」で切り替えます。

### 数値の出どころ

| 項目 | 出どころ |
|---|---|
| 稼働 | `system` の `turn_duration`（ターン終了時刻と所要時間）。記録がないログ（SDK 実行、古いバージョン）は、プロンプトから次のプロンプト直前の最後のメッセージまでを 1 ターンとして推定し、「推定」と表示する |
| コスト | セッション総額は `cost-state` の値（API 料金換算）。ブロックへの割り振りは、モデルごとにトークン量（入力 1、出力 5、キャッシュ読込 0.1、キャッシュ作成 1.25、1 時間キャッシュ作成 2 の重み）に比例させた推定 |
| コスト（`cost-state` がないセッション） | 実行中のセッションなど。ほかのセッションから求めたモデルごとの単価 × トークン量で推定し、「推定」と表示する |
| 変更行 | Edit、Write、NotebookEdit の結果に含まれる patch の行数。Claude Code の `totalLinesAdded` / `totalLinesRemoved` と同じ数え方 |
| PR | `pr-link` |
| ツール、モデル、スキル、MCP、編集ファイル | assistant メッセージの `tool_use`、`model`、`attributionSkill`、`attributionMcpServer`、ツール結果のファイルパス |

- サブエージェントのログ（`<session>/subagents/*.jsonl`）は、トークン、ツール、編集ファイル、変更行を親セッションのブロックに加えます。メッセージ数やブロックは増やしません。
- 日計とサイドバーの「稼働」は、Claude が動いていた時間の合計です。並行して動いたセッションはそれぞれ数えるので、1 日で 24 時間を超えることがあります。日をまたぐ区間は日ごとに分けて数えます。コスト、プロンプト、変更行、PR はブロックの開始日に数えます。

### その他

- 予定をクリックすると、そのセッションを再開する `claude --resume` コマンドを表示します。
- 詳細パネルの「ログを開く」で、セッションログを Monaco Editor（jsDelivr から初回だけ読み込み）で表示します。クリックしたブロックの最初の記録の位置から開きます。整形表示は全記録を 1 つの JSON 配列にまとめるので、記録ごとにたたむことができます。15MB を超えるログは JSONL のまま表示します。サブエージェントのログも切り替えて見られます。
- サーバーは Host ヘッダーが `127.0.0.1`、`localhost`、`::1` のリクエストにだけ応答します。DNS リバインディングで外部のページからログを読まれないようにするためです。
- 画面は 60 秒ごとに再取得します。サーバーはサイズか mtime が変わったログだけを読み直します。

FullCalendar と Monaco Editor は jsDelivr から読み込みます。
