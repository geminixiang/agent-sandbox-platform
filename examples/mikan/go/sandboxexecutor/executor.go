package mikan

import (
	"context"
	"time"

	sandbox "github.com/geminixiang/agent-sandbox-platform/packages/sdk-go"
)

type Task struct {
	ID             string
	Pool           string
	Command        string
	CWD            string
	TimeoutSeconds int
	Files          []TextFile
}

type TextFile struct {
	Path    string
	Content string
}

type Result struct {
	SandboxID string
	Stdout    string
	Stderr    string
	ExitCode  int
}

type Executor struct{ Client *sandbox.Client }

func (e Executor) Execute(ctx context.Context, task Task) (result Result, err error) {
	box, err := e.Client.Create(ctx, sandbox.CreateOptions{
		Pool:           task.Pool,
		IdempotencyKey: task.ID,
	})
	if err != nil {
		return Result{}, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cleanupErr := box.Close(cleanupCtx); err == nil {
			err = cleanupErr
		}
	}()

	for _, file := range task.Files {
		if err = box.Files().WriteText(ctx, file.Path, file.Content); err != nil {
			return Result{}, err
		}
	}
	command, err := box.Run(ctx, task.Command, sandbox.RunOptions{
		CWD:            task.CWD,
		TimeoutSeconds: task.TimeoutSeconds,
		Check:          true,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		SandboxID: box.ID(),
		Stdout:    command.Stdout,
		Stderr:    command.Stderr,
		ExitCode:  command.ExitCode,
	}, nil
}
