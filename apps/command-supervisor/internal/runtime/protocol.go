package runtime

import "time"

const (
	ProtocolVersion  = 1
	MaxRequestBytes  = 1 << 20
	MaxResponseBytes = 12 << 20
	MaxEventBytes    = 16 << 10
	MaxSpoolBytes    = 8 << 20
	MaxCommands      = 256
	MaxStdinBytes    = 64 << 10
)

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateExited   State = "exited"
	StateSignaled State = "signaled"
	StateLost     State = "lost"
)

func (s State) Terminal() bool {
	return s == StateExited || s == StateSignaled || s == StateLost
}

type Request struct {
	Version   int               `json:"version"`
	Operation string            `json:"operation"`
	RequestID string            `json:"requestId,omitempty"`
	CommandID string            `json:"commandId,omitempty"`
	Argv      []string          `json:"argv,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	After     uint64            `json:"after,omitempty"`
	Data      []byte            `json:"data,omitempty"`
	Signal    string            `json:"signal,omitempty"`
	TimeoutMS int               `json:"timeoutMs,omitempty"`
}

type Event struct {
	Seq    uint64 `json:"seq"`
	Stream string `json:"stream"`
	Data   []byte `json:"data"`
}

type Command struct {
	ID         string    `json:"id"`
	RequestID  string    `json:"requestId"`
	Argv       []string  `json:"argv"`
	Cwd        string    `json:"cwd"`
	Env        []string  `json:"-"`
	SpecHash   string    `json:"specHash"`
	State      State     `json:"state"`
	ExitCode   *int      `json:"exitCode,omitempty"`
	Signal     string    `json:"signal,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	NextSeq    uint64    `json:"nextSeq"`
}

type Response struct {
	Version    int       `json:"version"`
	OK         bool      `json:"ok"`
	Code       string    `json:"code,omitempty"`
	Message    string    `json:"message,omitempty"`
	Command    *Command  `json:"command,omitempty"`
	Commands   []Command `json:"commands,omitempty"`
	Events     []Event   `json:"events,omitempty"`
	NextCursor uint64    `json:"nextCursor,omitempty"`
}

func success() Response { return Response{Version: ProtocolVersion, OK: true} }
func failure(code, message string) Response {
	return Response{Version: ProtocolVersion, OK: false, Code: code, Message: message}
}
