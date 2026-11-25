# ax-mcp: AX sandboxes as MCP tasks

`ax-mcp` is an MCP server whose tool calls run as
[MCP tasks](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks)
(protocol 2025-11-25, SEP-1686), and every task is an [AX](../ax) task: a
sandbox on Agent Substrate.

| MCP tasks | AX |
|---|---|
| task-augmented `tools/call` → `CreateTaskResult` | `CreateTask` + `ResumeTask`, returns in ~20 ms |
| `taskId` | the AX task name (`mcp-xxxxxxxx`), labeled `mcp.ax.io/owner=ax-mcp` |
| `status` / `statusMessage` | the actor's state, then what the job reports (`axmcp.status`) |
| `input_required` + `elicitation/create` | the job calls `axmcp.ask(...)` inside the sandbox and blocks until the user answers |
| `tasks/result` | the job's `$AX_OUTPUT_FILE` (or stdout) once it exits; `isError` on non-zero exit |
| `tasks/cancel` | SIGTERMs the job's process group in the sandbox (SIGKILL 5s later); whatever the job wrote by then is the result |
| `tasks/list` | `ListTasks` filtered by label |
| `ttl` | label `mcp.ax.io/ttl-ms`; ax-mcp deletes the AX task when it expires |
| `notifications/tasks/status` | sent whenever a polled task changes |

**ax-mcp keeps no state.** Everything about a task lives in AX (labels, actor
state, files under `$AX_WORKSPACE/.ax/mcp`), so you can kill ax-mcp mid-flight,
start a new one, and `tasks/list`, `tasks/get` and `tasks/result` carry on.
The demo does exactly that.

## Run the demo

You need a running AX cluster (`ax local start`) and the `ax` CLI on `PATH`.

```bash
make demo          # interactive: you answer the forecast job's question
make demo-auto     # answers the elicitation itself
```

The demo client (`cmd/ax-mcp-demo`) starts `ax-mcp` over stdio and:

1. `initialize`s and shows `capabilities.tasks`;
2. `tools/list`s, showing `sandbox_run` with `execution.taskSupport: "optional"`;
3. plays a data science agent asked *"why did Q3 revenue dip, and what's next?"*, which fans out
   five task-augmented `tools/call`s. Each returns a `CreateTaskResult` in milliseconds. The jobs are
   standard-library Python on seeded synthetic data:
   - **profile**: loads 1.5M orders into SQLite, profiles every column, and finds the change point
     behind the dip (APAC app orders −60% from Aug 14), streaming progress as `statusMessage`
   - **retention**: trains a logistic regression churn model on 200k customers and reports validation AUC
     per epoch, then writes `churn_scores.csv`
   - **forecast**: finds 37 huge bulk orders and asks the user from inside the sandbox whether to
     drop, clip or keep them (`input_required`). The client calls `tasks/result`, the server sends a
     task-tied `elicitation/create`, and the answer is written back into the sandbox. Then the job
     fits a cross-validated Holt-Winters forecast for the next four weeks
   - **tune**: a hyperparameter grid search whose ETA is over an hour; the agent stops it with
     `tasks/cancel`, and the job catches SIGTERM and returns the best trial so far as its result
   - **etl**: loads a 500k-row payments export, quarantining the rows with an `N/A` amount
4. draws a live board (a [Bubble Tea](https://github.com/charmbracelet/bubbletea) UI) from
   `notifications/tasks/status` and `tasks/get` polling, answers forecast's elicitation in a form, and
   **SIGKILLs ax-mcp mid-flight**, starts a fresh one, and recovers every task with `tasks/list`;
5. collects every result with `tasks/result`;
6. deletes the sandboxes (`-keep` leaves them for `ax get tasks` / `ax ssh`).

Flags: `-trace frames.jsonl` logs every JSON-RPC frame, `-no-restart` skips the
kill, and `-server` points at another `ax-mcp` binary. The server's own log goes to
`$TMPDIR/ax-mcp-demo-server.log`.

## Use it from an MCP client

```bash
make build
claude mcp add ax-sandbox -- $PWD/bin/ax-mcp
```

Clients without task support call `sandbox_run` normally and get the result
when the job ends. It's the same AX task underneath.

`ax-mcp` flags: `-image` (`$AX_MCP_IMAGE`, a digest-pinned image with
`python3`; defaults to the runner image in an `ax local` cluster's registry),
`-atespace`, `-context`.

## Writing jobs

Python jobs can `import axmcp`:

```python
import axmcp
axmcp.status("halfway there")                  # statusMessage
answer = axmcp.ask("Proceed?", {"type": "object",
    "properties": {"ok": {"type": "boolean"}}}) # input_required → elicitation
axmcp.result("done")                           # the tool result text
```

Shell jobs can write the status file directly:
`echo "working" > "${AX_WORKSPACE:-$PWD}/.ax/mcp/status"`.

## How it works

```
MCP client ──stdio── ax-mcp ──gRPC──▶ ax-server ──▶ Substrate actor (sandbox)
                        │                               │
                        └──── ax ssh (guest services) ──┘  probe .ax/mcp/{status,ask.json,exit}
                                                           write .ax/mcp/{answer.json,cancel}
```

- `cmd/ax-mcp`: the server. `server.go` holds the MCP and tasks logic, `backend.go` talks to AX,
  and `sandbox/` holds the job wrapper, the `axmcp` helper, and the probe that runs inside each sandbox.
- `cmd/ax-mcp-demo`: the scripted client and its jobs.
- `internal/jsonrpc`, `internal/mcp`: a small bidirectional JSON-RPC peer and the protocol types.
  The Go MCP SDK does not implement tasks yet, so the protocol is spoken directly.

Notes:
- AX doesn't record a command's exit status yet (`docs/runner.md`), so the job wrapper writes
  it to `.ax/mcp/exit` and ax-mcp polls it through `ax ssh`.
- `ax ssh` runs as a user that can't write the job's files or signal its processes. The
  wrapper therefore makes `.ax/mcp` world-writable, and cancellation is a `cancel` file
  that the wrapper acts on.
- The runner doesn't export `AX_WORKSPACE` yet, so the scripts use `${AX_WORKSPACE:-$PWD}`.
  Both the command and `ax ssh` start in the workspace.
