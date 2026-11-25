package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/rakyll/ax-mcp/internal/jsonrpc"
	"github.com/rakyll/ax-mcp/internal/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	pollInterval = time.Second
	defaultTTL   = 10 * time.Minute
	minTTL       = 30 * time.Second
	maxTTL       = time.Hour
)

var sandboxRunTool = mcp.Tool{
	Name:  "sandbox_run",
	Title: "Run code in an AX sandbox",
	Description: "Run a Python or shell program in a fresh sandbox on AX, with no network access, and return its output. " +
		"Python programs can `import axmcp` to report progress (axmcp.status), ask the user a question (axmcp.ask) and set the result (axmcp.result).",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code":     map[string]any{"type": "string", "description": "The program to run."},
			"language": map[string]any{"type": "string", "enum": []string{"python", "sh"}, "default": "python"},
			"name":     map[string]any{"type": "string", "description": "A short label for the job."},
		},
		"required": []string{"code"},
	},
	Execution: &mcp.ToolExecution{TaskSupport: "optional"},
}

type server struct {
	b         *backend
	conn      *jsonrpc.Conn
	canElicit atomic.Bool

	mu    sync.Mutex
	tasks map[string]*task
}

// task is ax-mcp's cached view of one MCP task. Everything in it can be
// rebuilt from the AX task.
type task struct {
	id      string
	created time.Time
	ttl     int64

	mu       sync.Mutex
	status   string
	message  string
	updated  time.Time
	ask      *pendingAsk
	asking   int // id of the ask an elicitation is in flight for
	result   *mcp.CallToolResult
	changed  chan struct{} // closed and replaced on every change
	watching bool
}

type pendingAsk struct {
	ID              int            `json:"id"`
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema"`
}

func newServer(b *backend) *server {
	return &server{b: b, tasks: map[string]*task{}}
}

func (t *task) snapshot() mcp.Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	ttl := t.ttl
	return mcp.Task{
		TaskID:        t.id,
		Status:        t.status,
		StatusMessage: t.message,
		CreatedAt:     t.created.UTC().Format(time.RFC3339Nano),
		LastUpdatedAt: t.updated.UTC().Format(time.RFC3339Nano),
		TTL:           &ttl,
		PollInterval:  pollInterval.Milliseconds(),
	}
}

// update applies fn to t and, if the status or message changed, wakes
// waiters and sends notifications/tasks/status.
func (s *server) update(t *task, fn func(t *task)) {
	t.mu.Lock()
	if mcp.Terminal(t.status) {
		t.mu.Unlock()
		return // terminal tasks never change
	}
	before := t.status + "\x00" + t.message
	fn(t)
	changed := t.status+"\x00"+t.message != before
	if changed {
		t.updated = time.Now()
		close(t.changed)
		t.changed = make(chan struct{})
	}
	t.mu.Unlock()
	if changed && s.conn != nil {
		s.conn.Notify("notifications/tasks/status", t.snapshot())
	}
}

func (s *server) setStatus(t *task, status, message string) {
	s.update(t, func(t *task) { t.status, t.message = status, message })
}

func (s *server) finish(t *task, status, message string, res *mcp.CallToolResult) {
	s.update(t, func(t *task) { t.status, t.message, t.result, t.ask = status, message, res, nil })
}

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p mcp.InitializeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		var caps map[string]json.RawMessage
		json.Unmarshal(p.Capabilities, &caps)
		_, ok := caps["elicitation"]
		s.canElicit.Store(ok)
		slog.Info("client connected", "client", p.ClientInfo.Name, "protocolVersion", p.ProtocolVersion, "elicitation", ok)
		// Older clients get plain tool calls; tasks need 2025-11-25.
		version := mcp.ProtocolVersion
		if p.ProtocolVersion == "2025-06-18" || p.ProtocolVersion == "2025-03-26" {
			version = p.ProtocolVersion
		}
		return mcp.InitializeResult{
			ProtocolVersion: version,
			Capabilities: map[string]any{
				"tools": map[string]any{},
				"tasks": map[string]any{
					"list":     map[string]any{},
					"cancel":   map[string]any{},
					"requests": map[string]any{"tools": map[string]any{"call": map[string]any{}}},
				},
			},
			ServerInfo:   mcp.Implementation{Name: "ax-mcp", Title: "AX sandboxes as MCP tasks", Version: "0.1.0"},
			Instructions: "sandbox_run runs code in an isolated AX sandbox. Call it as a task to run many in parallel and poll them.",
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return mcp.ListToolsResult{Tools: []mcp.Tool{sandboxRunTool}}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	case "tasks/get":
		t, err := s.lookupParams(ctx, params)
		if err != nil {
			return nil, err
		}
		return t.snapshot(), nil
	case "tasks/result":
		t, err := s.lookupParams(ctx, params)
		if err != nil {
			return nil, err
		}
		return s.awaitResult(ctx, t)
	case "tasks/list":
		return s.listTasks(ctx)
	case "tasks/cancel":
		t, err := s.lookupParams(ctx, params)
		if err != nil {
			return nil, err
		}
		return s.cancel(ctx, t)
	}
	if strings.HasPrefix(method, "notifications/") {
		return nil, nil
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
}

func (s *server) callTool(ctx context.Context, params json.RawMessage) (any, error) {
	var p mcp.CallToolParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if p.Name != sandboxRunTool.Name {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown tool %q", p.Name)
	}
	var j job
	if err := json.Unmarshal(p.Arguments, &j); err != nil || strings.TrimSpace(j.Code) == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "sandbox_run needs a non-empty \"code\" argument")
	}
	if j.Language == "" {
		j.Language = "python"
	}
	if j.Language != "python" && j.Language != "sh" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "language must be python or sh, not %q", j.Language)
	}
	ttl := defaultTTL
	if p.Task != nil && p.Task.TTL != nil {
		ttl = min(max(time.Duration(*p.Task.TTL)*time.Millisecond, minTTL), maxTTL)
	}

	// The AX task exists before the client learns the taskId, so a restart
	// right after this call cannot lose it.
	id := newTaskID()
	at, err := s.b.create(ctx, id, j, ttl.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("creating AX task: %w", err)
	}
	t := s.adopt(at)
	s.setStatus(t, mcp.StatusWorking, "starting sandbox")
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.b.resume(rctx, id); err != nil {
			s.finish(t, mcp.StatusFailed, "starting sandbox failed", errorResult(id, "starting sandbox: "+err.Error()))
			return
		}
		s.watch(t)
	}()
	slog.Info("task created", "task", id, "job", j.Name, "taskAugmented", p.Task != nil)

	if p.Task == nil {
		// A plain tools/call: same task, but the caller waits for it.
		return s.awaitResult(ctx, t)
	}
	return mcp.CreateTaskResult{
		Task: t.snapshot(),
		Meta: map[string]any{mcp.MetaImmediateResponse: fmt.Sprintf("Started sandbox %s on AX; it keeps running in the background.", id)},
	}, nil
}

// adopt returns the cached task for an AX task, creating it if needed.
func (s *server) adopt(at *v1alpha1.Task) *task {
	id := at.GetMetadata().GetName()
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		return t
	}
	ttl, _ := strconv.ParseInt(at.GetMetadata().GetLabels()[labelTTL], 10, 64)
	t := &task{
		id:      id,
		created: at.GetMetadata().GetCreateTime().AsTime(),
		ttl:     ttl,
		status:  mcp.StatusWorking,
		updated: at.GetMetadata().GetUpdateTime().AsTime(),
		changed: make(chan struct{}),
	}
	s.tasks[id] = t
	return t
}

func (s *server) lookupParams(ctx context.Context, params json.RawMessage) (*task, error) {
	var p mcp.TaskIDParams
	if err := json.Unmarshal(params, &p); err != nil || p.TaskID == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "taskId is required")
	}
	return s.lookup(ctx, p.TaskID)
}

// lookup finds a task, asking AX about tasks this process has not seen,
// such as ones created before a restart.
func (s *server) lookup(ctx context.Context, id string) (*task, error) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	s.mu.Unlock()
	if ok {
		return t, nil
	}
	at, err := s.b.get(ctx, id)
	if status.Code(err) == codes.NotFound {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown task %q", id)
	}
	if err != nil {
		return nil, err
	}
	t = s.adopt(at)
	s.refresh(ctx, t, at)
	go s.watch(t)
	return t, nil
}

// watch polls a task until it is terminal. At most one watcher runs per task.
func (s *server) watch(t *task) {
	t.mu.Lock()
	if t.watching {
		t.mu.Unlock()
		return
	}
	t.watching = true
	t.mu.Unlock()
	for {
		t.mu.Lock()
		done := mcp.Terminal(t.status)
		t.mu.Unlock()
		if done {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.refresh(ctx, t, nil)
		cancel()
		time.Sleep(pollInterval)
	}
}

// refresh maps the AX task (and, while it runs, its sandbox) onto the MCP task.
func (s *server) refresh(ctx context.Context, t *task, at *v1alpha1.Task) {
	var err error
	if at == nil {
		at, err = s.b.get(ctx, t.id)
	}
	if status.Code(err) == codes.NotFound {
		s.finish(t, mcp.StatusFailed, "the AX task was deleted", errorResult(t.id, "the AX task was deleted"))
		return
	}
	if err != nil {
		slog.Warn("reading AX task", "task", t.id, "err", err)
		return
	}
	state := at.GetStatus().GetState()
	switch state {
	case v1alpha1.TaskState_TASK_STATE_RUNNING:
		p, err := s.b.probe(ctx, t.id)
		if err != nil {
			slog.Warn("probing sandbox", "task", t.id, "err", err)
			return
		}
		s.applyProbe(t, p)
	case v1alpha1.TaskState_TASK_STATE_CRASHED, v1alpha1.TaskState_TASK_STATE_FAILED:
		msg := "sandbox " + strings.ToLower(v1alpha1.FormatTaskState(at))
		for _, c := range at.GetStatus().GetConditions() {
			if c.GetType() == "Ready" && c.GetMessage() != "" {
				msg += ": " + c.GetMessage()
			}
		}
		s.finish(t, mcp.StatusFailed, msg, errorResult(t.id, msg))
	case v1alpha1.TaskState_TASK_STATE_SUSPENDING, v1alpha1.TaskState_TASK_STATE_SUSPENDED:
		s.setStatus(t, mcp.StatusWorking, "sandbox is suspended in AX (`ax resume task "+t.id+"` to continue)")
	default:
		s.setStatus(t, mcp.StatusWorking, "starting sandbox (AX: "+v1alpha1.FormatTaskState(at)+")")
	}
}

func (s *server) applyProbe(t *task, p *probe) {
	switch {
	case p.Cancelled && p.Exit == nil:
		// The job got SIGTERM and may be writing its result on the way out.
		s.setStatus(t, mcp.StatusWorking, "stopping")
	case p.Cancelled:
		s.finish(t, mcp.StatusCancelled, "cancelled", jobResult(t.id, p, -1, true))
	case p.Exit != nil:
		code, _ := strconv.Atoi(strings.TrimSpace(*p.Exit))
		if code == 0 {
			s.finish(t, mcp.StatusCompleted, "exited 0", jobResult(t.id, p, code, false))
		} else {
			s.finish(t, mcp.StatusFailed, fmt.Sprintf("exited with code %d", code), jobResult(t.id, p, code, true))
		}
	case p.Ask != nil:
		var a pendingAsk
		if err := json.Unmarshal([]byte(*p.Ask), &a); err != nil {
			return // partially written; next poll
		}
		s.update(t, func(t *task) { t.status, t.message, t.ask = mcp.StatusInputRequired, a.Message, &a })
	default:
		msg := "running"
		if p.Status != nil && strings.TrimSpace(*p.Status) != "" {
			msg = strings.TrimSpace(*p.Status)
		}
		s.update(t, func(t *task) { t.status, t.message, t.ask = mcp.StatusWorking, msg, nil })
	}
}

// awaitResult blocks until t is terminal and returns its tool result. While it
// waits it delivers the task's questions to the client as elicitation/create
// requests, which is how the tasks utility carries input_required.
func (s *server) awaitResult(ctx context.Context, t *task) (*mcp.CallToolResult, error) {
	for {
		t.mu.Lock()
		if mcp.Terminal(t.status) {
			res := *t.result
			t.mu.Unlock()
			res.Meta = mcp.RelatedTask(t.id)
			return &res, nil
		}
		ch := t.changed
		var ask *pendingAsk
		if t.status == mcp.StatusInputRequired && t.ask != nil && t.asking != t.ask.ID {
			ask = t.ask
			t.asking = ask.ID
		}
		t.mu.Unlock()

		if ask != nil {
			if err := s.elicit(ctx, t, ask); err != nil {
				t.mu.Lock()
				t.asking = 0
				t.mu.Unlock()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				slog.Warn("elicitation failed", "task", t.id, "err", err)
				time.Sleep(pollInterval)
			}
			continue
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *server) elicit(ctx context.Context, t *task, ask *pendingAsk) error {
	res := mcp.ElicitResult{Action: "decline"}
	if s.canElicit.Load() {
		err := s.conn.Call(ctx, "elicitation/create", mcp.ElicitParams{
			Mode:            "form",
			Message:         ask.Message,
			RequestedSchema: ask.RequestedSchema,
			Meta:            mcp.RelatedTask(t.id),
		}, &res)
		if err != nil {
			return err
		}
	}
	// The user has answered; retry handing it over rather than asking again.
	var err error
	for range 5 {
		if err = s.b.answer(ctx, t.id, ask.ID, res); err == nil {
			s.setStatus(t, mcp.StatusWorking, "got an answer ("+res.Action+")")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return err
}

func (s *server) cancel(ctx context.Context, t *task) (mcp.Task, error) {
	t.mu.Lock()
	st := t.status
	t.mu.Unlock()
	if mcp.Terminal(st) {
		return mcp.Task{}, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "cannot cancel task %s: it is already %s", t.id, st)
	}
	at, err := s.b.get(ctx, t.id)
	if err != nil {
		return mcp.Task{}, err
	}
	if at.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING {
		if err := s.b.delete(ctx, t.id); err != nil { // nothing to stop yet
			return mcp.Task{}, fmt.Errorf("cancelling %s: %w", t.id, err)
		}
		s.finish(t, mcp.StatusCancelled, "cancelled by client", errorResult(t.id, "the task was cancelled"))
		return t.snapshot(), nil
	}
	if err := s.b.cancel(ctx, t.id); err != nil {
		return mcp.Task{}, fmt.Errorf("cancelling %s: %w", t.id, err)
	}
	// Give the job a moment to stop (the wrapper SIGKILLs it 5s after SIGTERM),
	// so the result carries whatever it wrote on the way out.
	res := errorResult(t.id, "the task was cancelled")
	for deadline := time.Now().Add(7 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if p, err := s.b.probe(ctx, t.id); err == nil && p.Exit != nil {
			res = jobResult(t.id, p, -1, true)
			break
		}
	}
	s.finish(t, mcp.StatusCancelled, "cancelled by client", res)
	slog.Info("task cancelled", "task", t.id)
	return t.snapshot(), nil
}

func (s *server) listTasks(ctx context.Context) (mcp.ListTasksResult, error) {
	ats, err := s.b.list(ctx)
	if err != nil {
		return mcp.ListTasksResult{}, err
	}
	var wg sync.WaitGroup
	for _, at := range ats {
		s.mu.Lock()
		_, known := s.tasks[at.GetMetadata().GetName()]
		s.mu.Unlock()
		if known {
			continue
		}
		t := s.adopt(at)
		wg.Go(func() {
			s.refresh(ctx, t, at)
			go s.watch(t)
		})
	}
	wg.Wait()

	s.mu.Lock()
	all := make([]*task, 0, len(s.tasks))
	for _, t := range s.tasks {
		all = append(all, t)
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(a, b *task) int { return a.created.Compare(b.created) })
	out := mcp.ListTasksResult{Tasks: []mcp.Task{}}
	for _, t := range all {
		out.Tasks = append(out.Tasks, t.snapshot())
	}
	return out, nil
}

// reap deletes AX tasks whose MCP task has outlived its TTL.
func (s *server) reap(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		ats, err := s.b.list(ctx)
		if err != nil {
			continue
		}
		for _, at := range ats {
			ttl, err := strconv.ParseInt(at.GetMetadata().GetLabels()[labelTTL], 10, 64)
			if err != nil || time.Since(at.GetMetadata().GetCreateTime().AsTime()) < time.Duration(ttl)*time.Millisecond {
				continue
			}
			id := at.GetMetadata().GetName()
			if err := s.b.delete(ctx, id); err == nil {
				slog.Info("TTL expired; deleted sandbox", "task", id)
			}
			s.mu.Lock()
			delete(s.tasks, id)
			s.mu.Unlock()
		}
	}
}

func jobResult(id string, p *probe, exitCode int, isError bool) *mcp.CallToolResult {
	text, stdout := deref(p.Output), deref(p.Stdout)
	if strings.TrimSpace(text) == "" {
		text = stdout
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{{Type: "text", Text: text}},
		StructuredContent: map[string]any{"sandbox": id, "exitCode": exitCode, "stdout": stdout},
		IsError:           isError,
	}
}

func errorResult(id, msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{{Type: "text", Text: msg}},
		StructuredContent: map[string]any{"sandbox": id},
		IsError:           true,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
