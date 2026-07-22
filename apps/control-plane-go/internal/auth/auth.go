package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

const unauthorizedMessage = "Invalid or expired subject token"

type SecretResolver func(consumerID string) (string, bool)

type claims struct {
	ConsumerID string `json:"consumerId"`
	SubjectID  string `json:"subjectId"`
	ExpiresAt  int64  `json:"exp"`
}

func VerifySubjectToken(token string, resolve SecretResolver, now time.Time, maxTTL time.Duration) (lease.Scope, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return lease.Scope{}, unauthorized()
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return lease.Scope{}, unauthorized()
	}
	var value claims
	if json.Unmarshal(payload, &value) != nil || !validIdentity(value.ConsumerID) || !validIdentity(value.SubjectID) {
		return lease.Scope{}, unauthorized()
	}
	nowSeconds := now.Unix()
	if value.ExpiresAt <= nowSeconds || value.ExpiresAt > now.Add(maxTTL).Unix() {
		return lease.Scope{}, unauthorized()
	}

	secret, ok := resolve(value.ConsumerID)
	if !ok || secret == "" {
		return lease.Scope{}, unauthorized()
	}
	expected := signature(parts[0]+"."+parts[1], secret)
	if !hmac.Equal([]byte(parts[2]), []byte(expected)) {
		return lease.Scope{}, unauthorized()
	}
	return lease.Scope{ConsumerID: value.ConsumerID, SubjectID: value.SubjectID}, nil
}

func signature(value, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validIdentity(value string) bool {
	return len(value) > 0 && len(value) <= 200 && strings.TrimSpace(value) != ""
}

func unauthorized() error { return lease.NewError(401, "UNAUTHORIZED", unauthorizedMessage) }
