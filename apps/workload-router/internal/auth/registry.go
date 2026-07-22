// Package auth owns the ephemeral prototype exposure, capability, session, and
// active-connection state. Raw credentials are returned once and never stored.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const defaultBootstrapTTL = 60 * time.Second

var (
	ErrUnauthorized    = errors.New("workload router authorization failed")
	ErrConnectionLimit = errors.New("workload router connection limit reached")
)

// Event is intentionally secret-safe. Sinks receive an opaque exposure ID and
// an outcome, but never a capability, cookie, Host, URL, header, or body.
type Event struct {
	ExposureID string
	Type       string
	Outcome    string
}

type EventSink func(Event)

// Registration is trusted control-plane input, not an HTTP admin payload.
type Registration struct {
	ExposureID     string
	Host           string
	ExpiresAt      time.Time
	AllowWebSocket bool
	MaxConnections int
}

// Exposure is the authorization result consumed by the data plane. generation
// is deliberately private: callers can pass an authorization result back to
// the Registry, but cannot manufacture or alter its registration identity.
type Exposure struct {
	ID             string
	Host           string
	ExpiresAt      time.Time
	AllowWebSocket bool
	MaxConnections int
	generation     uint64
}

type exposureRef struct {
	id         string
	generation uint64
}

type capabilityRecord struct {
	exposure  exposureRef
	expiresAt time.Time
}

type sessionRecord struct {
	exposure  exposureRef
	expiresAt time.Time
}

type timer interface {
	Stop() bool
}

type clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) timer
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(delay time.Duration, callback func()) timer {
	return time.AfterFunc(delay, callback)
}

type registryOptions struct {
	bootstrapTTL time.Duration
	clock        clock
}

// Option configures prototype credential timing without changing the simple
// NewRegistry call used by existing callers.
type Option func(*registryOptions)

// WithBootstrapTTL bounds how long a newly issued one-time capability remains
// usable. A capability can never outlive its Exposure.
func WithBootstrapTTL(ttl time.Duration) Option {
	return func(options *registryOptions) {
		if ttl > 0 {
			options.bootstrapTTL = ttl
		}
	}
}

// withClock is an internal deterministic-test seam.
func withClock(value clock) Option {
	return func(options *registryOptions) {
		if value != nil {
			options.clock = value
		}
	}
}

type exposureState struct {
	exposure Exposure
	active   int
	timer    timer
	conns    map[net.Conn]struct{}
}

// Registry is an in-memory prototype registry. Empty process state is deny by
// default, so a restart cannot reactivate an old capability or session.
type Registry struct {
	mu           sync.Mutex
	random       io.Reader
	sessionTTL   time.Duration
	bootstrapTTL time.Duration
	clock        clock
	events       EventSink
	closed       bool
	nextGen      uint64

	exposures    map[string]*exposureState
	hosts        map[string]exposureRef
	capabilities map[[sha256.Size]byte]capabilityRecord
	sessions     map[[sha256.Size]byte]sessionRecord
}

func NewRegistry(sessionTTL time.Duration, events EventSink, options ...Option) *Registry {
	if sessionTTL <= 0 {
		sessionTTL = 15 * time.Minute
	}
	configured := registryOptions{bootstrapTTL: defaultBootstrapTTL, clock: realClock{}}
	for _, option := range options {
		if option != nil {
			option(&configured)
		}
	}
	return &Registry{
		random:       rand.Reader,
		sessionTTL:   sessionTTL,
		bootstrapTTL: configured.bootstrapTTL,
		clock:        configured.clock,
		events:       events,
		exposures:    make(map[string]*exposureState),
		hosts:        make(map[string]exposureRef),
		capabilities: make(map[[sha256.Size]byte]capabilityRecord),
		sessions:     make(map[[sha256.Size]byte]sessionRecord),
	}
}

// Register creates a single-use bootstrap capability and stores only its hash.
func (r *Registry) Register(reg Registration) (string, error) {
	host, err := CanonicalHost(reg.Host)
	if err != nil {
		return "", err
	}
	if reg.ExposureID == "" || len(reg.ExposureID) > 128 {
		return "", fmt.Errorf("invalid exposure ID")
	}
	if !reg.ExpiresAt.After(r.clock.Now()) {
		return "", fmt.Errorf("exposure expiry must be in the future")
	}
	if reg.MaxConnections <= 0 {
		return "", fmt.Errorf("maximum connections must be positive")
	}

	token, digest, err := r.newCredential()
	if err != nil {
		return "", fmt.Errorf("generate bootstrap capability: %w", err)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", fmt.Errorf("registry is closed")
	}
	if _, exists := r.exposures[reg.ExposureID]; exists {
		r.mu.Unlock()
		return "", fmt.Errorf("exposure already exists")
	}
	if _, exists := r.hosts[host]; exists {
		r.mu.Unlock()
		return "", fmt.Errorf("exposure host already exists")
	}
	now := r.clock.Now()
	if !reg.ExpiresAt.After(now) {
		r.mu.Unlock()
		return "", fmt.Errorf("exposure expiry must be in the future")
	}

	r.nextGen++
	generation := r.nextGen
	ref := exposureRef{id: reg.ExposureID, generation: generation}
	state := &exposureState{
		exposure: Exposure{
			ID:             reg.ExposureID,
			Host:           host,
			ExpiresAt:      reg.ExpiresAt,
			AllowWebSocket: reg.AllowWebSocket,
			MaxConnections: reg.MaxConnections,
			generation:     generation,
		},
		conns: make(map[net.Conn]struct{}),
	}
	capabilityExpiry := now.Add(r.bootstrapTTL)
	if reg.ExpiresAt.Before(capabilityExpiry) {
		capabilityExpiry = reg.ExpiresAt
	}
	r.exposures[reg.ExposureID] = state
	r.hosts[host] = ref
	r.capabilities[digest] = capabilityRecord{exposure: ref, expiresAt: capabilityExpiry}
	state.timer = r.clock.AfterFunc(reg.ExpiresAt.Sub(now), func() {
		r.revokeRef(ref, "expired")
	})
	r.mu.Unlock()
	r.emit(Event{ExposureID: reg.ExposureID, Type: "register", Outcome: "allowed"})
	return token, nil
}

// LookupHost returns only live exposure metadata. Unknown, expired, and revoked
// hosts all fail the same way.
func (r *Registry) LookupHost(host string) (Exposure, error) {
	canonical, err := CanonicalHost(host)
	if err != nil {
		return Exposure{}, ErrUnauthorized
	}
	now := r.clock.Now()
	r.mu.Lock()
	ref, ok := r.hosts[canonical]
	state := r.exposures[ref.id]
	if !ok || !stateMatches(state, ref) || !now.Before(state.exposure.ExpiresAt) {
		r.mu.Unlock()
		if stateMatches(state, ref) {
			r.revokeRef(ref, "expired")
		}
		return Exposure{}, ErrUnauthorized
	}
	exposure := state.exposure
	r.mu.Unlock()
	return exposure, nil
}

// Exchange consumes a valid one-time capability and returns a host-bound
// session credential. Invalid, replayed, cross-exposure, and expired inputs are
// deliberately indistinguishable to callers.
func (r *Registry) Exchange(host, capability string) (string, time.Time, error) {
	exposure, err := r.LookupHost(host)
	if err != nil || !validCredentialShape(capability) {
		r.emit(Event{Type: "exchange", Outcome: "denied"})
		return "", time.Time{}, ErrUnauthorized
	}
	ref := refFor(exposure)
	digest := sha256.Sum256([]byte(capability))
	now := r.clock.Now()

	r.mu.Lock()
	record, ok := r.capabilities[digest]
	state := r.exposures[ref.id]
	if !ok || record.exposure != ref || !stateMatches(state, ref) || !now.Before(record.expiresAt) {
		if ok && !now.Before(record.expiresAt) {
			delete(r.capabilities, digest)
		}
		r.mu.Unlock()
		r.emit(Event{ExposureID: exposure.ID, Type: "exchange", Outcome: "denied"})
		return "", time.Time{}, ErrUnauthorized
	}
	delete(r.capabilities, digest)
	r.mu.Unlock()

	session, sessionDigest, err := r.newCredential()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate session credential: %w", err)
	}
	now = r.clock.Now()
	expiresAt := now.Add(r.sessionTTL)
	if exposure.ExpiresAt.Before(expiresAt) {
		expiresAt = exposure.ExpiresAt
	}

	r.mu.Lock()
	state = r.exposures[ref.id]
	if !stateMatches(state, ref) || !now.Before(state.exposure.ExpiresAt) {
		r.mu.Unlock()
		return "", time.Time{}, ErrUnauthorized
	}
	r.sessions[sessionDigest] = sessionRecord{exposure: ref, expiresAt: expiresAt}
	r.mu.Unlock()
	r.emit(Event{ExposureID: exposure.ID, Type: "exchange", Outcome: "allowed"})
	return session, expiresAt, nil
}

// Authenticate validates a session against the request Host and Exposure
// generation.
func (r *Registry) Authenticate(host, session string) (Exposure, error) {
	exposure, err := r.LookupHost(host)
	if err != nil || !validCredentialShape(session) {
		return Exposure{}, ErrUnauthorized
	}
	ref := refFor(exposure)
	digest := sha256.Sum256([]byte(session))
	now := r.clock.Now()
	r.mu.Lock()
	record, ok := r.sessions[digest]
	state := r.exposures[ref.id]
	if !ok || record.exposure != ref || !stateMatches(state, ref) || !now.Before(record.expiresAt) {
		if ok && !now.Before(record.expiresAt) {
			delete(r.sessions, digest)
		}
		r.mu.Unlock()
		return Exposure{}, ErrUnauthorized
	}
	r.mu.Unlock()
	return exposure, nil
}

// Acquire reserves one concurrent request/upgrade slot for exactly the
// registration generation authorized by Authenticate.
func (r *Registry) Acquire(exposure Exposure) (func(), error) {
	ref := refFor(exposure)
	now := r.clock.Now()
	r.mu.Lock()
	state := r.exposures[ref.id]
	if !stateMatches(state, ref) || !now.Before(state.exposure.ExpiresAt) {
		r.mu.Unlock()
		return nil, ErrUnauthorized
	}
	if state.active >= state.exposure.MaxConnections {
		r.mu.Unlock()
		return nil, ErrConnectionLimit
	}
	state.active++
	released := false
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if !released {
			released = true
			if current := r.exposures[ref.id]; stateMatches(current, ref) && current.active > 0 {
				current.active--
			}
		}
		r.mu.Unlock()
	}, nil
}

// TrackConnection makes revocation and expiry actively close upstream HTTP,
// streaming, and WebSocket connections. The returned connection unregisters
// itself exactly once on Close.
func (r *Registry) TrackConnection(exposure Exposure, conn net.Conn) (net.Conn, error) {
	ref := refFor(exposure)
	now := r.clock.Now()
	r.mu.Lock()
	state := r.exposures[ref.id]
	if !stateMatches(state, ref) || !now.Before(state.exposure.ExpiresAt) {
		r.mu.Unlock()
		_ = conn.Close()
		return nil, ErrUnauthorized
	}
	state.conns[conn] = struct{}{}
	r.mu.Unlock()
	return &trackedConn{Conn: conn, close: func() {
		r.mu.Lock()
		if current := r.exposures[ref.id]; stateMatches(current, ref) {
			delete(current.conns, conn)
		}
		r.mu.Unlock()
	}}, nil
}

// Revoke immediately invalidates the current registration for an opaque ID and
// closes its tracked connections.
func (r *Registry) Revoke(exposureID string) {
	r.mu.Lock()
	state := r.exposures[exposureID]
	if state == nil {
		r.mu.Unlock()
		return
	}
	ref := refFor(state.exposure)
	r.mu.Unlock()
	r.revokeRef(ref, "revoked")
}

func (r *Registry) revokeRef(ref exposureRef, outcome string) {
	var conns []net.Conn
	r.mu.Lock()
	state := r.exposures[ref.id]
	if !stateMatches(state, ref) {
		r.mu.Unlock()
		return
	}
	delete(r.exposures, ref.id)
	if r.hosts[state.exposure.Host] == ref {
		delete(r.hosts, state.exposure.Host)
	}
	if state.timer != nil {
		state.timer.Stop()
	}
	for digest, record := range r.capabilities {
		if record.exposure == ref {
			delete(r.capabilities, digest)
		}
	}
	for digest, record := range r.sessions {
		if record.exposure == ref {
			delete(r.sessions, digest)
		}
	}
	for conn := range state.conns {
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	r.emit(Event{ExposureID: ref.id, Type: "revoke", Outcome: outcome})
}

func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	refs := make([]exposureRef, 0, len(r.exposures))
	for _, state := range r.exposures {
		refs = append(refs, refFor(state.exposure))
	}
	r.mu.Unlock()
	for _, ref := range refs {
		r.revokeRef(ref, "shutdown")
	}
}

func (r *Registry) newCredential() (string, [sha256.Size]byte, error) {
	var raw [32]byte
	if _, err := io.ReadFull(r.random, raw[:]); err != nil {
		return "", [sha256.Size]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func (r *Registry) emit(event Event) {
	if r.events != nil {
		r.events(event)
	}
}

func refFor(exposure Exposure) exposureRef {
	return exposureRef{id: exposure.ID, generation: exposure.generation}
}

func stateMatches(state *exposureState, ref exposureRef) bool {
	return state != nil && state.exposure.ID == ref.id && state.exposure.generation == ref.generation
}

// CanonicalHost rejects ambiguous authorities, ports, IP literals, trailing
// dots, and non-DNS hostnames. DNS names are compared case-insensitively.
func CanonicalHost(host string) (string, error) {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "@,/%\\[]:\t\r\n ") {
		return "", fmt.Errorf("invalid exposure host")
	}
	if strings.HasSuffix(host, ".") {
		return "", fmt.Errorf("invalid exposure host")
	}
	host = strings.ToLower(host)
	if _, err := netip.ParseAddr(host); err == nil {
		return "", fmt.Errorf("IP exposure hosts are forbidden")
	}
	if errs := validation.IsDNS1123Subdomain(host); len(errs) != 0 {
		return "", fmt.Errorf("invalid exposure host")
	}
	return host, nil
}

func validCredentialShape(value string) bool {
	if len(value) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil
}

type trackedConn struct {
	net.Conn
	once  sync.Once
	close func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.close)
	return err
}
