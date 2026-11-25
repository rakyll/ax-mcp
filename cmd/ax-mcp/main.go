// Command ax-mcp is an MCP server (stdio) whose tool calls run as MCP tasks
// (the io.modelcontextprotocol/tasks extension) backed by AX tasks: every
// tools/call becomes a sandboxed AX task, and tasks/get, tasks/update,
// tasks/cancel and notifications/tasks read from and act on it.
//
// ax-mcp keeps no state of its own. The AX task (its labels, its actor's
// state, and a few files in its workspace) is the MCP task, so ax-mcp can be
// killed and restarted without losing a task.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/ax/client"
	"github.com/rakyll/ax-mcp/internal/jsonrpc"
)

// defaultImage is the runner image that `make build-local-cluster` pushes to
// the local registry of an `ax local` cluster.
const defaultImage = "localhost:5001/ax-runner@sha256:2480d0dccb44ce9f09ae742c754f44940d0e713e39e0abc1c3a3014a5fe09957"

func main() {
	image := flag.String("image", envOr("AX_MCP_IMAGE", defaultImage), "digest-pinned image for sandboxes ($AX_MCP_IMAGE)")
	atespace := flag.String("atespace", envOr("AX_MCP_ATESPACE", "default"), "AX atespace for sandboxes ($AX_MCP_ATESPACE)")
	kubeContext := flag.String("context", "", "kubeconfig context of the AX cluster (default: current)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	axBin, err := exec.LookPath(envOr("AX_BIN", "ax"))
	if err != nil {
		fatal("finding the ax CLI (needed for ax ssh): %v", err)
	}
	b := &backend{dialOpts: client.Options{Context: *kubeContext, FromEnv: true}, atespace: *atespace, image: *image, axBin: axBin, owner: "ax-mcp"}
	if _, err := b.list(ctx); err != nil {
		fatal("%v", err)
	}

	s := newServer(b)
	s.conn = jsonrpc.NewConn(os.Stdin, os.Stdout, s.handle)
	go s.reap(ctx)
	log.Info("ax-mcp serving on stdio", "atespace", *atespace)
	if err := s.conn.Run(ctx); err != nil && ctx.Err() == nil {
		log.Info("session ended", "reason", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ax-mcp: "+format+"\n", args...)
	os.Exit(1)
}

// newTaskID returns an unguessable task ID: the spec lets anyone holding one
// read the task, and there is no tasks/list to scope.
func newTaskID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "mcp-" + strings.ToLower(base32.StdEncoding.EncodeToString(b))
}
