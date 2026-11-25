package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	taskTTL      = 10 * time.Minute
)

var sandboxRunTool = mcp.Tool{
	Name:  "sandbox_run",
	Title: "Run code as an AX task",
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
}

const instructions = "sandbox_run runs code as an AX task, in an isolated sandbox. " +
	"Clients with the io.modelcontextprotocol/tasks extension get a task back right away, so they can run many in parallel and poll them."

type server struct {
	b    *backend
	conn *jsonrpc.Conn
	// legacy is set when the client negotiated with initialize, so it gets
	// plain tool calls and, if it can, elicitation/create for questions.
	legacy    atomic.Bool
	canElicit atomic.Bool

	mu    sync.Mutex
	tasks map[string]*task
	subs  map[string]*subscription // by the ID of their subscriptions/listen
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
	answered int // the last ask this process handed an answer to
	result   *mcp.CallToolResult
	err      *mcp.Error
	changed  chan struct{} // closed and replaced on every change
	watching bool
}

type pendingAsk struct {
	ID              int            `json:"id"`
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema"`
}

// key is the ask's inputRequests key. axmcp numbers a job's asks from 1, so
// the key is unique over the task's lifetime.
func (a *pendingAsk) key() string { return "ask-" + strconv.Itoa(a.ID) }

// subscription is an open subscriptions/listen.
type subscription struct {
	id      json.RawMessage
	taskIDs map[string]bool
	cancel  context.CancelFunc
}

func newServer(b *backend) *server {
	return &server{b: b, tasks: map[string]*task{}, subs: map[string]*subscription{}}
}

func (t *task) snapshot() mcp.Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *task) snapshotLocked() mcp.Task {
	ttl := t.ttl
	return mcp.Task{
		TaskID:         t.id,
		Status:         t.status,
		StatusMessage:  t.message,
		CreatedAt:      t.created.UTC().Format(time.RFC3339Nano),
		LastUpdatedAt:  t.updated.UTC().Format(time.RFC3339Nano),
		TTLMs:          &ttl,
		PollIntervalMs: pollInterval.Milliseconds(),
	}
}

// detailed is what tasks/get returns and notifications/tasks carries.
func (t *task) detailed() mcp.DetailedTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := mcp.DetailedTask{Task: t.snapshotLocked()}
	switch t.status {
	case mcp.StatusInputRequired:
		d.InputRequests = map[string]mcp.InputRequest{t.ask.key(): {
			Method: "elicitation/create",
			Params: mcp.ElicitParams{Mode: "form", Message: t.ask.Message, RequestedSchema: t.ask.RequestedSchema},
		}}
	case mcp.StatusCompleted:
		d.Result = t.result
	case mcp.StatusFailed:
		d.Error = t.err
	}
	return d
}

// update applies fn to t and, if the status or message changed, wakes
// waiters and notifies listeners.
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
		// createdAt comes from AX's clock; never go before it.
		t.updated = later(time.Now(), t.created)
		close(t.changed)
		t.changed = make(chan struct{})
	}
	t.mu.Unlock()
	if changed {
		s.notify(t, nil)
	}
}

// notify sends t's state to sub, or to every listener on t if sub is nil.
func (s *server) notify(t *task, sub *subscription) {
	var ids []json.RawMessage
	if sub != nil {
		ids = append(ids, sub.id)
	} else {
		s.mu.Lock()
		for _, sub := range s.subs {
			if sub.taskIDs[t.id] {
				ids = append(ids, sub.id)
			}
		}
		s.mu.Unlock()
	}
	if len(ids) == 0 || s.conn == nil {
		return
	}
	d := t.detailed()
	for _, id := range ids {
		d.Meta = map[string]any{mcp.MetaSubscriptionID: id}
		s.conn.Notify("notifications/tasks", d)
	}
}

func (s *server) setStatus(t *task, status, message string) {
	s.update(t, func(t *task) { t.status, t.message = status, message })
}

// complete ends t with a tool result, which may be an error result.
func (s *server) complete(t *task, message string, res *mcp.CallToolResult) {
	s.update(t, func(t *task) { t.status, t.message, t.result, t.ask = mcp.StatusCompleted, message, res, nil })
}

// fail ends t with the JSON-RPC error a plain tools/call would have returned.
func (s *server) fail(t *task, message string) {
	s.update(t, func(t *task) {
		t.status, t.message, t.ask = mcp.StatusFailed, message, nil
		t.err = &mcp.Error{Code: jsonrpc.CodeInternalError, Message: message}
	})
}

func (s *server) cancelled(t *task, message string) {
	s.update(t, func(t *task) { t.status, t.message, t.ask = mcp.StatusCancelled, message, nil })
}

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if strings.HasPrefix(method, "notifications/") {
		if method == "notifications/cancelled" {
			s.endListen(params)
		}
		return nil, nil
	}
	var p struct {
		Meta mcp.RequestMeta `json:"_meta"`
	}
	json.Unmarshal(params, &p)
	if v := p.Meta.ProtocolVersion; v != "" && v != mcp.ProtocolVersion {
		return nil, &jsonrpc.Error{
			Code:    mcp.CodeUnsupportedProtocolVersion,
			Message: "Unsupported protocol version",
			Data:    map[string]any{"supported": []string{mcp.ProtocolVersion}, "requested": v},
		}
	}
	tasks := p.Meta.ClientCapabilities.Tasks()

	switch method {
	case "initialize":
		return s.initialize(params)
	case "server/discover":
		return mcp.DiscoverResult{
			ResultType:        mcp.ResultComplete,
			TTLMs:             time.Hour.Milliseconds(),
			CacheScope:        "public",
			SupportedVersions: []string{mcp.ProtocolVersion},
			Capabilities: map[string]any{
				"tools":      map[string]any{},
				"extensions": map[string]any{mcp.ExtTasks: map[string]any{}},
			},
			Instructions: instructions,
		}, nil
	case "ping":
		return mcp.EmptyResult{ResultType: mcp.ResultComplete}, nil
	case "tools/list":
		return mcp.ListToolsResult{ResultType: mcp.ResultComplete, TTLMs: time.Hour.Milliseconds(), CacheScope: "public", Tools: []mcp.Tool{sandboxRunTool}}, nil
	case "tools/call":
		return s.callTool(ctx, params, tasks)
	case "subscriptions/listen":
		return s.listen(ctx, params, tasks)
	case "tasks/get", "tasks/update", "tasks/cancel":
		if !tasks {
			return nil, missingTasks()
		}
		var tp mcp.TaskIDParams
		if err := json.Unmarshal(params, &tp); err != nil || tp.TaskID == "" {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "taskId is required")
		}
		t, err := s.lookup(ctx, tp.TaskID)
		if err != nil {
			return nil, err
		}
		switch method {
		case "tasks/get":
			d := t.detailed()
			d.ResultType = mcp.ResultComplete
			return d, nil
		case "tasks/update":
			return s.updateTask(ctx, t, params)
		default:
			return s.cancel(ctx, t)
		}
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
}

func missingTasks() error {
	return &jsonrpc.Error{
		Code:    mcp.CodeMissingClientCapability,
		Message: "Missing required client capability",
		Data:    map[string]any{"requiredCapabilities": map[string]any{"extensions": map[string]any{mcp.ExtTasks: map[string]any{}}}},
	}
}

// initialize serves clients on revisions before 2026-07-28. They get
// sandbox_run as a plain tool call that returns when the job is done.
func (s *server) initialize(params json.RawMessage) (any, error) {
	var p mcp.InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	var caps map[string]json.RawMessage
	json.Unmarshal(p.Capabilities, &caps)
	_, ok := caps["elicitation"]
	s.legacy.Store(true)
	s.canElicit.Store(ok)
	slog.Info("client connected", "client", p.ClientInfo.Name, "protocolVersion", p.ProtocolVersion, "elicitation", ok)
	version := mcp.LegacyProtocolVersion
	if p.ProtocolVersion == "2025-06-18" || p.ProtocolVersion == "2025-03-26" {
		version = p.ProtocolVersion
	}
	return mcp.InitializeResult{
		ProtocolVersion: version,
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo:      mcp.Implementation{Name: "ax-mcp", Title: "AX tasks over MCP", Version: "0.1.0"},
		Instructions:    instructions,
	}, nil
}

func (s *server) callTool(ctx context.Context, params json.RawMessage, asTask bool) (any, error) {
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

	// The AX task exists before the client learns the taskId, so tasks/get
	// finds it even if this process dies right after replying.
	id := newTaskID()
	at, err := s.b.create(ctx, id, j, taskTTL.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("creating AX task: %w", err)
	}
	t := s.adopt(at)
	s.setStatus(t, mcp.StatusWorking, "starting sandbox")
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.b.resume(rctx, id); err != nil {
			s.fail(t, "starting sandbox: "+err.Error())
			return
		}
		s.watch(t)
	}()
	slog.Info("task created", "task", id, "job", j.Name, "asTask", asTask)

	if !asTask {
		// The client can't take a task: same task, but the caller waits for it.
		return s.awaitResult(ctx, t, s.legacy.Load() && s.canElicit.Load())
	}
	return mcp.CreateTaskResult{ResultType: mcp.ResultTask, Task: t.snapshot()}, nil
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
		updated: later(at.GetMetadata().GetUpdateTime().AsTime(), at.GetMetadata().GetCreateTime().AsTime()),
		changed: make(chan struct{}),
	}
	s.tasks[id] = t
	return t
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
	if status.Code(err) == codes.NotFound || (err == nil && at.GetMetadata().GetLabels()[labelOwner] != s.b.owner) {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "Failed to retrieve task: task %q not found", id)
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
		s.fail(t, "the AX task was deleted")
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
		s.fail(t, msg)
	case v1alpha1.TaskState_TASK_STATE_SUSPENDING, v1alpha1.TaskState_TASK_STATE_SUSPENDED:
		s.setStatus(t, mcp.StatusWorking, "sandbox is suspended in AX (`ax resume task "+t.id+"` to continue)")
	default:
		s.setStatus(t, mcp.StatusWorking, "starting sandbox (AX: "+v1alpha1.FormatTaskState(at)+")")
	}
}

func (s *server) applyProbe(t *task, p *probe) {
	switch {
	case p.Exit != nil && p.Cancelled:
		// A cancelled task carries no result, so what the job wrote on its
		// way out becomes the status message.
		s.cancelled(t, cmp.Or(strings.TrimSpace(jobText(p)), "cancelled"))
	case p.Exit != nil:
		code, _ := strconv.Atoi(strings.TrimSpace(*p.Exit))
		if code == 0 {
			s.complete(t, "exited 0", jobResult(t.id, p, code, false))
		} else {
			// The tool ran and reports an error: that is a completed task
			// whose result has isError, not a failed one.
			s.complete(t, fmt.Sprintf("exited with code %d", code), jobResult(t.id, p, code, true))
		}
	case p.CancelReq || p.Cancelled:
		// The job got (or is about to get) SIGTERM and may be writing its
		// result on the way out.
		s.setStatus(t, mcp.StatusWorking, "stopping")
	case p.Ask != nil:
		var a pendingAsk
		if err := json.Unmarshal([]byte(*p.Ask), &a); err != nil {
			return // partially written; next poll
		}
		s.update(t, func(t *task) {
			if a.ID <= t.answered || (p.Answered != nil && *p.Answered == a.ID) {
				// Answered; the job hasn't picked it up yet.
				t.status, t.ask = mcp.StatusWorking, nil
				return
			}
			t.status, t.message, t.ask = mcp.StatusInputRequired, a.Message, &a
		})
	default:
		msg := "running"
		if p.Status != nil && strings.TrimSpace(*p.Status) != "" {
			msg = strings.TrimSpace(*p.Status)
		}
		s.update(t, func(t *task) { t.status, t.message, t.ask = mcp.StatusWorking, msg, nil })
	}
}

// updateTask serves tasks/update: an answer to the task's outstanding
// question goes into its sandbox, where axmcp.ask is waiting for it.
func (s *server) updateTask(ctx context.Context, t *task, params json.RawMessage) (any, error) {
	var p mcp.UpdateTaskParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	t.mu.Lock()
	ask := t.ask
	if t.status != mcp.StatusInputRequired {
		ask = nil
	}
	t.mu.Unlock()
	// Responses to keys that aren't outstanding are ignored.
	if ask != nil {
		if res, ok := p.InputResponses[ask.key()]; ok {
			if err := s.answer(ctx, t, ask.ID, res); err != nil {
				return nil, fmt.Errorf("handing the answer to %s: %w", t.id, err)
			}
		}
	}
	return mcp.EmptyResult{ResultType: mcp.ResultComplete}, nil
}

// answer writes the answer to ask askID into the task's sandbox.
func (s *server) answer(ctx context.Context, t *task, askID int, res mcp.ElicitResult) error {
	var err error
	for range 5 {
		if err = s.b.answer(ctx, t.id, askID, res); err == nil {
			s.update(t, func(t *task) {
				t.answered = max(t.answered, askID)
				t.status, t.message, t.ask = mcp.StatusWorking, "got an answer ("+res.Action+")", nil
			})
			slog.Info("answer delivered", "task", t.id, "ask", askID, "action", res.Action)
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

// awaitResult blocks until t is terminal and returns what a plain tools/call
// returns. The job's questions go to the client as elicitation/create if
// elicit, and are declined otherwise.
func (s *server) awaitResult(ctx context.Context, t *task, elicit bool) (*mcp.CallToolResult, error) {
	asked := 0
	for {
		t.mu.Lock()
		st, ch, ask := t.status, t.changed, t.ask
		res, rpcErr, msg := t.result, t.err, t.message
		t.mu.Unlock()
		switch st {
		case mcp.StatusCompleted:
			r := *res
			r.ResultType = mcp.ResultComplete
			return &r, nil
		case mcp.StatusFailed:
			return nil, &jsonrpc.Error{Code: rpcErr.Code, Message: rpcErr.Message}
		case mcp.StatusCancelled:
			return nil, jsonrpc.Errorf(jsonrpc.CodeInternalError, "cancelled: %s", msg)
		case mcp.StatusInputRequired:
			if ask != nil && ask.ID != asked {
				asked = ask.ID
				answer := mcp.ElicitResult{Action: "decline"}
				if elicit {
					err := s.conn.Call(ctx, "elicitation/create", mcp.ElicitParams{
						Mode: "form", Message: ask.Message, RequestedSchema: ask.RequestedSchema,
					}, &answer)
					if err != nil {
						if ctx.Err() != nil {
							return nil, ctx.Err()
						}
						slog.Warn("elicitation failed", "task", t.id, "err", err)
						answer = mcp.ElicitResult{Action: "cancel"}
					}
				}
				if err := s.answer(ctx, t, ask.ID, answer); err != nil {
					return nil, err
				}
				continue
			}
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// cancel serves tasks/cancel. It only acknowledges: the job gets SIGTERM, and
// the task reaches cancelled once it has exited.
func (s *server) cancel(ctx context.Context, t *task) (any, error) {
	ack := mcp.EmptyResult{ResultType: mcp.ResultComplete}
	t.mu.Lock()
	st := t.status
	t.mu.Unlock()
	if mcp.Terminal(st) {
		return ack, nil
	}
	at, err := s.b.get(ctx, t.id)
	if err != nil {
		return nil, err
	}
	if at.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING {
		if err := s.b.delete(ctx, t.id); err != nil { // nothing to stop yet
			return nil, fmt.Errorf("cancelling %s: %w", t.id, err)
		}
		s.cancelled(t, "cancelled before the sandbox started")
		return ack, nil
	}
	if err := s.b.cancel(ctx, t.id); err != nil {
		return nil, fmt.Errorf("cancelling %s: %w", t.id, err)
	}
	s.setStatus(t, mcp.StatusWorking, "stopping")
	slog.Info("cancel requested", "task", t.id)
	return ack, nil
}

// listen serves subscriptions/listen: notifications/tasks for the requested
// tasks until the client cancels the request or the server shuts down.
func (s *server) listen(ctx context.Context, params json.RawMessage, tasks bool) (any, error) {
	var p mcp.ListenParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if len(p.Notifications.TaskIDs) > 0 && !tasks {
		return nil, missingTasks()
	}
	id := jsonrpc.RequestID(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sub := &subscription{id: id, taskIDs: map[string]bool{}, cancel: cancel}

	// Tasks from before a restart are looked up in AX, in parallel.
	found := make([]*task, len(p.Notifications.TaskIDs))
	var wg sync.WaitGroup
	for i, tid := range p.Notifications.TaskIDs {
		wg.Go(func() { found[i], _ = s.lookup(ctx, tid) })
	}
	wg.Wait()
	acked := []string{}
	for _, t := range found {
		if t != nil && !sub.taskIDs[t.id] {
			sub.taskIDs[t.id] = true
			acked = append(acked, t.id)
		}
	}
	meta := map[string]any{mcp.MetaSubscriptionID: id}
	s.conn.Notify("notifications/subscriptions/acknowledged", mcp.ListenAck{Meta: meta, Notifications: mcp.SubscriptionFilter{TaskIDs: acked}})
	s.mu.Lock()
	s.subs[string(id)] = sub
	s.mu.Unlock()
	slog.Info("listening", "subscription", string(id), "tasks", len(acked))
	// Start the listener off with where each task is now.
	for _, t := range found {
		if t != nil {
			s.notify(t, sub)
		}
	}

	<-ctx.Done()
	s.mu.Lock()
	delete(s.subs, string(id))
	s.mu.Unlock()
	return mcp.EmptyResult{ResultType: mcp.ResultComplete, Meta: meta}, nil
}

// endListen closes the subscription a notifications/cancelled names.
func (s *server) endListen(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	sub := s.subs[string(p.RequestID)]
	s.mu.Unlock()
	if sub != nil {
		sub.cancel()
	}
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

// jobText is what the job reported: its axmcp.result, else its stdout.
func jobText(p *probe) string {
	if text := deref(p.Output); strings.TrimSpace(text) != "" {
		return text
	}
	return deref(p.Stdout)
}

func jobResult(id string, p *probe, exitCode int, isError bool) *mcp.CallToolResult {
	text := jobText(p)
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{{Type: "text", Text: text}},
		StructuredContent: map[string]any{"sandbox": id, "exitCode": exitCode, "stdout": deref(p.Stdout)},
		IsError:           isError,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}


func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
