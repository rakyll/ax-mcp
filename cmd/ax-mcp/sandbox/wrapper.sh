# The command of every AX task ax-mcp creates. It runs the job in its own
# process group and records the exit code.
#
# ax-mcp reaches the sandbox through `ax ssh`, whose user may not write the
# job's files or signal its processes. So the state directory is world-writable
# and ax-mcp asks for cancellation with a file, which the watcher below acts on.
set -u
ws="${AX_WORKSPACE:-$PWD}"
d="$ws/.ax/mcp"
mkdir -p "$d"
chmod 0777 "$d"
printf '%s' "$MCP_HELPER" > "$d/axmcp.py"
case "$MCP_LANG" in
  python) printf '%s' "$MCP_CODE" > "$d/job.py"; set -- python3 -u "$d/job.py" ;;
  *)      printf '%s' "$MCP_CODE" > "$d/job.sh"; set -- sh "$d/job.sh" ;;
esac
cd "$ws"
PYTHONPATH="$d" setsid "$@" > "$d/stdout" 2>&1 &
pid=$!
echo "$pid" > "$d/pid"
(
  while kill -0 "$pid" 2> /dev/null; do
    if [ -e "$d/cancel" ]; then
      touch "$d/cancelled"
      kill -TERM "-$pid"
      sleep 5
      kill -KILL "-$pid" 2> /dev/null
      break
    fi
    sleep 0.3
  done
) &
wait "$pid"
echo $? > "$d/exit.tmp"
mv "$d/exit.tmp" "$d/exit"
