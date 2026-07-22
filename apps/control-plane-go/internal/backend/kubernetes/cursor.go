package kubernetes

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

const (
	listCursorVersion = 1
	listCursorDomain  = "lease-list-cursor/v1"
	listCursorAAD     = "agent-sandbox-platform/lease-list-cursor/v1"
	maxCursorBytes    = lease.MaxListCursorBytes
)

type listCursorPayload struct {
	Version      int    `json:"v"`
	ScopeDigest  string `json:"scope"`
	PoolDigest   string `json:"pool"`
	Limit        int    `json:"limit"`
	AsOfUnixNano int64  `json:"asOf"`
	Continue     string `json:"continue"`
}

type listCursorCodec struct {
	aead     cipher.AEAD
	identity metadataIdentity
}

func newListCursorCodec(identity metadataIdentity) (listCursorCodec, error) {
	mac := hmac.New(sha256.New, identity.secret)
	_, _ = mac.Write([]byte(listCursorDomain))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return listCursorCodec{}, fmt.Errorf("create list cursor cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return listCursorCodec{}, fmt.Errorf("create list cursor AEAD: %w", err)
	}
	return listCursorCodec{aead: aead, identity: identity}, nil
}

func (c listCursorCodec) encode(scope lease.Scope, pool string, limit int, asOf time.Time, continuation string) (string, error) {
	payload := listCursorPayload{
		Version:      listCursorVersion,
		ScopeDigest:  c.identity.scopeHash(scope),
		PoolDigest:   c.poolDigest(pool),
		Limit:        limit,
		AsOfUnixNano: asOf.UTC().UnixNano(),
		Continue:     continuation,
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal list cursor: %w", err)
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("create list cursor nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, []byte(listCursorAAD))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c listCursorCodec) decode(value string, scope lease.Scope, pool string, limit int) (listCursorPayload, error) {
	if value == "" || len(value) > maxCursorBytes {
		return listCursorPayload{}, invalidCursor()
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(sealed) < c.aead.NonceSize()+c.aead.Overhead() {
		return listCursorPayload{}, invalidCursor()
	}
	nonce, ciphertext := sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(listCursorAAD))
	if err != nil {
		return listCursorPayload{}, invalidCursor()
	}
	var payload listCursorPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil ||
		payload.Version != listCursorVersion ||
		payload.ScopeDigest != c.identity.scopeHash(scope) ||
		payload.PoolDigest != c.poolDigest(pool) ||
		payload.Limit != limit ||
		payload.AsOfUnixNano <= 0 ||
		payload.Continue == "" {
		return listCursorPayload{}, invalidCursor()
	}
	return payload, nil
}

func (c listCursorCodec) poolDigest(pool string) string {
	if pool == "" {
		return "none"
	}
	return c.identity.poolHash(pool)
}

func invalidCursor() error {
	return lease.NewError(400, "INVALID_CURSOR", "Invalid list cursor")
}
