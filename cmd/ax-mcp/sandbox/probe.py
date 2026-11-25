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


st = {
    "status": read(d + "/status"),
    "ask": read(d + "/ask.json"),
    "exit": read(d + "/exit"),
    "cancelled": os.path.exists(d + "/cancel") or os.path.exists(d + "/cancelled"),
}
if st["exit"] is not None or st["cancelled"]:
    st["output"] = read(os.environ.get("AX_OUTPUT_FILE") or os.path.join(ws, ".ax", "output"), 65536)
    st["stdout"] = read(d + "/stdout", 65536)
print(json.dumps(st))
