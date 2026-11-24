# aopm — Agent Orchestration Process Manager

[English](#english) | [日本語](#日本語)

## English

A single-binary [MCP](https://modelcontextprotocol.io/) server (Go, standard library only) that runs coding-agent CLIs as sub-agents on your own accounts. Pairs well with [mcpm](https://github.com/noir-chat-9661/mcpm).

```bash
go build -o ~/.local/bin/aopm .
mcpm add aopm -- ~/.local/bin/aopm
```

Requires Go 1.25+, macOS or Linux, and the CLIs you want to use.

| Backend | CLI | Isolation |
| --- | --- | --- |
| `antigravity_*` | `agy` | Edits agent-enforced; shell OS-sandboxed in `workspace-write`, denied in `read-only`; MCP always denied. Verified on agy 1.5.15 / macOS only |
| `codex_*` | `codex` | OS sandbox, but MCP servers in your codex config run outside it |
| `copilot_*` | `copilot` | Agent-enforced; `workspace-write` can use MCP servers in your config |
| `claude_*` | `claude` | Agent-enforced |
| `opencode_*` | `opencode` 1.x | Agent-enforced; `workspace-write` can use MCP servers in your config |

Each backend has `_ask` (new session), `_continue` (resume the session last used in that workspace) and `_status` (no quota).

- `prompt` and `workspace` (absolute, existing directory) are required. Optional: `model`, `timeout_s` (default 600), `sandbox`, `effort` (not opencode), and `plan` / `schema` for antigravity.
- `sandbox` defaults to `read-only`. `_continue` inherits the session's sandbox unless overridden. antigravity runs no shell commands in `read-only`, so include any diff in the prompt. In `workspace-write` its shell can write only the workspace and temp directories, with no network and no `git commit`; a denied action ends the run, and `_continue` resumes it.
- Session IDs are stored in `${XDG_STATE_HOME:-~/.local/state}/aopm`, so `_continue` survives restarts.
- Set `AGY_BIN`, `CODEX_BIN`, `COPILOT_BIN`, `CLAUDE_BIN` or `OPENCODE_BIN` to override a binary path.

Every backend is an autonomous agent: use trusted prompts on trusted content only.

## 日本語

コーディングエージェントのCLIを、自分の契約枠でサブエージェントとして呼び出すMCPサーバーです。Goの標準ライブラリのみで書かれた単一バイナリで、[mcpm](https://github.com/noir-chat-9661/mcpm)との併用を想定しています。

```bash
go build -o ~/.local/bin/aopm .
mcpm add aopm -- ~/.local/bin/aopm
```

各バックエンドに `_ask`（新規）、`_continue`（同じworkspaceの直近セッションを再開）、`_status`（利用枠を消費しない診断）があります。対応バックエンドと隔離方式は上の表の通りです。

- `prompt` と `workspace`（実在する絶対パス）は必須です。任意引数は `model`、`timeout_s`（既定600秒）、`sandbox`、`effort`（opencode以外）で、antigravityには `plan` と `schema` もあります。
- `sandbox` の既定は `read-only` です（`danger-full-access` は指定できません）。`_continue` は、指定がなければセッションのsandboxを引き継ぎます。
- セッションIDは `${XDG_STATE_HOME:-~/.local/state}/aopm` に保存され、再起動後も `_continue` できます。
- antigravityのファイル編集の制限はagyの権限設定によるもので、シェルだけが `workspace-write` でOSサンドボックスに入ります。`read-only` ではシェルを使えないため、差分などはプロンプトに含めてください。`workspace-write` のシェルが書き込めるのはworkspaceと一時ディレクトリだけで、ネットワークと `git commit` は使えません。拒否されるとその場で終わり、`_continue` で再開できます（agy 1.5.15 / macOSでのみ確認）。
- codexはOSサンドボックスで動きますが、設定済みのMCPサーバーはその外で動きます。信頼できるプロンプトと内容にだけ使ってください。

## License

[MIT License](./LICENSE)
