# ax-mcp: AX tasks over MCP

`ax-mcp` is an MCP server whose tool calls run as
[MCP tasks](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks),
and every MCP task is an AX task.

![demo](docs/demo.gif)

| MCP | AX |
|---|---|
| task-augmented `tools/call` | `CreateTask` + `ResumeTask` |
| `tasks/get`, `notifications/tasks/status` | the task's state and what the job reports |
| `input_required` + `elicitation/create` | the job asks the user a question and waits |
| `tasks/result` | the job's output once it exits |
| `tasks/cancel` | SIGTERM to the job; what it writes on the way out is the result |
| `tasks/list` | `ListTasks` |

ax-mcp keeps no state: everything lives in AX, so you can kill ax-mcp
mid-flight and a new one picks up every task.

## Run the demo

You need a running AX cluster (`ax local start`) and `ax` on `PATH`.

```bash
make demo        # you answer the forecast job's question
make demo-auto   # answers it for you
```

A data science agent asked *"why did Q3 revenue dip, and what's next?"* starts
five jobs as MCP tasks:

- **profile** finds the cause: APAC app orders down 60% from Aug 14
- **retention** trains a churn model on 200k customers
- **forecast** asks whether to drop, clip or keep 37 bulk orders, then forecasts four weeks
- **tune** runs a grid search that would take over an hour, so the agent cancels it
- **etl** loads September's payments and quarantines two bad rows

Along the way the demo kills ax-mcp and recovers every task with `tasks/list`.
At the end it deletes the AX tasks (`-keep` leaves them).

## Use it from an MCP client

```bash
make build
claude mcp add ax-tasks -- $PWD/bin/ax-mcp
```

## Writing jobs

Python jobs can `import axmcp`:

```python
import axmcp
axmcp.status("halfway there")                       # statusMessage
answer = axmcp.ask("Proceed?", {"type": "object",
    "properties": {"ok": {"type": "boolean"}}})      # input_required → elicitation
axmcp.result("done")                                # the tool result
```

Without `axmcp.result`, the result is the job's stdout.
