"""axmcp: talk to the MCP client from inside an AX task.

ax-mcp puts this module on PYTHONPATH of every job. It reads and writes small
files under $AX_WORKSPACE/.ax/mcp, which ax-mcp polls through `ax ssh`:

  status       statusMessage of the MCP task      (axmcp.status)
  ask.json     a question for the user            (axmcp.ask)
  answer.json  the user's answer, written by ax-mcp
  exit         the job's exit code, written by the wrapper

The result of the tool call is $AX_OUTPUT_FILE (axmcp.result), or the job's
stdout if it never writes one.
"""

import json
import os
import time

_WS = os.environ.get("AX_WORKSPACE") or os.getcwd()
_DIR = os.path.join(_WS, ".ax", "mcp")
_asks = 0


def _write(path, data):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write(data)
    os.replace(tmp, path)


def status(message):
    """Set the statusMessage clients see in tasks/get and notifications."""
    _write(os.path.join(_DIR, "status"), str(message))


def result(text):
    """Set the text of the tool result."""
    _write(os.environ.get("AX_OUTPUT_FILE") or os.path.join(_WS, ".ax", "output"), str(text))


def ask(message, schema):
    """Ask the user through MCP elicitation and block until they answer.

    The task goes to input_required until the client answers. Returns the
    elicitation result: {"action": "accept"|"decline"|"cancel", "content": {...}}.
    """
    global _asks
    _asks += 1
    ask_path = os.path.join(_DIR, "ask.json")
    answer_path = os.path.join(_DIR, "answer.json")
    _write(ask_path, json.dumps({"id": _asks, "message": message, "requestedSchema": schema}))
    while True:
        try:
            with open(answer_path) as f:
                answer = json.load(f)
            if answer.get("id") == _asks:
                break
        except (FileNotFoundError, ValueError):
            pass
        time.sleep(0.2)
    os.remove(ask_path)
    os.remove(answer_path)
    return answer.get("result", {})
