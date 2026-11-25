import json, os

ws = os.environ.get("AX_WORKSPACE") or os.getcwd()
d = os.path.join(ws, ".ax", "mcp")


def read(path, tail=None):
    try:
        with open(path, errors="replace") as f:
            s = f.read()
        return s[-tail:] if tail else s
    except (FileNotFoundError, NotADirectoryError):
        return None


def answered():
    # The id of an answer the job hasn't picked up yet.
    try:
        return json.loads(read(d + "/answer.json") or "null")["id"]
    except (ValueError, TypeError, KeyError):
        return None


st = {
    "status": read(d + "/status"),
    "ask": read(d + "/ask.json"),
    "answered": answered(),
    "exit": read(d + "/exit"),
    "cancelRequested": os.path.exists(d + "/cancel"),
    "cancelled": os.path.exists(d + "/cancelled"),  # the job got SIGTERM
}
if st["exit"] is not None:
    st["output"] = read(os.environ.get("AX_OUTPUT_FILE") or os.path.join(ws, ".ax", "output"), 65536)
    st["stdout"] = read(d + "/stdout", 65536)
print(json.dumps(st))
