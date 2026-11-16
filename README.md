# Baton

**Switch agents. Keep the conversation.**

Baton is a local CLI that lets you continue one conversation between **Codex CLI**
and **Claude Code**, in the same working directory. Work with one agent, hand the
conversation to the other, and return with the new context carried forward.

Baton reads native session history, prepares a bounded context package, and
launches the original CLI directly in your terminal. It confirms each handoff
against the destination's transcript before recording delivery.

Supports **macOS and Linux**.

## Getting started

### Requirements

- Go 1.25 or later to build from source.
- Codex CLI and Claude Code installed and authenticated for the agents you use.
- An interactive terminal for native CLI sessions.

Baton uses each CLI's existing authentication; no separate model API key is
required by Baton.

### Build and install

```sh
git clone https://github.com/m2zyi/Baton.git
cd Baton
go build -o bin/baton ./cmd/baton
./bin/baton help
```

Install the binary in a directory on your `PATH`, for example:

```sh
mkdir -p ~/.local/bin
install -m 755 bin/baton ~/.local/bin/baton
```

Make sure `~/.local/bin` is on your `PATH`. Agents need to be able to run
`baton context <id>` to retrieve history that does not fit in a handoff package.

### Start a conversation

Run Baton from your project directory:

```sh
baton
```

The menu lets you create a Codex or Claude conversation, attach an existing native
session in the current directory, or open a conversation already managed by
Baton. After the native agent exits, you return to the menu to continue, switch
agents, inspect context, or quit.

You can also start directly:

```sh
baton start --agent codex --prompt "Explain this project's architecture."
```

Once the native session exits, hand it to Claude:

```sh
baton switch --to claude
```

If the directory has multiple managed conversations, Baton asks you to choose
rather than silently selecting the most recent one. The menu shows conversation
IDs, active agents, update times, and pending handoffs.

## Commands

```text
baton
baton start --agent codex|claude [--prompt TEXT]
baton attach --agent codex|claude --session NATIVE_SESSION_ID
baton switch --to codex|claude [--conversation BATON_ID] [--from NATIVE_SESSION_ID]
baton resume BATON_ID [--agent codex|claude]
baton list [--json]
baton context BATON_ID [--after N] [--limit N] [--json]
baton doctor
baton help
baton version
```

There are two kinds of IDs:

- **Native session IDs** belong to Codex or Claude. Use them with `--session` and
  `--from`.
- **Baton conversation IDs** are local, 32-character lowercase hexadecimal values.
  Use them with `--conversation`, `resume`, and `context`.

Options for `resume` and `context` may follow the conversation ID:

```sh
baton context 0123abcd0123abcd0123abcd0123abcd --after 20 --limit 10 --json
```

Using `resume ID --agent claude` for a conversation currently active in Codex also
supplies Claude with the context it has not received and updates the active agent.

## Configuration and local data

| Variable | Purpose |
| --- | --- |
| `BATON_HOME` | Override Baton's data directory. |
| `BATON_CODEX_BIN` | Override the Codex executable used by Baton. |
| `BATON_CLAUDE_BIN` | Override the Claude executable used by Baton. |
| `CODEX_HOME` | Locate Codex configuration and session history. |
| `CLAUDE_CONFIG_DIR` | Locate Claude configuration and project history. |

Baton chooses its data directory in this order:

1. `BATON_HOME`
2. `$XDG_DATA_HOME/baton`
3. `~/.local/share/baton`

Conversation history is stored locally. Directories use `0700` permissions and
files use `0600`. Baton does not modify either agent's global configuration,
repository rules, or source transcripts.

## How handoffs work

Baton reads the source session's history and passes a context package to the
destination agent. Short conversations are included in full; longer ones retain
the initial request and recent context, with `baton context <id>` available for
retrieving the full history. Returning agents receive only the new context they
have not already received.

History is labeled as quoted data rather than new instructions. Baton confirms
delivery against the destination transcript and keeps unfinished handoffs pending
for retry. If delivery cannot be confirmed or is ambiguous, it reports an error.

Native agents run directly in your terminal, retaining their interface and
keyboard shortcuts. Exit the native session to return to Baton and switch agents.

## Diagnostics

```sh
baton doctor
```

This checks that Baton's data directory is writable and that the Codex and
Claude executables can be found, including any `BATON_*_BIN` overrides. A failed
check returns a nonzero exit code.

## Limitations

- Conversations are local to a working directory; cross-machine synchronization
  and shared long-term memory are not supported.
- Baton relies on native CLI history formats. Unsupported or corrupt transcripts
  produce an error.
- A Codex session binding uses a fixed history backend. Switching between
  app-server and rollout history requires a new binding.

## Development and testing

```sh
gofmt -w cmd internal
go vet ./...
go test ./...
go test -race ./...
go build -o bin/baton ./cmd/baton
python3 scripts/smoke_pty.py ./bin/baton
```

The optional PTY smoke test requires Python 3. Tests use synthetic native CLIs
and isolated data directories; they do not invoke real models or read private
user sessions.
