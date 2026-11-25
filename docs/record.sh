#!/bin/sh
# Records docs/demo.gif. Needs a running AX cluster with no leftover mcp-* tasks,
# asciinema and agg (brew install asciinema agg).
set -e
cd "$(dirname "$0")/.."
make build
cast=$(mktemp -t demo).cast
asciinema rec --headless --overwrite --window-size 140x50 -i 2 -q \
  -c "sh -c 'printf \"\\033[32m\$\\033[0m ./bin/ax-mcp-demo -auto\\n\"; sleep 1; ./bin/ax-mcp-demo -auto; sleep 3'" "$cast"
agg --font-size 14 --theme monokai --last-frame-duration 5 "$cast" docs/demo.gif
rm "$cast"
