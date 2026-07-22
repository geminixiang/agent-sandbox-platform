package kubernetes

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

type metadataIdentity struct{ secret []byte }

func newMetadataIdentity(secret string) (metadataIdentity, error) {
	if secret == "" {
		return metadataIdentity{}, lease.NewError(500, "INVALID_CONFIGURATION", "metadata secret is required")
	}
	return metadataIdentity{secret: []byte(secret)}, nil
}
func (i metadataIdentity) consumerHash(scope lease.Scope) string {
	return i.digest("consumer", scope.ConsumerID)
}
func (i metadataIdentity) scopeHash(scope lease.Scope) string {
	return i.digest("scope", scope.ConsumerID, scope.SubjectID)
}
func (i metadataIdentity) idempotencyHash(scope lease.Scope, key string) string {
	return i.digest("idempotency", scope.ConsumerID, scope.SubjectID, key)
}
func (i metadataIdentity) poolHash(pool string) string {
	return i.idempotencyHash(lease.Scope{ConsumerID: "pool", SubjectID: "pool"}, pool)
}
func (i metadataIdentity) digest(parts ...string) string {
	payload, _ := json.Marshal(parts)
	mac := hmac.New(sha256.New, i.secret)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))[:40]
}
