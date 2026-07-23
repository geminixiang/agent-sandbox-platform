package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
)

type Sandbox struct {
	client *Client
	files  *Files

	mu         sync.Mutex
	record     LeaseRecord
	transition *terminalTransition
}

type terminalTransition struct {
	kind   string
	done   chan struct{}
	record LeaseRecord
	err    error
}

func newSandbox(client *Client, record LeaseRecord) *Sandbox {
	sandbox := &Sandbox{client: client, record: record}
	sandbox.files = &Files{sandbox: sandbox}
	return sandbox
}

func (s *Sandbox) ID() string { return s.Record().ID }
func (s *Sandbox) Record() LeaseRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record
}
func (s *Sandbox) Files() *Files { return s.files }

func (s *Sandbox) Refresh(ctx context.Context) (LeaseRecord, error) {
	current, err := s.client.Get(ctx, s.ID())
	if err != nil {
		return LeaseRecord{}, err
	}
	record := current.Record()
	s.mu.Lock()
	s.record = record
	s.mu.Unlock()
	return record, nil
}

func (s *Sandbox) Run(ctx context.Context, command string, options RunOptions) (CommandResult, error) {
	if strings.TrimSpace(command) == "" {
		return CommandResult{}, errors.New("sandbox: command must not be blank")
	}
	if options.TimeoutSeconds < 0 {
		return CommandResult{}, errors.New("sandbox: TimeoutSeconds must be positive")
	}
	body := map[string]any{"command": command}
	if options.CWD != "" {
		body["cwd"] = options.CWD
	}
	if options.Env != nil {
		copyEnv := make(map[string]string, len(options.Env))
		keys := make([]string, 0, len(options.Env))
		for key := range options.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			copyEnv[key] = options.Env[key]
		}
		body["env"] = copyEnv
	}
	if options.TimeoutSeconds > 0 {
		body["timeoutSeconds"] = options.TimeoutSeconds
	}
	var response struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Code   int    `json:"code"`
	}
	if err := s.client.doJSON(ctx, http.MethodPost, leasePath+"/"+url.PathEscape(s.ID())+"/exec", nil, nil, body, &response, http.StatusOK); err != nil {
		return CommandResult{}, err
	}
	result := CommandResult{Stdout: response.Stdout, Stderr: response.Stderr, ExitCode: response.Code}
	if options.Check && !result.Succeeded() {
		return result, &CommandFailedError{Command: command, Result: result}
	}
	return result, nil
}

func (s *Sandbox) Release(ctx context.Context) (LeaseRecord, error) {
	return s.terminal(ctx, "release")
}
func (s *Sandbox) Delete(ctx context.Context) error { _, err := s.terminal(ctx, "delete"); return err }
func (s *Sandbox) Close(ctx context.Context) error  { _, err := s.terminal(ctx, "release"); return err }

func (s *Sandbox) terminal(ctx context.Context, kind string) (LeaseRecord, error) {
	s.mu.Lock()
	if s.transition != nil {
		transition := s.transition
		s.mu.Unlock()
		select {
		case <-transition.done:
			return transition.record, transition.err
		case <-ctx.Done():
			return LeaseRecord{}, abortedError(ctx.Err())
		}
	}
	transition := &terminalTransition{kind: kind, done: make(chan struct{})}
	s.transition = transition
	s.mu.Unlock()

	var record LeaseRecord
	var err error
	if kind == "delete" {
		err = s.client.doJSON(ctx, http.MethodDelete, leasePath+"/"+url.PathEscape(s.ID()), nil, nil, nil, nil, http.StatusNoContent)
		if err == nil {
			record = s.Record()
		}
	} else {
		var response leaseResponse
		err = s.client.doJSON(ctx, http.MethodPost, leasePath+"/"+url.PathEscape(s.ID())+"/release", nil, nil, nil, &response, http.StatusOK)
		if err == nil {
			err = validateRecord(response.Lease)
			record = response.Lease
		}
	}

	s.mu.Lock()
	if err != nil {
		s.transition = nil // failed cleanup is retryable
	} else {
		s.record = record
	}
	transition.record, transition.err = record, err
	close(transition.done)
	s.mu.Unlock()
	if err != nil {
		return LeaseRecord{}, err
	}
	return record, nil
}

func (s *Sandbox) String() string { return fmt.Sprintf("Sandbox(%s)", s.ID()) }
