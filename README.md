# ai-working-calendar

[![go](https://github.com/sters/ai-working-calendar/workflows/Go/badge.svg)](https://github.com/sters/ai-working-calendar/actions?query=workflow%3AGo)
[![coverage](docs/coverage.svg)](https://github.com/sters/ai-working-calendar)
[![go-report](https://goreportcard.com/badge/github.com/sters/ai-working-calendar)](https://goreportcard.com/report/github.com/sters/ai-working-calendar)

A localhost-only web tool that reads Claude Code session logs (`~/.claude/projects/*/*.jsonl`) and shows the time you spent working as a calendar.

## Install

```shell
go install github.com/sters/ai-working-calendar@latest
```

## Usage

```shell
ai-working-calendar
# open http://127.0.0.1:8137
```

| Flag | Default | Description |
|---|---|---|
| `-addr` | `127.0.0.1:8137` | Address to listen on. Logs contain prompts and code, so do not bind to anything other than a loopback address |
| `-dir` | `~/.claude/projects` | Directory of session logs |
| `-gap` | `30m` | Idle time that splits one session into separate events |
| `-cache` | `<UserCacheDir>/ai-working-calendar/index.json` | Cache of the aggregated results. An empty string disables it |

## What is shown

The UI is in Japanese. Labels are quoted below as they appear on screen.

### Events

- Each session log is split into several events (work blocks) wherever the gap between messages exceeds `-gap`. A session resumed the next day with `--resume` becomes one event per stretch of work.
- Switching the sidebar to 「稼働区間」 (active spans) shows only the turns in which Claude was actually running. Turns that follow each other within 5 minutes are merged into one.
- An event's title is the `ai-title`, otherwise the first prompt, otherwise `last-prompt`. Its colour is determined by the session's `cwd` at start.
- Sessions whose `entrypoint` is `sdk-cli` (started programmatically through the SDK) are hidden by default. Toggle them with 「SDK 実行も表示」 (also show SDK runs) in the sidebar.

### Where the numbers come from

| Item | Source |
|---|---|
| Active time | `turn_duration` in `system` records (turn end time and duration). For logs without it (SDK runs, older versions), a turn is estimated as running from a prompt to the last message before the next prompt, and is marked 「推定」 (estimated) |
| Cost | The session total is the `cost-state` value (API-price equivalent). It is split across blocks in proportion to token volume per model, weighted input 1, output 5, cache read 0.1, cache write 1.25, 1-hour cache write 2. The split is an estimate |
| Cost (sessions without `cost-state`) | Such as sessions still running. Estimated as per-model unit price, derived from other sessions, × token volume, and marked 「推定」 (estimated) |
| Changed lines | Line counts of the patches in Edit, Write and NotebookEdit results. Counted the same way as Claude Code's `totalLinesAdded` / `totalLinesRemoved` |
| PR | `pr-link` |
| Tools, models, skills, MCP, edited files | `tool_use`, `model`, `attributionSkill` and `attributionMcpServer` in assistant messages, and file paths in tool results |

- Subagent logs (`<session>/subagents/*.jsonl`) add their tokens, tools, edited files and changed lines to the parent session's block. They do not add messages or blocks.
- The daily total and the sidebar's 「稼働」 (active) figure are the sum of time Claude was running. Sessions running in parallel are each counted, so a day can exceed 24 hours. Spans crossing midnight are counted per day. Cost, prompts, changed lines and PRs are counted on the day the block starts.

### Other

- Clicking an event shows the `claude --resume` command that resumes that session.
- 「ログを開く」 (open log) in the detail panel shows the session log in Monaco Editor, loaded from jsDelivr on first use. It opens at the first record of the clicked block. The pretty-printed view wraps all records in a single JSON array so each record can be folded. Logs larger than 15MB are shown as raw JSONL. Subagent logs can be selected as well.
- The server only responds to requests whose Host header is `127.0.0.1`, `localhost` or `::1`. This keeps external pages from reading the logs through DNS rebinding.
- The page refetches every 60 seconds. The server rereads only logs whose size or mtime has changed.

FullCalendar and Monaco Editor are loaded from jsDelivr.
