package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultMaxClients   = 16
	defaultReadTimeout  = 5 * time.Second
	defaultWriteTimeout = 10 * time.Second
)

type Server struct {
	manager      *Manager
	socket       string
	maxClients   int
	readTimeout  time.Duration
	writeTimeout time.Duration
}

func NewServer(manager *Manager, socket string) *Server {
	return &Server{
		manager:      manager,
		socket:       socket,
		maxClients:   defaultMaxClients,
		readTimeout:  defaultReadTimeout,
		writeTimeout: defaultWriteTimeout,
	}
}

func (s *Server) Listen() (net.Listener, error) {
	if filepath.Dir(s.socket) != s.manager.cfg.StateDir {
		return nil, errors.New("socket must be directly inside state directory")
	}
	_ = os.Remove(s.socket)
	listener, err := net.Listen("unix", s.socket)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(s.socket, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	defer func() {
		_ = listener.Close()
		_ = os.Remove(s.socket)
	}()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	clients := make(chan struct{}, s.maxClients)
	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		select {
		case clients <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		conn, err := listener.Accept()
		if err != nil {
			<-clients
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-clients }()
			s.handleConnection(ctx, conn)
		}()
	}
}

func (s *Server) handleConnection(serverCtx context.Context, conn net.Conn) {
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(s.readTimeout)); err != nil {
		return
	}
	if s.manager.cfg.RequireRoot {
		if err := requireRootPeer(conn); err != nil {
			return
		}
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxRequestBytes {
		s.writeResponse(conn, failure("REQUEST_TOO_LARGE", "request envelope exceeds limit"))
		return
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(conn, body); err != nil {
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	var request Request
	if err := json.Unmarshal(body, &request); err != nil {
		s.writeResponse(conn, failure("INVALID_REQUEST", "request is not valid JSON"))
		return
	}

	requestCtx, cancel := context.WithCancel(serverCtx)
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		var extra [1]byte
		_, _ = conn.Read(extra[:])
		// EOF/error means disconnect; extra input violates the one-request
		// framing contract. Either way no operation remains detached.
		cancel()
	}()
	response := s.Dispatch(requestCtx, request)
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseRead()
	}
	cancel()
	<-disconnected
	s.writeResponse(conn, response)
}

func (s *Server) writeResponse(conn net.Conn, response Response) {
	if err := conn.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
		return
	}
	if err := writeFrame(conn, response); errors.Is(err, errResponseTooLarge) {
		_ = writeFrame(conn, failure("RESPONSE_TOO_LARGE", "response envelope exceeds limit"))
	}
}

var errResponseTooLarge = errors.New("response envelope exceeds limit")

func writeFrame(writer io.Writer, response Response) error {
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > MaxResponseBytes {
		return errResponseTooLarge
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err = writer.Write(header[:]); err != nil {
		return err
	}
	_, err = writer.Write(body)
	return err
}

func (s *Server) Dispatch(ctx context.Context, request Request) Response {
	if request.Version != ProtocolVersion {
		return failure("UNSUPPORTED_VERSION", "unsupported protocol version")
	}
	if request.Operation == "health" {
		if response, unhealthy := s.manager.unhealthyResponse(); unhealthy {
			return response
		}
		return success()
	}
	switch request.Operation {
	case "start":
		return s.manager.Start(request)
	case "list":
		return s.manager.List()
	case "status":
		return s.manager.Status(request.CommandID)
	case "connect":
		return s.manager.Connect(request.CommandID, request.After)
	case "stdin":
		return s.manager.Stdin(request.CommandID, request.Data)
	case "closeStdin":
		return s.manager.CloseStdin(request.CommandID)
	case "signal":
		return s.manager.Signal(request.CommandID, request.Signal)
	case "wait":
		return s.manager.Wait(ctx, request.CommandID, time.Duration(request.TimeoutMS)*time.Millisecond)
	case "killAll":
		return s.manager.KillAll()
	default:
		return failure("UNKNOWN_OPERATION", "unknown operation")
	}
}
