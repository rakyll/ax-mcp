// Command ax-mcp-demo is an MCP client that tours the MCP Tasks extension
// (io.modelcontextprotocol/tasks) against ax-mcp: it fans out sandboxed jobs
// as tasks, answers a question one of them asks from inside its sandbox,
// cancels a runaway job, kills ax-mcp mid-flight and carries on with a new
// one, and collects the results.
//
// The tour runs in its own goroutine and drives a Bubble Tea UI (ui.go):
// finished steps print above a live board of tasks.
package main

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rakyll/ax-mcp/internal/jsonrpc"
	"github.com/rakyll/ax-mcp/internal/mcp"
)

//go:embed jobs
var jobFiles embed.FS

var jobs = []struct{ name, file, lang string }{
	{"profile", "jobs/profile.py", "python"},
	{"retention", "jobs/retention.py", "python"},
	{"forecast", "jobs/forecast.py", "python"},
	{"tune", "jobs/tune.py", "python"},
	{"etl", "jobs/etl.py", "python"},
}

// autoAnswers are what -auto types into elicitation fields without a default.
var autoAnswers = map[string]string{
	"reason": "one-off partner bulk orders, not consumer demand",
}

var (
	serverPath = flag.String("server", "", "ax-mcp binary (default: next to this binary, then $PATH)")
	auto       = flag.Bool("auto", false, "answer elicitations without asking (also when stdin is not a terminal)")
	noRestart  = flag.Bool("no-restart", false, "skip killing and restarting ax-mcp")
	keep       = flag.Bool("keep", false, "keep the AX tasks when the demo ends (ax-mcp deletes them after their TTL)")
	tracePath  = flag.String("trace", "", "write every JSON-RPC frame to this file")
)

func main() {
	flag.Parse()
	inTTY, outTTY := isTerminal(os.Stdin), isTerminal(os.Stdout)
	if !inTTY {
		*auto = true
	}
	d := &demo{start: time.Now(), byID: map[string]*row{}, plain: !outTTY, pollEvery: time.Second}
	if err := d.openLogs(); err != nil {
		fmt.Fprintf(os.Stderr, "ax-mcp-demo: %v\n", err)
		os.Exit(1)
	}

	// In a terminal, Bubble Tea reads ctrl+c as a key and the model cancels;
	// otherwise SIGINT does.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	d.cancel, d.ctx = cancel, ctx
	errc := make(chan error, 1)
	var uiErr error
	if d.plain { // no terminal to draw on: just print
		errc <- d.run(ctx)
	} else {
		var opts []tea.ProgramOption
		if !inTTY {
			opts = append(opts, tea.WithInput(nil))
		}
		d.ui = tea.NewProgram(newModel(d), opts...)
		go func() {
			errc <- d.run(ctx)
			d.stopUI()
		}()
		_, uiErr = d.ui.Run()
		d.uiDone.Store(true)
		if uiErr != nil {
			cancel()
		}
	}
	go func() {
		<-ctx.Done()
		if s := d.sess(); s != nil {
			s.stop() // unblocks run if it is mid-call
		}
	}()
	runErr := <-errc
	interrupted := ctx.Err() != nil
	if !d.finished.Load() {
		if interrupted {
			d.print("\n" + red.Render("interrupted"))
		}
		d.cleanup()
	}
	if err := cmp.Or(uiErr, runErr); err != nil && !interrupted {
		fmt.Fprintf(os.Stderr, "\n%s\n", red.Render("ax-mcp-demo: "+err.Error()))
		os.Exit(1)
	}
	if interrupted {
		os.Exit(130)
	}
}

func (d *demo) run(ctx context.Context) error {
	d.print("\n" + banner.Render(" AX × MCP Tasks ") + "\n" +
		faint.Render(`  A data science agent asked "why did Q3 revenue dip, and what's next?" fans out its analysis.`) + "\n" +
		faint.Render("  Every tool call below runs as an MCP task backed by an AX task.") + "\n" +
		faint.Render("  server log: "+d.logPath))

	d.print(step("1", "server/discover", "the server advertises the tasks extension"))
	d.setActivity("starting ax-mcp")
	disc, err := d.connect(ctx)
	if err != nil {
		return err
	}
	exts, _ := json.Marshal(disc.Capabilities["extensions"])
	d.print(frame("←", "supportedVersions %v, capabilities.extensions = %s", disc.SupportedVersions, exts))

	d.print(step("2", "tools/list", "capabilities are per request: every request declares the tasks extension"))
	d.print(frame("→", `_meta["%s"] = %s`, mcp.MetaClientCapabilities, mustJSON(clientCaps)))
	var tools mcp.ListToolsResult
	if err := d.sess().call(ctx, "tools/list", nil, &tools); err != nil {
		return err
	}
	for _, t := range tools.Tools {
		d.print(frame("←", "%s  %q", bold.Render(t.Name), t.Title))
	}

	d.print(step("3", "tools/call × 5", "the server decides to answer each with a task; the work runs in AX"))
	d.setActivity("creating tasks")
	for _, j := range jobs {
		code, _ := jobFiles.ReadFile(j.file)
		params := map[string]any{
			"name":      "sandbox_run",
			"arguments": map[string]any{"code": string(code), "language": j.lang, "name": j.name},
		}
		t0 := time.Now()
		var res mcp.ToolCallResult
		if err := d.sess().call(ctx, "tools/call", params, &res); err != nil {
			return fmt.Errorf("tools/call %s: %w", j.name, err)
		}
		if res.ResultType != mcp.ResultTask {
			return fmt.Errorf("tools/call %s: got resultType %q, not a task", j.name, res.ResultType)
		}
		d.print(frame("→", "tools/call sandbox_run {name: %s}", strconv.Quote(j.name)) + "\n" +
			frame("←", "resultType %q  %s %s %s", res.ResultType, cyan.Render(res.TaskID), res.Status, faint.Render("("+time.Since(t0).Round(time.Millisecond).String()+")")))
		d.add(&row{id: res.TaskID, job: j.name, status: res.Status, message: res.StatusMessage, asked: map[string]bool{}})
		if res.PollIntervalMs > 0 {
			d.pollEvery = time.Duration(res.PollIntervalMs) * time.Millisecond
		}
	}

	d.print(step("4", "watch", "notifications/tasks from subscriptions/listen, and tasks/get every pollIntervalMs"))
	d.setActivity("")
	d.setBoard(true)
	d.listen(ctx)
	d.watch(ctx)
	d.setBoard(false)
	d.stopUI()
	d.print(renderBoard(d.snapshot(), "", 0, !d.plain))
	if ctx.Err() != nil {
		return nil
	}

	d.print(step("5", "results", "a finished task's tasks/get carries its tool result inline"))
	for _, r := range d.rows {
		icon, style := statusIcon(r.status)
		text := lipgloss.NewStyle()
		var lines []string
		switch {
		case r.result != nil:
			lines = resultLines(r.result.Content, 5)
			if r.result.IsError {
				text = red
			}
		case r.err != nil:
			lines, text = []string{fmt.Sprintf("error %d: %s", r.err.Code, r.err.Message)}, red
		default: // cancelled: no result, just a status message
			lines = resultLines([]mcp.Content{{Text: r.message}}, 5)
		}
		var b strings.Builder
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(&b, "  %s %s", style.Render(icon), bold.Render(fmt.Sprintf("%-10s", r.job)))
			} else {
				b.WriteString("\n              ")
			}
			b.WriteString(text.Render(line))
		}
		d.print(b.String())
	}
	d.setActivity("")

	d.sess().stop()
	d.cleanup()
	d.finished.Store(true)
	return nil
}

// cleanup deletes the demo's sandboxes, unless -keep.
func (d *demo) cleanup() {
	d.print(step("6", "clean up", ""))
	if *keep {
		d.print("  Kept the AX tasks: " + bold.Render("ax get tasks") + ". ax-mcp deletes them when their TTL runs out.\n")
		return
	}
	var lines []string
	for _, r := range d.rows {
		out, err := exec.Command("ax", "delete", "task", r.id).CombinedOutput()
		style := faint
		if err != nil {
			style = red
		}
		lines = append(lines, "  "+style.Render(strings.TrimSpace(string(out))))
	}
	d.print(strings.Join(lines, "\n") + "\n")
}

func step(n, title, sub string) string {
	s := "\n" + blue.Bold(true).Render(" "+n+" ") + " " + bold.Render(title)
	if sub != "" {
		s += "  " + faint.Render(sub)
	}
	return s
}

func frame(dir, format string, args ...any) string {
	style := green
	if dir == "←" {
		style = purple
	}
	return "  " + style.Render(dir) + " " + fmt.Sprintf(format, args...)
}

func resultLines(content []mcp.Content, max int) []string {
	var text strings.Builder
	for _, c := range content {
		text.WriteString(c.Text)
	}
	lines := strings.Split(strings.TrimSpace(text.String()), "\n")
	if len(lines) > max {
		lines = append(lines[:1], append([]string{"..."}, lines[len(lines)-max+2:]...)...)
	}
	return lines
}

// session is one ax-mcp process and the MCP session with it.
type session struct {
	cmd  *exec.Cmd
	conn *jsonrpc.Conn
}

// clientCaps is what the demo declares on every request: it takes tasks, and
// answers the elicitations they carry in inputRequests.
var clientCaps = map[string]any{
	"elicitation": map[string]any{"form": map[string]any{}},
	"extensions":  map[string]any{mcp.ExtTasks: map[string]any{}},
}

// call sends a request with the per-request _meta of 2026-07-28.
func (s *session) call(ctx context.Context, method string, params map[string]any, out any) error {
	p := map[string]any{"_meta": map[string]any{
		mcp.MetaProtocolVersion:    mcp.ProtocolVersion,
		mcp.MetaClientInfo:         mcp.Implementation{Name: "ax-mcp-demo", Version: "0.1.0"},
		mcp.MetaClientCapabilities: clientCaps,
	}}
	for k, v := range params {
		p[k] = v
	}
	return s.conn.Call(ctx, method, p, out)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *session) stop() {
	s.cmd.Process.Kill()
	s.cmd.Wait()
}

type row struct {
	id, job, status, message string
	result                   *mcp.CallToolResult // once completed
	err                      *mcp.Error          // once failed
	asked                    map[string]bool     // inputRequests keys already handled
}

type demo struct {
	start   time.Time
	logPath string
	log     *os.File
	trace   *os.File
	ui      *tea.Program
	plain   bool // stdout is not a terminal: print, don't draw
	cancel  context.CancelFunc
	ctx     context.Context

	printMu  sync.Mutex
	uiDone   atomic.Bool
	finished atomic.Bool

	mu       sync.Mutex
	cur      *session
	restarts int
	rows     []*row
	byID     map[string]*row
	events   []string
	notifs   int
	polls    int
	board    bool   // the live board is showing
	activity  string // what the demo is doing when there is no board
	answered  bool
	pollEvery time.Duration
}

// stopUI ends the live UI once the board is done, so the rest prints
// straight to the terminal. (Lines printed above a view that shrinks can get
// erased.)
func (d *demo) stopUI() {
	if d.ui == nil || d.uiDone.Load() {
		return
	}
	d.ui.Send(doneMsg{})
	d.ui.Wait()
	d.uiDone.Store(true)
}

// print writes s above the live UI, where it stays.
func (d *demo) print(s string) {
	d.printMu.Lock()
	defer d.printMu.Unlock()
	if d.plain || d.uiDone.Load() {
		fmt.Println(s)
		return
	}
	d.ui.Println(s)
}

func (d *demo) openLogs() error {
	var err error
	d.logPath = filepath.Join(os.TempDir(), "ax-mcp-demo-server.log")
	if d.log, err = os.Create(d.logPath); err != nil {
		return err
	}
	if *tracePath != "" {
		if d.trace, err = os.Create(*tracePath); err != nil {
			return err
		}
	}
	return nil
}

func (d *demo) sess() *session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur
}

func (d *demo) add(r *row) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows = append(d.rows, r)
	d.byID[r.id] = r
}

func (d *demo) setBoard(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.board = on
}

func (d *demo) setActivity(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.activity = s
}

func (d *demo) jobName(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.byID[id]; ok {
		return r.job
	}
	return id
}

func (d *demo) event(format string, args ...any) {
	e := faint.Render(fmt.Sprintf("%6.1fs", time.Since(d.start).Seconds())) + "  " + fmt.Sprintf(format, args...)
	d.mu.Lock()
	d.events = append(d.events, e)
	d.mu.Unlock()
	if d.plain {
		d.print("  " + e)
	}
}

// snapshot is what the UI draws: a copy of the board's state.
type snapshot struct {
	board                   bool
	activity                string
	rows                    []row
	events                  []string
	notifs, polls, restarts int
}

func (d *demo) snapshot() snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := snapshot{board: d.board, activity: d.activity, events: slices.Clone(d.events), notifs: d.notifs, polls: d.polls, restarts: d.restarts}
	for _, r := range d.rows {
		s.rows = append(s.rows, *r)
	}
	return s
}

// connect starts ax-mcp and asks it what it supports. There is no session to
// set up: every request carries its own protocol version and capabilities.
func (d *demo) connect(ctx context.Context) (*mcp.DiscoverResult, error) {
	path := *serverPath
	if path == "" {
		if exe, err := os.Executable(); err == nil {
			if p := filepath.Join(filepath.Dir(exe), "ax-mcp"); fileExists(p) {
				path = p
			}
		}
	}
	if path == "" {
		path = "ax-mcp"
	}
	cmd := exec.Command(path)
	cmd.Stderr = d.log
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ax-mcp: %w", err)
	}
	s := &session{cmd: cmd, conn: jsonrpc.NewConn(stdout, stdin, d.handle)}
	if d.trace != nil {
		pid := cmd.Process.Pid
		s.conn.Trace = func(dir string, b []byte) {
			d.mu.Lock()
			defer d.mu.Unlock()
			fmt.Fprintf(d.trace, "%s pid=%d %s %s\n", time.Now().Format("15:04:05.000"), pid, dir, b)
		}
	}
	go s.conn.Run(ctx)

	var res mcp.DiscoverResult
	if err := s.call(ctx, "server/discover", nil, &res); err != nil {
		s.stop()
		return nil, fmt.Errorf("server/discover: %w (see %s)", err, d.logPath)
	}
	d.mu.Lock()
	d.cur = s
	d.mu.Unlock()
	return &res, nil
}

// handle serves what ax-mcp sends us: notifications on our subscription.
func (d *demo) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "notifications/subscriptions/acknowledged":
		var ack mcp.ListenAck
		if json.Unmarshal(params, &ack) == nil {
			var names []string
			for _, id := range ack.Notifications.TaskIDs {
				names = append(names, d.jobName(id))
			}
			d.event("%s notifications/tasks for %d tasks: %s", purple.Render("← subscription acknowledged:"), len(names), strings.Join(names, ", "))
		}
		return nil, nil
	case "notifications/tasks":
		var t mcp.DetailedTask
		if json.Unmarshal(params, &t) == nil {
			d.mu.Lock()
			d.notifs++
			d.mu.Unlock()
			d.apply(t)
		}
		return nil, nil
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
}

// apply records a task's state, from tasks/get or notifications/tasks, and
// takes up any input request it hasn't seen.
func (d *demo) apply(t mcp.DetailedTask) {
	d.mu.Lock()
	r, ok := d.byID[t.TaskID]
	if !ok || mcp.Terminal(r.status) {
		d.mu.Unlock()
		return
	}
	changed := r.status != t.Status
	r.status, r.message, r.result, r.err = t.Status, t.StatusMessage, t.Result, t.Error
	var keys []string
	if t.Status == mcp.StatusInputRequired {
		for key := range t.InputRequests {
			if !r.asked[key] {
				r.asked[key] = true
				keys = append(keys, key)
			}
		}
	}
	d.mu.Unlock()
	for _, key := range keys {
		go d.fulfill(r, key, t.InputRequests[key])
	}
	if changed && mcp.Terminal(t.Status) {
		icon, style := statusIcon(t.Status)
		d.event("%s %s", style.Render(icon+" "+r.job), t.Status)
	}
}

// fulfill answers one of a task's inputRequests with tasks/update.
func (d *demo) fulfill(r *row, key string, req mcp.InputRequest) {
	ctx := d.ctx
	if req.Method != "elicitation/create" {
		d.event(red.Render("%s asked for %s, which this demo can't answer"), r.job, req.Method)
		return
	}
	d.event("%s %s is input_required: inputRequests[%q] is an elicitation", purple.Render("?"), r.job, key)
	res, err := d.elicit(ctx, r, key, req.Params)
	if err != nil {
		return
	}
	err = d.sess().call(ctx, "tasks/update", map[string]any{"taskId": r.id, "inputResponses": map[string]any{key: res}}, nil)
	if err != nil {
		d.event(red.Render("tasks/update %s: %v"), r.job, err)
		return
	}
	d.event("%s %s: the answer was written into its sandbox", green.Render("→ tasks/update"), r.job)
	d.mu.Lock()
	d.answered = true
	d.mu.Unlock()
}

// elicit asks the user, through the UI's form, the question a job asked from
// inside its sandbox, and prints the exchange once it is answered.
func (d *demo) elicit(ctx context.Context, r *row, key string, p mcp.ElicitParams) (mcp.ElicitResult, error) {
	req := elicitMsg{taskID: r.id, job: r.job, key: key, params: p, reply: make(chan mcp.ElicitResult, 1)}
	var res mcp.ElicitResult
	if d.plain {
		res = mcp.ElicitResult{Action: "accept", Content: autoContent(req)}
	} else {
		d.ui.Send(req)
		select {
		case res = <-req.reply:
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}

	bar := purple.Render("│")
	lines := []string{
		"",
		"  " + purple.Bold(true).Render("┌ inputRequests["+strconv.Quote(key)+"]: elicitation/create") + " " + faint.Render("(task "+r.id+", job "+r.job+")"),
		"  " + bar + " " + bold.Render(p.Message),
	}
	props, _ := p.RequestedSchema["properties"].(map[string]any)
	for _, name := range fieldOrder(p.RequestedSchema) {
		v, ok := res.Content[name]
		if !ok {
			continue
		}
		prop, _ := props[name].(map[string]any)
		title, _ := prop["title"].(string)
		answer := fmt.Sprint(v)
		if *auto {
			answer += faint.Render(" (auto)")
		}
		lines = append(lines, "  "+bar+" "+title+": "+answer)
	}
	lines = append(lines, "  "+purple.Render("└ "+res.Action+" → tasks/update"), "")
	d.print(strings.Join(lines, "\n"))
	return res, nil
}

// fieldOrder is the order to ask for a schema's fields in: required ones
// first, then the rest by name.
func fieldOrder(schema map[string]any) []string {
	var names []string
	seen := map[string]bool{}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		if name, ok := r.(string); ok && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	props, _ := schema["properties"].(map[string]any)
	var rest []string
	for name := range props {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	return append(names, rest...)
}

// watch polls the tasks until every one is terminal, and runs the scripted
// moments of the demo: cancel, input_required, and the server restart.
func (d *demo) watch(ctx context.Context) {
	poll := time.NewTicker(d.pollEvery)
	defer poll.Stop()
	cancelled, restarted := false, *noRestart
	watchStart := time.Now()
	for {
		select {
		case <-poll.C:
		case <-ctx.Done():
			return
		}
		d.pollAll(ctx)

		d.mu.Lock()
		var tune *row
		allDone := true
		for _, r := range d.rows {
			if r.job == "tune" {
				tune = r
			}
			if !mcp.Terminal(r.status) {
				allDone = false
			}
		}
		answered := d.answered
		var tuneID, tuneMsg string
		tuneWorking := tune != nil && tune.status == mcp.StatusWorking
		if tune != nil {
			tuneID, tuneMsg = tune.id, tune.message
		}
		d.mu.Unlock()
		age := time.Since(watchStart)
		if allDone || age > 4*time.Minute {
			if !allDone {
				d.event(red.Render("gave up waiting after %s"), age.Round(time.Second))
			}
			return
		}

		if !cancelled && tuneWorking && strings.Contains(tuneMsg, "ETA") && age > 6*time.Second {
			cancelled = true
			eta := "an ETA of " + tuneMsg[strings.LastIndex(tuneMsg, "ETA")+4:]
			d.event("%s tune: the grid search reports %s, too long", green.Render("→ tasks/cancel"), eta)
			if err := d.sess().call(ctx, "tasks/cancel", map[string]any{"taskId": tuneID}, nil); err != nil {
				d.event(red.Render("tasks/cancel %s failed: %v"), tuneID, err)
			} else {
				d.event("%s cancellation is cooperative: the job gets SIGTERM", purple.Render("← acked;"))
			}
		}
		if !restarted && cancelled && (answered || age > 40*time.Second) {
			restarted = true
			d.restart(ctx)
		}
	}
}

func (d *demo) pollAll(ctx context.Context) {
	s := d.sess()
	var wg sync.WaitGroup
	for _, id := range d.pending() {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var t mcp.DetailedTask
			if s.call(cctx, "tasks/get", map[string]any{"taskId": id}, &t) == nil {
				d.mu.Lock()
				d.polls++
				d.mu.Unlock()
				d.apply(t)
			}
		})
	}
	wg.Wait()
}

// pending is the IDs of the tasks that are not done yet.
func (d *demo) pending() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, r := range d.rows {
		if !mcp.Terminal(r.status) {
			ids = append(ids, r.id)
		}
	}
	return ids
}

// listen opens a subscriptions/listen for the pending tasks. It stays open
// until the server goes away.
func (d *demo) listen(ctx context.Context) {
	s, ids := d.sess(), d.pending()
	d.event("%s taskIds of %d tasks", green.Render("→ subscriptions/listen"), len(ids))
	go s.call(ctx, "subscriptions/listen", map[string]any{"notifications": map[string]any{"taskIds": ids}}, nil)
}

// restart kills ax-mcp and starts a new one, which knows nothing. The demo
// kept the task IDs, and the new server finds every task in AX.
func (d *demo) restart(ctx context.Context) {
	old := d.sess()
	d.event("%s, with %d tasks in flight", red.Render(fmt.Sprintf("✝ SIGKILL ax-mcp (pid %d)", old.cmd.Process.Pid)), d.inFlight())
	old.stop()
	if _, err := d.connect(ctx); err != nil {
		d.event(red.Render("restarting ax-mcp: %v"), err)
		return
	}
	d.mu.Lock()
	d.restarts++
	d.mu.Unlock()
	d.event("%s starts empty; the task IDs the demo kept still work", green.Render(fmt.Sprintf("↻ new ax-mcp (pid %d)", d.sess().cmd.Process.Pid)))
	d.listen(ctx)
}

func (d *demo) inFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, r := range d.rows {
		if !mcp.Terminal(r.status) {
			n++
		}
	}
	return n
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
