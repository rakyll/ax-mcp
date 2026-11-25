# Records docs/demo.gif: `vhs docs/demo.tape` (needs a running AX cluster).
Output docs/demo.gif

Set Shell "bash"
Set FontSize 14
Set Width 1400
Set Height 900
Set Padding 16
Set TypingSpeed 40ms

Hide
Type "make build >/dev/null && clear"
Enter
Sleep 3s
Show

Type "./bin/ax-mcp-demo -auto"
Sleep 500ms
Enter
Wait+Screen@120s /clean up/
Sleep 6s
