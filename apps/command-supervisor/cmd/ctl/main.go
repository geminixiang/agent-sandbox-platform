package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	commandruntime "github.com/geminixiang/agent-sandbox-platform/apps/command-supervisor/internal/runtime"
)

func main() {
	socket := flag.String("socket", "/run/agent-sandbox-supervisor/supervisor.sock", "supervisor Unix socket")
	flag.Parse()
	body, err := io.ReadAll(io.LimitReader(os.Stdin, commandruntime.MaxRequestBytes+1))
	if err != nil {
		fatal(err.Error())
	}
	if len(body) > commandruntime.MaxRequestBytes {
		fatal("request envelope exceeds limit")
	}
	response, err := commandruntime.RoundTrip(*socket, body)
	if err != nil {
		fatal(err.Error())
	}
	if _, err = os.Stdout.Write(append(response, '\n')); err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
