package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/client"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

var (
	//go:embed sandbox/wrapper.sh
	wrapperScript string
	//go:embed sandbox/axmcp.py
	helperModule string
	//go:embed sandbox/probe.py
	probeScript string
)

// Labels on every AX task that backs an MCP task. They are all ax-mcp needs
// to rebuild its view of a task after a restart: AX is the only state.
const (
	labelOwner = "mcp.ax.io/owner"
	labelTTL   = "mcp.ax.io/ttl-ms"
	labelJob   = "mcp.ax.io/job"
)

// backend runs MCP tasks as AX tasks.
type backend struct {
	dialOpts client.Options
	atespace string
	image    string
	axBin    string // the ax CLI, for `ax ssh`
	owner    string

	mu   sync.Mutex
	ax   v1alpha1.AXClient
	conn *grpc.ClientConn
}

// call runs fn against ax-server. The address of ax-server is usually a local
// port-forward, which can die and come back on another port (ax and other
// clients restart it), so on a connection error call dials again and retries.
func (b *backend) call(ctx context.Context, fn func(v1alpha1.AXClient) error) error {
	for attempt := 0; ; attempt++ {
		b.mu.Lock()
		if b.ax == nil {
			ax, conn, err := client.Dial(ctx, b.dialOpts)
			if err != nil {
				b.mu.Unlock()
				return fmt.Errorf("connecting to AX: %w", err)
			}
			b.ax, b.conn = ax, conn
		}
		ax, conn := b.ax, b.conn
		b.mu.Unlock()

		err := fn(ax)
		if attempt > 0 || !isConnError(err) {
			return err
		}
		b.mu.Lock()
		if b.conn == conn {
			b.conn.Close()
			b.ax, b.conn = nil, nil
		}
		b.mu.Unlock()
	}
}

// isConnError reports whether err means ax-server could not be reached, as
// opposed to ax-server answering Unavailable itself (no free workers).
func isConnError(err error) bool {
	return status.Code(err) == codes.Unavailable && strings.Contains(status.Convert(err).Message(), "connection error")
}

// job is what a sandbox_run call asks for.
type job struct {
	Code     string `json:"code"`
	Language string `json:"language,omitempty"`
	Name     string `json:"name,omitempty"`
}

func (b *backend) create(ctx context.Context, id string, j job, ttlMs int64) (*v1alpha1.Task, error) {
	cfg, err := structpb.NewStruct(map[string]any{"mcp": map[string]any{
		"tool": "sandbox_run", "language": j.Language, "name": j.Name,
	}})
	if err != nil {
		return nil, err
	}
	labels := map[string]string{labelOwner: b.owner, labelTTL: strconv.FormatInt(ttlMs, 10)}
	if j.Name != "" {
		labels[labelJob] = j.Name
	}
	task := b.taskFor(id, j, labels, cfg)
	var t *v1alpha1.Task
	err = b.call(ctx, func(ax v1alpha1.AXClient) (err error) {
		t, err = ax.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: task})
		return err
	})
	return t, err
}

func (b *backend) taskFor(id string, j job, labels map[string]string, cfg *structpb.Struct) *v1alpha1.Task {
	return &v1alpha1.Task{
		ApiVersion: "ax.io/v1alpha1",
		Kind:       "Task",
		Metadata:   &v1alpha1.ObjectMeta{Name: id, Atespace: b.atespace, Labels: labels},
		Spec: &v1alpha1.TaskSpec{
			Profile:  &v1alpha1.Profile{Image: b.image},
			Commands: []string{"sh", "-c", wrapperScript},
			Env: []*v1alpha1.EnvVar{
				{Name: "MCP_CODE", Value: j.Code},
				{Name: "MCP_LANG", Value: j.Language},
				{Name: "MCP_HELPER", Value: helperModule},
			},
			Debug:  true, // guest services back `ax ssh`, which is how ax-mcp watches the job
			Config: cfg,
		},
	}
}

// resume starts the task's actor, retrying while Substrate has no free worker.
func (b *backend) resume(ctx context.Context, id string) error {
	delay := time.Second
	for {
		err := b.call(ctx, func(ax v1alpha1.AXClient) error {
			_, err := ax.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: b.atespace, Name: id})
			return err
		})
		if err == nil || status.Code(err) != codes.Unavailable {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(delay):
		}
		delay = min(delay*2, 10*time.Second)
	}
}

func (b *backend) get(ctx context.Context, id string) (*v1alpha1.Task, error) {
	var t *v1alpha1.Task
	err := b.call(ctx, func(ax v1alpha1.AXClient) (err error) {
		t, err = ax.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: b.atespace, Name: id})
		return err
	})
	if err != nil {
		return nil, err
	}
	if t.GetMetadata().GetLabels()[labelOwner] != b.owner {
		return nil, status.Errorf(codes.NotFound, "task %q is not an MCP task", id)
	}
	return t, nil
}

func (b *backend) list(ctx context.Context) ([]*v1alpha1.Task, error) {
	var out []*v1alpha1.Task
	token := ""
	for {
		var resp *v1alpha1.ListTasksResponse
		err := b.call(ctx, func(ax v1alpha1.AXClient) (err error) {
			resp, err = ax.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: b.atespace, PageSize: 1000, PageToken: token})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, t := range resp.GetTasks() {
			if t.GetMetadata().GetLabels()[labelOwner] == b.owner {
				out = append(out, t)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			return out, nil
		}
	}
}

func (b *backend) delete(ctx context.Context, id string) error {
	return b.call(ctx, func(ax v1alpha1.AXClient) error {
		_, err := ax.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: b.atespace, Name: id})
		return err
	})
}

// exec runs argv in the task's sandbox through `ax ssh`.
func (b *backend) exec(ctx context.Context, id string, stdin []byte, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := append([]string{"ssh", "-a", b.atespace, id, "--"}, argv...)
	cmd := exec.CommandContext(ctx, b.axBin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("ax ssh %s: %w: %s", id, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// probe is what the sandbox reports about the job.
type probe struct {
	Status    *string `json:"status"`
	Ask       *string `json:"ask"`
	Answered  *int    `json:"answered"`
	Exit      *string `json:"exit"`
	CancelReq bool    `json:"cancelRequested"`
	Cancelled bool    `json:"cancelled"`
	Output    *string `json:"output"`
	Stdout    *string `json:"stdout"`
}

func (b *backend) probe(ctx context.Context, id string) (*probe, error) {
	out, err := b.exec(ctx, id, nil, "python3", "-c", probeScript)
	if err != nil {
		return nil, err
	}
	var p probe
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("decoding probe of %s: %w (%q)", id, err, out)
	}
	return &p, nil
}

// answer hands the user's answer to the job blocked in axmcp.ask.
func (b *backend) answer(ctx context.Context, id string, askID int, result any) error {
	data, err := json.Marshal(map[string]any{"id": askID, "result": result})
	if err != nil {
		return err
	}
	_, err = b.exec(ctx, id, data, "sh", "-c", `d="${AX_WORKSPACE:-$PWD}/.ax/mcp"; cat > "$d/answer.json.tmp" && mv "$d/answer.json.tmp" "$d/answer.json"`)
	return err
}

// cancel asks the wrapper to kill the job's process group.
func (b *backend) cancel(ctx context.Context, id string) error {
	_, err := b.exec(ctx, id, nil, "sh", "-c", `touch "${AX_WORKSPACE:-$PWD}/.ax/mcp/cancel"`)
	return err
}
