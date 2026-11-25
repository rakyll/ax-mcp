// Package mcp holds the slice of the Model Context Protocol that ax-mcp
// speaks: the 2026-07-28 revision with the Tasks extension
// (io.modelcontextprotocol/tasks, SEP-2663), plus initialize for clients on
// older revisions.
package mcp

import "encoding/json"

// ProtocolVersion is the revision the Tasks extension builds on.
const ProtocolVersion = "2026-07-28"

// LegacyProtocolVersion is the newest revision that negotiates with initialize.
const LegacyProtocolVersion = "2025-11-25"

// ExtTasks identifies the Tasks extension in capabilities.
const ExtTasks = "io.modelcontextprotocol/tasks"

// Reserved _meta keys.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaSubscriptionID     = "io.modelcontextprotocol/subscriptionId"
)

// Error codes added by 2026-07-28.
const (
	CodeMissingClientCapability    = -32021
	CodeUnsupportedProtocolVersion = -32022
)

// Values of resultType.
const (
	ResultComplete = "complete"
	ResultTask     = "task"
)

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

// RequestMeta is the per-request _meta every 2026-07-28 request carries.
type RequestMeta struct {
	ProtocolVersion    string             `json:"io.modelcontextprotocol/protocolVersion,omitempty"`
	ClientCapabilities ClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	ClientInfo         *Implementation    `json:"io.modelcontextprotocol/clientInfo,omitempty"`
}

// ClientCapabilities is what a client supports, declared per request.
type ClientCapabilities struct {
	Elicitation json.RawMessage            `json:"elicitation,omitempty"`
	Extensions  map[string]json.RawMessage `json:"extensions,omitempty"`
}

// Tasks reports whether the client declared the Tasks extension.
func (c ClientCapabilities) Tasks() bool {
	_, ok := c.Extensions[ExtTasks]
	return ok
}

// Task is the state of a task, as in CreateTaskResult.
type Task struct {
	TaskID        string `json:"taskId"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
	CreatedAt     string `json:"createdAt"`
	LastUpdatedAt string `json:"lastUpdatedAt"`
	// TTLMs is in milliseconds from creation; null means unlimited.
	TTLMs          *int64 `json:"ttlMs"`
	PollIntervalMs int64  `json:"pollIntervalMs,omitempty"`
}

// DetailedTask is a Task with the fields of its status: inputRequests while
// input_required, result once completed, error once failed. tasks/get returns
// it and notifications/tasks carries it.
type DetailedTask struct {
	ResultType string         `json:"resultType,omitempty"`
	Meta       map[string]any `json:"_meta,omitempty"`
	Task
	InputRequests map[string]InputRequest `json:"inputRequests,omitempty"`
	Result        *CallToolResult         `json:"result,omitempty"`
	Error         *Error                  `json:"error,omitempty"`
}

// CreateTaskResult is what a server returns instead of a tool result when it
// runs the call as a task.
type CreateTaskResult struct {
	ResultType string `json:"resultType"` // "task"
	Task
}

// InputRequest is a server-to-client request the task waits on.
type InputRequest struct {
	Method string       `json:"method"` // elicitation/create
	Params ElicitParams `json:"params"`
}

// Error is a JSON-RPC error object, as carried by a failed task.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// TaskIDParams is the params of tasks/get and tasks/cancel.
type TaskIDParams struct {
	TaskID string `json:"taskId"`
}

// UpdateTaskParams is the params of tasks/update.
type UpdateTaskParams struct {
	TaskID         string                  `json:"taskId"`
	InputResponses map[string]ElicitResult `json:"inputResponses"`
}

// EmptyResult acknowledges tasks/update and tasks/cancel.
type EmptyResult struct {
	ResultType string         `json:"resultType"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

// ListenParams is the params of subscriptions/listen.
type ListenParams struct {
	Notifications SubscriptionFilter `json:"notifications"`
}

// SubscriptionFilter is the notifications a listener opts in to. ax-mcp only
// offers the Tasks extension's taskIds.
type SubscriptionFilter struct {
	TaskIDs []string `json:"taskIds,omitempty"`
}

// ListenAck is the params of notifications/subscriptions/acknowledged.
type ListenAck struct {
	Meta          map[string]any     `json:"_meta"`
	Notifications SubscriptionFilter `json:"notifications"`
}

// DiscoverResult is the result of server/discover.
type DiscoverResult struct {
	ResultType        string         `json:"resultType"`
	TTLMs             int64          `json:"ttlMs"`
	CacheScope        string         `json:"cacheScope"`
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      map[string]any `json:"capabilities"`
	Instructions      string         `json:"instructions,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

// InitializeParams is the params of initialize, for older clients.
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
}

// ListToolsResult is the result of tools/list.
type ListToolsResult struct {
	ResultType string `json:"resultType,omitempty"`
	TTLMs      int64  `json:"ttlMs"`
	CacheScope string `json:"cacheScope,omitempty"`
	Tools      []Tool `json:"tools"`
}

// CallToolParams is the params of tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Content is a text content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult is the result of tools/call, and the result of a completed
// task created by tools/call.
type CallToolResult struct {
	ResultType        string         `json:"resultType,omitempty"`
	Content           []Content      `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// ElicitParams is the params of elicitation/create (form mode).
type ElicitParams struct {
	Mode            string         `json:"mode,omitempty"`
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema"`
}

// ElicitResult is the answer to elicitation/create.
type ElicitResult struct {
	Action  string         `json:"action"` // accept, decline or cancel
	Content map[string]any `json:"content,omitempty"`
}

// ToolCallResult is either reply to tools/call from a client that declared
// the Tasks extension: a task when ResultType is "task", else a tool result.
type ToolCallResult struct {
	ResultType string `json:"resultType"`
	Task
	CallToolResult
}

// UnmarshalJSON decodes the shared resultType once and the rest into the
// matching half.
func (r *ToolCallResult) UnmarshalJSON(b []byte) error {
	var head struct {
		ResultType string `json:"resultType"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	r.ResultType = head.ResultType
	if head.ResultType == ResultTask {
		return json.Unmarshal(b, &r.Task)
	}
	return json.Unmarshal(b, &r.CallToolResult)
}
