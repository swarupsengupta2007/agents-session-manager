# agents-session-manager

A TUI (Go, Bubble Tea) that lists the chat sessions of every supported
agent CLI on a machine — **Claude Code**, **Codex CLI**, **Grok CLI**,
**Antigravity CLI (`agy`)**, **Qwen Code**, **Muse Code** — and repairs
sessions whose project folder has moved, so the agents' own resume
commands find them again.

## The problem it solves

These agents key their session storage to the project path. Move a project
from `/path/to/dir1` to `/path/to/dir2` and the transcripts still point at
the old path: `claude --resume`, `codex resume`, `grok --resume`,
`agy --conversation`, `qwen --resume` and `muse resume` no longer show
those sessions. This tool detects such **orphaned** sessions and remaps
them to the new path.

| Agent | Storage | Path encoding | What a remap does |
|---|---|---|---|
| Claude Code | `~/.claude/projects/<dir>/<uuid>.jsonl` | cwd sanitized into the dir name (every non-alphanumeric → `-`) | rewrite `cwd` fields in transcripts, move them (plus `memory/` and per-session sidecar dirs) into the dir encoded for the new path |
| Codex CLI | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` | none on disk; `cwd` lives in the `session_meta` record | rewrite the `cwd` field in place |
| Grok CLI | `~/.grok/sessions/<url-encoded-cwd>/<session-id>/` | URL-encoded cwd | move session dirs (+ `prompt_history.jsonl`), rewrite `summary.json`, update the `session_docs.cwd` rows in `session_search.sqlite` |
| Antigravity CLI (`agy`) | `~/.gemini/antigravity-cli/conversations/<uuid>.db` + `conversation_summaries.db` | none on disk; workspace is a `file://` URI | rewrite `workspace_uris`, `history.jsonl` workspace, and the `~/.gemini/projects.json` key. Conversation dbs stay put (they are keyed by UUID). |
| Qwen Code | `~/.qwen/projects/<dir>/chats/<uuid>.jsonl` | same as Claude (every non-alphanumeric → `-`) | rewrite `cwd` and `.runtime.json` `work_dir`, move chats + `memory/` into the dir encoded for the new path |
| Muse Code | `~/.local/share/muse/sessions/YYYY/MM/DD/<uuid>/session.jsonl` | none on disk; `workspace_root` in the log and `session-index.db` | rewrite `workspace_root`/`cwd` in place, update the sqlite index, remap `~/.config/muse/trust.json` |

A session is **orphaned** when the project path recorded in its transcript
no longer exists on disk.

## Build

Requires Go ≥ 1.23. `modernc.org/sqlite` is pure Go, so the scripts
build with `CGO_ENABLED=0` and can cross-compile.

```sh
# Linux / macOS / Git Bash
./build.sh            # host OS/arch → dist/<os>-<arch>/
./build.sh all        # linux, windows, macos (Intel + Apple Silicon)
./build.sh linux
./build.sh windows
./build.sh macos

# macOS only (same binaries as ./build.sh macos)
./build-macos.sh          # darwin/arm64 and darwin/amd64
./build-macos.sh arm64    # Apple Silicon
./build-macos.sh amd64    # Intel
./build-macos.sh native   # this Mac's CPU
```

```bat
REM Windows cmd
build.cmd
build.cmd all
build.cmd linux
build.cmd windows
build.cmd macos
```

Or by hand:

```sh
go build -o agents-session-manager .
```

## Usage

### TUI

```sh
./agents-session-manager
```

Keys:

| Key | Action |
|---|---|
| `↑/↓` (`j/k`), `pgup/pgdn`, `g/G` | navigate |
| `/` | search titles, IDs, paths, models |
| `tab` | cycle agent filter (all / claude / codex / grok / agy / qwen / muse) |
| `o` | show orphaned sessions only |
| `enter` / `d` | session detail pane |
| `m` | remap: applies to **all** sessions sharing the selected session's project path |
| `n` | rename the selected session (display title / session name; the GUID stays put) |
| `x` | delete (soft: artifacts are archived to the backup dir) |
| `r` | resume handoff: suspends the TUI and runs `claude --resume <id>` / `codex resume <id>` / `grok --resume <id>` / `agy --conversation <id>` / `qwen --resume <id>` / `muse resume <id>` in the session's project dir |
| `a` | add an extra Claude `CONFIG_DIR` (persisted) |
| `L` | re-detect running agents now (also polled every second) |
| `R` | rescan |
| `q` | quit |

Remap flow: select an orphaned session → `m` → type the new project path →
a **preview of every planned action** is shown → `y` applies it.

Rename flow: select a session → `n` → edit the title (prefilled) → enter
applies it. Lookup on the CLI is by GUID or by the current title.

### Headless

```sh
# list everything (also: --json, --orphans)
./agents-session-manager scan

# migrate one agent's sessions from an old path to a new one
./agents-session-manager remap --agent claude --from /path/to/dir1 --to /path/to/dir2 [--yes]

# rename by GUID or by the current session name
./agents-session-manager rename --agent claude --id 0664494f-279a-4d7d-a398-2c83039d6885 --to "Widget v2" --yes
./agents-session-manager rename --agent grok --name "Fix the frobnicator" --to "Frobnicator v2"
./agents-session-manager rename --agent muse --session "update" --to "Ship it" --yes
```

`remap` and `rename` print the plan and ask for confirmation unless `--yes`
is given. `--session` accepts either a GUID or a title; `--id` / `--name`
are the same selectors spelled out. A title must match exactly one session
(case-insensitive). Multiple homes for the same agent need `--store`.

## Safety model

- **Preview first** — every migration shows the full action list before
  anything touches disk.
- **Backup before every mutation** — originals are copied to
  `~/.agents-session-manager/backups/<timestamp>-<agent>/` (including the
  sqlite index) before the corresponding action runs. Deletes are *moves*
  into that backup dir, never `rm`.
- **Exact-match rewrites** — `cwd` values are rewritten only when the full
  JSON value matches (`/a/b` never clobbers `/a/b10`), preserving each
  file's formatting and mtime.
- **Active-session guard** — Grok sessions listed in
  `~/.grok/active_sessions.json` (pid still alive), agy conversations
  whose `presence/<id>.lock` is flocked, qwen `.runtime.json` pids that
  are still alive, and muse `.session.lock` files that are flocked are
  refused by remap/delete/rename.
- **Running-agent lock** — per agent. If `claude` / `codex` / `grok` /
  `agy` / `qwen` / `muse` is running (process `comm` match, including
  `muse-bin-*` prefix and `qwen-code` on a node cmdline, or a live
  activity marker), that agent's filter tab is red. Selecting the tab
  shows a short `LOCKED  PID …` line for that store only. Remap/delete/
  rename are refused. Locks are re-detected every second while the TUI
  is open (and immediately after a scan, apply, resume, or `L`), so an
  agent that starts or exits after the manager launched is reflected
  without a restart. Stale marker PIDs that now belong to some other
  process are ignored. Resume handoff is still allowed. Headless
  `remap` / `rename` refuse too; `scan` does not.
- **Copy-modify-swap writes** — every in-place mutation is applied to a
  sibling work copy of a snapshot, then swapped over the original. The
  writer probes for a live agent before the snapshot, before the swap,
  and after the swap. After the swap the on-disk bytes are compared to
  the copy we just wrote; a mismatch (or an agent appearing after the
  swap) is a **WARNING** about possible concurrent-writer corruption.
  A probe that fails *before* the swap aborts and leaves the original
  untouched.
- **Conflict detection** — remapping into a project dir that already has a
  colliding `memory/`, sidecar dir, etc. fails before applying.
- No automatic rollback: if an apply fails mid-plan, stop and inspect —
  everything overwritten or moved has a pristine copy in the backup dir.

## Environment overrides

`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GROK_HOME`, `GEMINI_HOME`,
`QWEN_HOME`, `MUSE_HOME` and `MUSE_CONFIG` are respected (used by the
test suite to run against sandboxed copies). Muse also honors
`XDG_DATA_HOME` / `XDG_CONFIG_HOME` when the dedicated overrides are unset.

### Extra agent homes

These CLIs can keep sessions outside the default path. Extra roots show
up as `agent@<basename>` tabs.

| Agent | Official override | Default |
|---|---|---|
| Claude | `CLAUDE_CONFIG_DIR` | `~/.claude` |
| Codex | `CODEX_HOME` | `~/.codex` |
| Grok | `GROK_HOME` | `~/.grok` |
| Qwen | `QWEN_HOME` | `~/.qwen` |
| Muse | `XDG_DATA_HOME/muse` (also `MUSE_HOME`) | `~/.local/share/muse` |
| Antigravity (`agy`) | none | `~/.gemini` only |

```sh
# persist — or in the TUI: select the agent tab, then `a`
./agents-session-manager config add-dir grok /path/to/alt-grok
./agents-session-manager config list
./agents-session-manager config rm-dir grok /path/to/alt-grok

# this run only
./agents-session-manager --grok-dir /path/to/alt-grok
./agents-session-manager scan --codex-dir /path/to/alt-codex
./agents-session-manager remap --agent qwen --store /path/to/alt-qwen \
    --from /old --to /new --yes
```

Settings live in `~/.agents-session-manager/config.json` (`ASM_CONFIG`
overrides the path). Resume sets the matching env var so the CLI hits
that store.

A running process only locks the home it is actually using (`CLAUDE_CONFIG_DIR`
/ `CODEX_HOME` / `GROK_HOME` / `QWEN_HOME` / `MUSE_HOME` or `XDG_DATA_HOME`
in `/proc/<pid>/environ`, Claude's `--json-path`, or open files under the
store). Unbound processes lock only the default home.

## Tests

```sh
go test ./...
```

Includes an end-to-end test that copies the machine's *real* agent data
into a temp sandbox, simulates a folder move, remaps every discovered
agent through the production code paths, and verifies the sessions come
back healthy (skips on machines without agent data). Claude's
path-encoding scheme is pinned against a directory name observed from an
actual `claude` run.

## Known limitations (v1)

- Linux-first; macOS shares the same storage paths and has darwin
  build targets, but the TUI is untested on a Mac.
- Codex transcripts keep their date-based location after a remap (only
  `cwd` changes), which matches how Codex resolves sessions.
- Grok's `events.jsonl`/`chat_history.jsonl` may mention the old path
  inside message *content*; those are conversation text, not resume state,
  and are left untouched.
- Agy conversation databases store workspace URIs inside protobuf blobs;
  those are not rewritten (a length change would corrupt the db). Resume
  listing uses `conversation_summaries.workspace_uris` and `projects.json`.
- Muse `session-index.search_text` may still mention the old path after a
  remap (picker search only). Resume-by-id uses `workspace_key`.
- Rename changes the **display title / session name**, never the UUID.
  Claude rewrites or appends an `ai-title` record; Grok updates
  `summary.json` plus the search index; Muse sets `title` and
  `session_name` (and bumps `session_name_revision`); Agy updates
  `conversation_summaries.title` and appends a history line. Codex has
  no official title field, so this tool writes a `<transcript>.title`
  sidecar that only this listing reads. Qwen writes `title` into
  `.runtime.json` (preferred by this listing; Qwen itself may still
  show the first user message).
- Resume handoff runs the agent binary found in `PATH` (or
  `~/.grok/bin/grok`); the TUI resumes when the agent exits.
