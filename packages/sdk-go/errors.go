package sandbox

import (
	"errors"
	"fmt"
)

const (
	codeClientClosed    = "CLIENT_CLOSED"
	codeRequestFailed   = "REQUEST_FAILED"
	codeInvalidResponse = "INVALID_RESPONSE"
	codeCommandFailed   = "COMMAND_FAILED"
	codeIntegrity       = "INTEGRITY_ERROR"
)

// Error represents a platform or SDK operation failure.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return "sandbox: " + e.Code
	}
	return "sandbox: operation failed"
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *Error) Is(target error) bool {
	var other *Error
	if errors.As(target, &other) && other.Code != "" && e != nil {
		if other.Code == codeIntegrity {
			return e.Code == codeIntegrity || e.Code == "CONTENT_LENGTH_MISMATCH" || e.Code == "CONTENT_DIGEST_MISMATCH" || e.Code == "INVALID_CONTENT_DIGEST"
		}
		return e.Code == other.Code
	}
	return false
}

var (
	ErrNotFound              = &Error{Code: "LEASE_NOT_FOUND"}
	ErrNotActive             = &Error{Code: "LEASE_NOT_ACTIVE"}
	ErrExpired               = &Error{Code: "LEASE_EXPIRED"}
	ErrQuotaExceeded         = &Error{Code: "LEASE_QUOTA_EXCEEDED"}
	ErrAborted               = &Error{Code: "ABORTED"}
	ErrFileNotFound          = &Error{Code: "FILE_NOT_FOUND"}
	ErrTransferTooLarge      = &Error{Code: "TRANSFER_TOO_LARGE"}
	ErrTransferLimit         = &Error{Code: "TRANSFER_LIMIT_REACHED"}
	ErrIntegrity             = &Error{Code: codeIntegrity}
	ErrStreamingNotSupported = &Error{Code: "STREAMING_NOT_SUPPORTED"}
	ErrInvalidCursor         = &Error{Code: "INVALID_CURSOR"}
	ErrCursorExpired         = &Error{Code: "CURSOR_EXPIRED"}
	ErrUnknownPool           = &Error{Code: "UNKNOWN_POOL"}
	ErrClosed                = &Error{Code: codeClientClosed}
	ErrCommandFailed         = &Error{Code: codeCommandFailed}
)

// CommandFailedError retains a failed command and all of its diagnostics.
type CommandFailedError struct {
	Command string
	Result  CommandResult
}

func (e *CommandFailedError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.Result.ExitCode)
}
func (e *CommandFailedError) Is(target error) bool { return target == ErrCommandFailed }

func (e *CommandFailedError) Unwrap() error { return ErrCommandFailed }

func operationError(err error) error {
	if err == nil {
		return nil
	}
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return err
	}
	if errors.Is(err, ErrCommandFailed) {
		return err
	}
	return &Error{Code: codeRequestFailed, Message: "sandbox: request failed", Cause: err}
}

func abortedError(ctxErr error) error {
	return &Error{Code: "ABORTED", Message: "sandbox: request aborted", Cause: ctxErr}
}

func integrityError(message string, cause error) error {
	return &Error{Code: codeIntegrity, Message: message, Cause: cause}
}
