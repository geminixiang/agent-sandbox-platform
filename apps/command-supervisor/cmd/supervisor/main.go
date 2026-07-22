package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	goruntime "runtime"
	"syscall"

	commandruntime "github.com/geminixiang/agent-sandbox-platform/apps/command-supervisor/internal/runtime"
)

func main() {
	stateDir := flag.String("state-dir", "/run/agent-sandbox-supervisor", "root-owned runtime state directory")
	socket := flag.String("socket", "", "Unix socket path (defaults inside state-dir)")
	flag.Parse()
	if goruntime.GOOS != "linux" {
		fatal("the trusted supervisor is Linux-only")
	}
	if *socket == "" {
		*socket = *stateDir + "/supervisor.sock"
	}
	manager, err := commandruntime.NewManager(commandruntime.Config{StateDir: *stateDir, ChildUID: 10001, ChildGID: 10001, RequireRoot: true})
	if err != nil {
		fatal(err.Error())
	}
	defer manager.Close()
	server := commandruntime.NewServer(manager, *socket)
	listener, err := server.Listen()
	if err != nil {
		fatal(err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err = server.Serve(ctx, listener); err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
