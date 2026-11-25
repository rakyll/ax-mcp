// Command ax-mcp-demo is an MCP client that tours the MCP tasks protocol
// against ax-mcp: it fans out sandboxed jobs as tasks, answers a question one
// of them asks from inside its sandbox, cancels a runaway job, kills ax-mcp
// mid-flight and recovers every task from AX, and collects the results.
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
	d := &demo{start: time.Now(), byID: map[string]*row{}, plain: !outTTY}
	if err := d.openLogs(); err != nil {
		fmt.Fprintf(os.Stderr, "ax-mcp-demo: %v\n", err)
		os.Exit(1)
	}

	// In a terminal, Bubble Tea reads ctrl+c as a key and the model cancels;
	// otherwise SIGINT does.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	d.cancel = cancel
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

	d.print(step("1", "initialize", "the server advertises the tasks capability"))
	d.setActivity("starting ax-mcp")
	init, err := d.connect(ctx)
	if err != nil {
		return err
	}
	tasksCap, _ := json.Marshal(init.Capabilities["tasks"])
	d.print(frame("←", "protocolVersion %s, capabilities.tasks = %s", init.ProtocolVersion, tasksCap))

	d.print(step("2", "tools/list", "tools say whether they can run as tasks"))
	var tools mcp.ListToolsResult
	if err := d.sess().conn.Call(ctx, "tools/list", struct{}{}, &tools); err != nil {
		return err
	}
	for _, t := range tools.Tools {
		d.print(frame("←", "%s  execution.taskSupport = %q", bold.Render(t.Name), t.Execution.TaskSupport))
	}

	d.print(step("3", "tools/call × 5, task-augmented", "each call returns a CreateTaskResult right away; the work runs in AX"))
	d.setActivity("creating tasks")
	for _, j := range jobs {
		code, _ := jobFiles.ReadFile(j.file)
		ttl := int64(10 * time.Minute / time.Millisecond)
		params := map[string]any{
			"name":      "sandbox_run",
			"arguments": map[string]any{"code": string(code), "language": j.lang, "name": j.name},
			"task":      mcp.TaskParams{TTL: &ttl},
		}
		t0 := time.Now()
		var res mcp.CreateTaskResult
		if err := d.sess().conn.Call(ctx, "tools/call", params, &res); err != nil {
			return fmt.Errorf("tools/call %s: %w", j.name, err)
		}
		d.print(frame("→", "tools/call sandbox_run {name: %-12s task: {ttl: %d}}", strconv.Quote(j.name)+",", ttl) + "\n" +
			frame("←", "task %s %s %s", cyan.Render(res.Task.TaskID), res.Task.Status, faint.Render("("+time.Since(t0).Round(time.Millisecond).String()+")")))
		d.add(&row{id: res.Task.TaskID, job: j.name, status: res.Task.Status, message: res.Task.StatusMessage})
	}

	d.print(step("4", "watch", "notifications/tasks/status and tasks/get polling drive the board"))
	d.setActivity("")
	d.setBoard(true)
	d.watch(ctx)
	d.setBoard(false)
	d.stopUI()
	d.print(renderBoard(d.snapshot(), "", 0, !d.plain))
	if ctx.Err() != nil {
		return nil
	}

	d.print(step("5", "tasks/result", "the tool results, each tagged with io.modelcontextprotocol/related-task"))
	d.setActivity("collecting results")
	for _, r := range d.rows {
		var res mcp.CallToolResult
		if err := d.sess().conn.Call(ctx, "tasks/result", mcp.TaskIDParams{TaskID: r.id}, &res); err != nil {
			d.print(fmt.Sprintf("  %s %v", bold.Render(fmt.Sprintf("%-10s", r.job)), err))
			continue
		}
		icon, style := statusIcon(r.status)
		text := lipgloss.NewStyle()
		if r.status == mcp.StatusFailed {
			text = red
		}
		var b strings.Builder
		for i, line := range resultLines(res, 5) {
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

func resultLines(res mcp.CallToolResult, max int) []string {
	var text strings.Builder
	for _, c := range res.Content {
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

func (s *session) stop() {
	s.cmd.Process.Kill()
	s.cmd.Wait()
}

type row struct {
	id, job, status, message string
	resultRunning            bool
}

type demo struct {
	start   time.Time
	logPath string
	log     *os.File
	trace   *os.File
	ui      *tea.Program
	plain   bool // stdout is not a terminal: print, don't draw
	cancel  context.CancelFunc

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
	activity string // what the demo is doing when there is no board
	answered bool
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

// connect starts ax-mcp and initializes a session with it.
func (d *demo) connect(ctx context.Context) (*mcp.InitializeResult, error) {
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

	var res mcp.InitializeResult
	err = s.conn.Call(ctx, "initialize", mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		Capabilities:    json.RawMessage(`{"elicitation":{"form":{}}}`),
		ClientInfo:      mcp.Implementation{Name: "ax-mcp-demo", Version: "0.1.0"},
	}, &res)
	if err != nil {
		s.stop()
		return nil, fmt.Errorf("initialize: %w (see %s)", err, d.logPath)
	}
	s.conn.Notify("notifications/initialized", struct{}{})
	d.mu.Lock()
	d.cur = s
	d.mu.Unlock()
	return &res, nil
}

// handle serves what ax-mcp sends us: status notifications and elicitations.
func (d *demo) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "notifications/tasks/status":
		var t mcp.Task
		if json.Unmarshal(params, &t) == nil {
			d.mu.Lock()
			d.notifs++
			d.mu.Unlock()
			d.apply(t)
		}
		return nil, nil
	case "elicitation/create":
		var p mcp.ElicitParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		return d.elicit(ctx, p)
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
}

func (d *demo) apply(t mcp.Task) {
	d.mu.Lock()
	r, ok := d.byID[t.TaskID]
	if !ok || mcp.Terminal(r.status) {
		d.mu.Unlock()
		return
	}
	changed := r.status != t.Status
	r.status, r.message = t.Status, t.StatusMessage
	d.mu.Unlock()
	if changed && mcp.Terminal(t.Status) {
		icon, style := statusIcon(t.Status)
		d.event("%s %s", style.Render(icon+" "+r.job), t.Status)
	}
}

// elicit asks the user, through the UI's form, the question a job asked from
// inside its sandbox, and prints the exchange once it is answered.
func (d *demo) elicit(ctx context.Context, p mcp.ElicitParams) (mcp.ElicitResult, error) {
	taskID := ""
	if rt, ok := p.Meta[mcp.MetaRelatedTask].(map[string]any); ok {
		taskID, _ = rt["taskId"].(string)
	}
	req := elicitMsg{taskID: taskID, job: d.jobName(taskID), params: p, reply: make(chan mcp.ElicitResult, 1)}
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
		"  " + purple.Bold(true).Render("┌ elicitation/create") + " " + faint.Render("(related-task "+taskID+", job "+req.job+")"),
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
	lines = append(lines, "  "+purple.Render("└ "+res.Action), "")
	d.print(strings.Join(lines, "\n"))
	d.event("%s elicitation answered for %s; the answer was written into its sandbox", purple.Render("?"), req.job)

	d.mu.Lock()
	d.answered = true
	d.mu.Unlock()
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
	poll := time.NewTicker(time.Second)
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
			if r.status == mcp.StatusInputRequired && !r.resultRunning {
				r.resultRunning = true
				go d.awaitInput(ctx, r)
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
			var t mcp.Task
			if err := d.sess().conn.Call(ctx, "tasks/cancel", mcp.TaskIDParams{TaskID: tuneID}, &t); err != nil {
				d.event(red.Render("tasks/cancel %s failed: %v"), tuneID, err)
			} else {
				d.apply(t)
			}
		}
		if !restarted && cancelled && (answered || age > 40*time.Second) {
			restarted = true
			d.restart(ctx)
		}
	}
}

func (d *demo) pollAll(ctx context.Context) {
	d.mu.Lock()
	var ids []string
	for _, r := range d.rows {
		if !mcp.Terminal(r.status) {
			ids = append(ids, r.id)
		}
	}
	d.mu.Unlock()
	s := d.sess()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var t mcp.Task
			if s.conn.Call(cctx, "tasks/get", mcp.TaskIDParams{TaskID: id}, &t) == nil {
				d.mu.Lock()
				d.polls++
				d.mu.Unlock()
				d.apply(t)
			}
		})
	}
	wg.Wait()
}

// awaitInput calls tasks/result for a task that needs input. That is the
// channel ax-mcp uses to send the task's elicitation/create request.
func (d *demo) awaitInput(ctx context.Context, r *row) {
	d.event("%s %s is input_required %s, so the server can ask", purple.Render("?"), r.job, green.Render("→ tasks/result"))
	var res mcp.CallToolResult
	d.sess().conn.Call(ctx, "tasks/result", mcp.TaskIDParams{TaskID: r.id}, &res)
	d.mu.Lock()
	r.resultRunning = false
	d.mu.Unlock()
}

// restart kills ax-mcp and starts a new one, which knows nothing: tasks/list
// rebuilds every task from AX.
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
	var list mcp.ListTasksResult
	if err := d.sess().conn.Call(ctx, "tasks/list", struct{}{}, &list); err != nil {
		d.event(red.Render("tasks/list: %v"), err)
		return
	}
	var names []string
	for _, t := range list.Tasks {
		d.apply(t)
		names = append(names, d.jobName(t.TaskID))
	}
	d.event("%s %s recovered %d tasks from AX: %s", green.Render(fmt.Sprintf("↻ new ax-mcp (pid %d)", d.sess().cmd.Process.Pid)),
		green.Render("→ tasks/list"), len(list.Tasks), strings.Join(names, ", "))
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
