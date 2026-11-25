// Package mcp holds the slice of the Model Context Protocol (2025-11-25) that
// ax-mcp speaks, including the experimental Tasks utility (SEP-1686).
package mcp

import "encoding/json"

// ProtocolVersion is the first MCP revision with tasks.
const ProtocolVersion = "2025-11-25"

// Task statuses.
const (
	StatusWorking       = "working"
	StatusInputRequired = "input_required"
	StatusCompleted     = "completed"
	StatusFailed        = "failed"
	StatusCancelled     = "cancelled"
)

// Terminal reports whether status is final.
func Terminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// Meta keys defined by the tasks utility.
const (
	MetaRelatedTask       = "io.modelcontextprotocol/related-task"
	MetaImmediateResponse = "io.modelcontextprotocol/model-immediate-response"
)

// Task is the task object carried by CreateTaskResult, tasks/get, tasks/list,
// tasks/cancel and notifications/tasks/status.
type Task struct {
	TaskID        string `json:"taskId"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
	CreatedAt     string `json:"createdAt"`
	LastUpdatedAt string `json:"lastUpdatedAt"`
	// TTL is in milliseconds from creation; null means unlimited.
	TTL          *int64 `json:"ttl"`
	PollInterval int64  `json:"pollInterval,omitempty"`
}

// TaskParams is the "task" field a requestor adds to make a request task-augmented.
type TaskParams struct {
	TTL *int64 `json:"ttl,omitempty"`
}

// CreateTaskResult is returned instead of the normal result of a task-augmented request.
type CreateTaskResult struct {
	Task Task           `json:"task"`
	Meta map[string]any `json:"_meta,omitempty"`
}

// TaskIDParams is the params of tasks/get, tasks/result and tasks/cancel.
type TaskIDParams struct {
	TaskID string `json:"taskId"`
}

// ListTasksResult is the result of tasks/list.
type ListTasksResult struct {
	Tasks      []Task `json:"tasks"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// InitializeParams is the params of initialize.
type InitializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ClientInfo      Implementation  `json:"clientInfo"`
}

// InitializeResult is the result of initialize.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Tool is a tool definition from tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Execution   *ToolExecution `json:"execution,omitempty"`
}

// ToolExecution says whether a tool can run as a task:
// "forbidden" (the default), "optional" or "required".
type ToolExecution struct {
	TaskSupport string `json:"taskSupport,omitempty"`
}

// ListToolsResult is the result of tools/list.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// CallToolParams is the params of tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Task      *TaskParams     `json:"task,omitempty"`
}

// Content is a text content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult is the result of tools/call, and of tasks/result for a
// task created by tools/call.
type CallToolResult struct {
	Content           []Content      `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

// ElicitParams is the params of elicitation/create (form mode).
type ElicitParams struct {
	Mode            string         `json:"mode,omitempty"`
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema"`
	Meta            map[string]any `json:"_meta,omitempty"`
}

// ElicitResult is the result of elicitation/create.
type ElicitResult struct {
	Action  string         `json:"action"` // accept, decline or cancel
	Content map[string]any `json:"content,omitempty"`
}

// RelatedTask returns the _meta value that ties a message to a task.
func RelatedTask(taskID string) map[string]any {
	return map[string]any{MetaRelatedTask: map[string]string{"taskId": taskID}}
}
