# agents-session-manager — work in progress

Status doc. Last updated: 2026-08-18.

## What this is

A Go TUI (Bubble Tea) that lists the chat sessions of every supported agent
CLI on a machine and repairs sessions whose project folder has moved, so the
agents' own resume commands find them again.

- Claude Code: `claude --resume`
- Codex CLI: `codex resume <id>`
- Grok CLI: `grok --resume <id>`
- Google Antigravity CLI (`agy`): `agy --conversation <id>`

A session is **orphaned** when the project path recorded in its transcript no
longer exists on disk.

## Done — v1 (claude / codex / grok)

### Storage formats discovered (verified on this machine)

| Agent | Storage | Path encoding |
|---|---|---|
| Claude | `~/.claude/projects/<enc-cwd>/<uuid>.jsonl` | cwd sanitized into dir name; **every non-alphanumeric char → `-`** (verified by running real `claude` in `/tmp/enc.probe_x` → dir `-tmp-enc-probe-x`) |
| Codex | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` | none on disk; `cwd` inside `session_meta` record (line 1) |
| Grok | `~/.grok/sessions/<url-encoded-cwd>/<session-uuid>/` | URL-encoded cwd (`/root/configs` → `%2Froot%2Fconfigs`); also `session_search.sqlite` (`session_docs` table with `cwd` column; FTS index does **not** cover cwd) |

Extra per-agent artifacts that must move with the project:
- Claude project dirs also hold `memory/` (project memory) and per-session
  sidecar dirs `<uuid>/tool-results/`.
- Grok project dirs also hold `prompt_history.jsonl`.

### Remap mechanics

- **Claude**: rewrite `"cwd": "<old>"` fields in each transcript (exact JSON
  value match, formatting + mtime preserved), move transcripts + `memory/` +
  sidecar dirs into the dir encoded for the new path, remove old dir if empty.
  Conflict check if target dir already has a same-named entry.
- **Codex**: rewrite `cwd` in place in the rollout file (file location is
  date-based and unrelated to the project).
- **Grok**: move session dirs + leftovers to the newly encoded dir, rewrite
  `summary.json` cwd, `UPDATE session_docs SET cwd` in sqlite. Backup of the
  sqlite file taken once per plan.
- **Grok guard**: sessions listed in `~/.grok/active_sessions.json` (schema:
  session_id, pid, cwd, opened_at) are refused by remap/delete.

### Safety model

1. Preview of every planned action before anything touches disk (TUI preview
   mode and `remap` subcommand print the plan).
2. Before each mutation the affected artifact is copied to
   `~/.agents-session-manager/backups/<timestamp>-<agent>/` (mirrors path
   under $HOME). Deletes are moves into `<backup>/archived/` — never `rm`.
3. Exact-match rewrites: `/a/b` never clobbers `/a/b10`; regex
   `("cwd"\s*:\s*)<json-escaped old>` handles both compact and pretty JSON.
4. No automatic rollback; backups make manual recovery possible.

### Features

- Unified session table (agent, orphan flag, updated, msgs, model, project,
  title), orphans sort first. Parallel scan per agent (4 workers each).
- Filters: `/` text search (title/id/cwd/model), `tab` agent filter, `o`
  orphans-only. Detail pane (`enter`/`d`).
- Remap: `m` on any session remaps the whole project group (all sessions
  sharing its Kind+Cwd) → path input → preview → `y`.
- Delete: `x` → confirm → archived to backup dir.
- Resume handoff: `r` suspends the TUI via `tea.ExecProcess` and runs the
  agent's resume command with `Dir` = session cwd; TUI returns on exit and
  rescans. Orphaned sessions refuse resume.
- Headless modes for scripting/testing: `scan [--json] [--orphans]` and
  `remap --agent <k> --from <old> --to <new> [--yes]`.
- Env overrides respected: `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GROK_HOME`.

### Verification

- Unit tests: encoding, rewrite edge cases (escaping, prefix safety, pretty
  JSON, mtime preservation), backup/apply, per-agent scan + remap + delete on
  synthetic fixtures, UI model tests (filters, remap flow, delete flow,
  resume refusal, view rendering at several sizes).
- **E2E on real data**: `internal/agents/e2e_real_test.go` copies this
  machine's real ~/.claude, ~/.codex, ~/.grok (~77 MB) into a temp sandbox
  (via env overrides), simulates a folder move, remaps all three agents
  through the production code paths, verifies sessions come back healthy,
  sidecar/memory/prompt-history moved, sqlite rows updated, old dirs removed,
  backups present. Skips on machines without agent data.
- Headless `remap` CLI exercised on a sandboxed copy of real Claude data
  (6 sessions migrated, `-root` dir fully removed, mtimes preserved).
- TUI smoke-tested on a real pty (`script` with stty 140x40): renders header
  / table / footer, exits cleanly on `q`.

### Layout

```
main.go                     # TUI entry + scan/remap headless subcommands
internal/model/             # Session type
internal/agents/            # claude.go codex.go grok.go agy.go qwen.go muse.go + guard.go/proc.go
internal/migrate/           # Plan/Action types + Apply engine (backup+execute)
internal/ui/                # Bubble Tea model (ui.go) + rendering (view.go)
```

Binary: `./agents-session-manager` (no args = TUI).

## Done — extra homes for relocatable agents (2026-08-19)

Probed CLIs. Official relocatable homes:

- claude: `CLAUDE_CONFIG_DIR` → extra via `--claude-dir` / `config add-dir claude`
- codex: `CODEX_HOME`
- grok: `GROK_HOME`
- qwen: `QWEN_HOME`
- muse: `XDG_DATA_HOME/muse` (and `MUSE_HOME`)
- agy: **no** official relocate of `~/.gemini` — extras not offered

TUI `a` adds an extra home for the **selected agent tab**. Locks bind
via the matching env var / Claude `--json-path` / open fds. Unbound
pids lock only the default home. Resume sets the matching env.

---

## Done — v1.2 (2026-08-18)

### Qwen Code

Storage: `~/.qwen/projects/<claude-style-enc>/chats/<uuid>.jsonl` plus
`.runtime.json` (`pid`, `work_dir`). Env: `QWEN_HOME`.
Resume: `qwen --resume <id>`.

Remap: rewrite `cwd` and `work_dir`, move chats + `memory/` into the
encoded new project dir. `file-history/<id>/` is session-keyed, left put.
Live `.runtime.json` pid blocks remap/delete of that session.

Process lock: comm `qwen` (rarely used) **or** cmdline contains `qwen-code`
(real writers are `node …/qwen-code/lib/cli.js`).

### Muse Code

Storage: `~/.local/share/muse/sessions/YYYY/MM/DD/<uuid>/session.jsonl`
plus `session-index.db` (`workspace_root`, `workspace_key`). Config:
`~/.config/muse/trust.json`. Env: `MUSE_HOME` / `MUSE_CONFIG`, else
`XDG_DATA_HOME` / `XDG_CONFIG_HOME`. Resume: `muse resume <id>`.

Remap: rewrite `workspace_root` and `cwd` in the jsonl (dirs stay
date-based), update the two sqlite columns, move the `trust.json` project
key. Flocked `.session.lock` (content `pid=N`) blocks that session.

Process lock: comm `muse` or prefix `muse-bin-*` (Linux comm is truncated
to `muse-bin-0.2.1-`). The bash launcher is `comm=bash` and is ignored.

---

## Done — v1.1 (2026-08-18)

### Task 1: Google Antigravity CLI (`agy`)

Implemented. `Discover` now includes `NewAgy`; env override is `GEMINI_HOME`
(points at `~/.gemini`). Resume command: `agy --conversation <id>`.

Remap mechanics (conversation dbs are UUID-keyed and stay put):
- `SQLiteSetWorkspace` rewrites `file://<old>` → `file://<new>` inside
  `conversation_summaries.workspace_uris` (exact JSON value; prefix-safe).
- `ProjectsJSONRemap` moves the `projects.json` map key.
- `history.jsonl` `"workspace"` is rewritten (needed: 3/5 conversations on
  this machine have no summaries row, so history is the cwd source).
- Conversation-db protobuf blobs are **not** rewritten (a length change
  would corrupt them). They are not used as `Session.Cwd`.
- Scan lists conversation dbs **and** summaries-only rows (both directions).
- Delete archives the `.db`, `brain/<id>/`, and presence lock, then
  deletes the summaries row.
- Per-conversation guard: flocked `presence/<id>.lock` refuses remap/delete.

### Task 2: running-agent guard

Implemented as **per-agent read-only**, not a blank screen (confirmed
2026-08-18). Detection is the union of:

- comm-matched `/proc/<pid>/comm` ∈ {claude, codex, grok, agy}
- live activity markers (pid liveness-checked):
  - grok: `~/.grok/active_sessions.json`
  - agy: flocked `presence/<id>.lock` (+ `/proc/locks` for the holder pid)
  - claude: `daemon.status.json` supervisor/workers + `daemon.lock` pid
  - codex: process scan only

TUI always starts and scans. Locked-agent sessions are marked `L` and
dimmed; a banner names agent + PID(s). Remap/delete refused; resume
allowed. Locks re-polled every 2s. Headless `remap` refuses; `scan` does not.

---

### Investigation notes (agy 1.1.13, binary at /root/.local/bin/agy)

Investigation findings (agy 1.1.13, binary at /root/.local/bin/agy):

- Storage root: `~/.gemini/antigravity-cli/`.
- **`conversations/<conversation-uuid>.db`** — one SQLite db per
  conversation. Sampled dbs contain only a `trajectory_meta` table
  (trajectory_id, cascade_id, trajectory_type, source); the bulk content
  appears to live under `brain/<conversation-uuid>/` (dirs exist per
  conversation; not yet inspected in depth).
- **`conversation_summaries.db`** — table `conversation_summaries` with
  columns: conversation_id, title, preview, step_count, last_modified_time,
  **workspace_uris** (JSON array of file:// URIs, e.g. `["file:///root"]`),
  status, source, project_id (e.g. `default-cli-project`), agent_name,
  parent_conversation_id, nesting_depth, battle_id, winning_conversation_id,
  not_fully_idle, killed, last_user_input_time, last_user_input_step_index,
  app_data_dir.
- **`~/.gemini/projects.json`** — project registry: `{"projects": {"/root": "root"}}`
  (absolute path → project name).
- **`history.jsonl`** — prompt history entries carry `"workspace":"/root"`
  and optional conversationId.
- **`presence/<conversation-id>.lock`** — per-conversation lock files that
  agy **flocks while the conversation is live** (verified: flock test showed
  the lock for conversation 98e76761 HELD by the running agy, others not).
  This is a reliable per-conversation "active" marker.
- Resume: `agy --conversation <id>` / `agy --continue`; also `--project`.
- No `-wal`/`-shm` files currently present next to the conversation dbs.

Remap mechanics: implemented (see Done — v1.1 above). History workspace
is rewritten; conversation-db protobuf blobs are not.

#### Probe output (2026-08-18, real machine)

`~/.gemini/projects.json`:

```json
{
  "projects": {
    "/root": "root"
  }
}
```

`~/.gemini/antigravity-cli/` top level:

```
antigravity-oauth-token  bin  brain  builtin  cache  cli.log  conversations
conversation_summaries.db  crashes  history.jsonl  implicit  installation_id
jetski_state.pbtxt  keybindings.json  knowledge  last_check.timestamp  log
presence  settings.json  updater
```

`conversations/` (one sqlite db per conversation, sizes):

```
-rw-r--r--  970752 Aug 14 11:35 7ae4bcb4-06b0-4845-ae7f-9aad00141ecb.db
-rw-r--r--  864256 Aug 18 11:48 98e76761-f2c1-4b16-987e-ebc17cd407e5.db
-rw-r--r--   49152 Aug  1 01:56 a40b6baf-877e-4cac-8635-8e241c2e158d.db
-rw-r--r--  479232 Jul 25 05:41 bca1d2fc-6baf-49f5-b367-87268590b64a.db
-rw-r--r--   49152 Aug  7 00:17 fab8ccf6-2deb-450a-b8df-f72e9445e4b7.db
```

No `-wal`/`-shm` files alongside them. `brain/` contains one dir per
conversation id (same 5 ids).

`conversation_summaries.db` schema + all rows at probe time:

```
conversation_summaries: ['conversation_id', 'title', 'preview', 'step_count',
  'last_modified_time', 'workspace_uris', 'status', 'source', 'project_id',
  'agent_name', 'parent_conversation_id', 'nesting_depth', 'battle_id',
  'winning_conversation_id', 'not_fully_idle', 'killed',
  'last_user_input_time', 'last_user_input_step_index', 'app_data_dir']
 rows: 2
 ('bca1d2fc-6baf-49f5-b367-87268590b64a', '', 'Install OpenSSH Server', 38,
  '2026-07-25 00:10:45.782732137+00:00', '["file:///root"]', '', '',
  'default-cli-project', '', '', 0, '', '', 0, 0,
  '0001-01-01 00:00:00+00:00', -1, 'antigravity-cli')
 ('a40b6baf-877e-4cac-8635-8e241c2e158d', '', '', 0,
  '0001-01-01 00:00:00+00:00', '["file:///root"]', '', '',
  'default-cli-project', '', '', 0, '', '', 0, 0,
  '0001-01-01 00:00:00+00:00', -1, 'antigravity-cli')
```

Note: 5 conversation dbs exist but only 2 summaries rows — the other 3
conversations have no summaries entry (scan must tolerate both directions).

Per-conversation db schema (same for empty and populated conversations):

```
trajectory_meta (1 rows): ['trajectory_id', 'cascade_id', 'trajectory_type', 'source']
 ('9a766323-619e-47bd-ac66-84560f43fba6',
  'bca1d2fc-6baf-49f5-b367-87268590b64a', 4, 17)
```

`history.jsonl` sample:

```json
{"display":"can you check and install openssh server on my pct 114","timestamp":1784937832567,"workspace":"/root"}
{"display":"can you also check why  am unable to run `pacman -Sy` on pct 114","timestamp":1784937982723,"workspace":"/root","conversationId":"bca1d2fc-6baf-49f5-b367-87268590b64a"}
```

`presence/` + flock probe (this is the live-conversation detector):

```
-rw------- 0 Aug 14 10:56 7ae4bcb4-06b0-4845-ae7f-9aad00141ecb.lock
-rw------- 0 Aug 16 11:32 98e76761-f2c1-4b16-987e-ebc17cd407e5.lock

7ae4bcb4-...lock -> NOT held
98e76761-...lock -> HELD (agy running with this conversation)
```

The holding process:

```
root 3282747 15.8 1.1 4950072 183836 pts/9 Sl+ Aug14 978:32 agy --dangerously-skip-permissions
```

agy relevant CLI surface (v1.1.13): `--conversation <id>` resume by id,
`--continue`/`-c` most recent, `--project <id>`, `--print` non-interactive.

### Task 2: running-agent guard (lock screen)

Requirement (user): while modifying any agent's transcripts, no agent
process may be up that could write concurrently. The app must still start,
but show a blank screen naming the running agent process(es) and PID(s),
and refuse to work on the files while they run.

Process landscape on this machine (relevant for detection rules):
- Real agent processes: `/root/.grok/bin/grok`, `/root/.local/bin/codex`,
  `claude agents`, `claude bg-pty-host`, `claude bg-spare`,
  `agy --dangerously-skip-permissions` (PID 3282747 at time of writing).
- Helper/wrapper processes that are NOT the agents themselves:
  `bash /root/.local/bin/grok-herdr`, `bash /root/.local/bin/codex-herdr`
  (wrappers), `python3 .../runtime/hud/statusline ...` (statusline helpers).
- Per-agent "active" markers found:
  - grok: `~/.grok/active_sessions.json` (authoritative; has pid + cwd).
    Probe at write time: `[{"session_id": "019ffc9b-...", "pid": 1325815,
    "cwd": "/root/configs", "opened_at": "2026-08-13T19:31:46Z"}]` — note
    the listed pid can differ from the main grok process pid (31472), so
    entries need a liveness check before being trusted.
  - agy: flock on `presence/<conv>.lock` (authoritative per conversation)
  - claude: `~/.claude/daemon.lock`, `daemon.status.json`. Probe:
    `{"supervisorPid": 3653228, "supervisorProcStart": "1742072426",
    "writtenAt": 1786856934787, "workers": {}}` — supervisor pid + workers
    map; check supervisor liveness via /proc.
  - codex: nothing useful found yet (only plugins.sync.lock) → process
    scan only.

Confirmed design decisions (user, 2026-08-18):
1. **Detection**: markers first, process scan as fallback. Implementation:
   per-agent locked = live activity marker OR comm-matched process
   (union — over-locking is acceptable, under-locking is not). comm-based
   matching on /proc/<pid>/comm ∈ {claude, codex, grok, agy} naturally
   excludes the herdr wrappers (comm=bash) and statusline helpers
   (comm=python3).
2. **Lock scope**: per-agent. A running grok only blocks grok mutations;
   claude sessions stay browsable/remappable.
3. **Locked UX**: read-only. App always starts, scans, and shows the list;
   sessions of locked agents are visually marked, banner shows agent +
   PID(s), and remap/delete for a locked agent are refused with a message.
   Lock state re-evaluated every ~2s (auto-unlock when processes exit).
   Resume handoff stays allowed (it hands control to the agent itself and
   does not mutate files).
4. Headless `remap` subcommand refuses too; `scan` stays allowed.
