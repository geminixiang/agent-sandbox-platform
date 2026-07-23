package sandbox

import (
	"context"
	"errors"
	"strings"
)

// Credentials supplies a short-lived Subject token. GetToken is called for
// every HTTP operation so callers can rotate tokens without replacing Client.
type Credentials interface {
	GetToken(context.Context) (string, error)
}

// TokenProviderFunc adapts a function to Credentials.
type TokenProviderFunc func(context.Context) (string, error)

func (f TokenProviderFunc) GetToken(ctx context.Context) (string, error) {
	if f == nil {
		return "", errors.New("sandbox: nil token provider")
	}
	return f(ctx)
}

// StaticToken returns one fixed token. It is intended only for tokens that are
// already short-lived.
type StaticToken string

func (t StaticToken) GetToken(context.Context) (string, error) {
	token := strings.TrimSpace(string(t))
	if token == "" {
		return "", errors.New("sandbox: token must not be blank")
	}
	return token, nil
}
