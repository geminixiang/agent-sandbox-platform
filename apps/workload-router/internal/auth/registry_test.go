package auth

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestCapabilityIsSingleUseExposureBoundAndExpiryBound(t *testing.T) {
	registry := NewRegistry(time.Hour, nil)
	defer registry.Close()
	now := time.Now()
	capabilityA, err := registry.Register(Registration{ExposureID: "a", Host: "a.router.test", ExpiresAt: now.Add(time.Minute), MaxConnections: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Register(Registration{ExposureID: "b", Host: "b.router.test", ExpiresAt: now.Add(time.Minute), MaxConnections: 2})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := registry.Exchange("b.router.test", capabilityA); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-exposure exchange error = %v", err)
	}
	session, expiresAt, err := registry.Exchange("a.router.test", capabilityA)
	if err != nil {
		t.Fatal(err)
	}
	if expiresAt.After(now.Add(time.Minute)) {
		t.Fatalf("session outlives exposure: %v", expiresAt)
	}
	if _, _, err := registry.Exchange("a.router.test", capabilityA); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay error = %v", err)
	}
	if _, err := registry.Authenticate("b.router.test", session); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-exposure session error = %v", err)
	}
	if _, err := registry.Authenticate("a.router.test", session); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapCapabilityHasSeparateShortTTL(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC))
	registry := NewRegistry(time.Hour, nil, withClock(clock))
	defer registry.Close()
	if registry.bootstrapTTL != 60*time.Second {
		t.Fatalf("default bootstrap TTL = %v", registry.bootstrapTTL)
	}
	capability, err := registry.Register(Registration{
		ExposureID: "a", Host: "a.router.test", ExpiresAt: clock.Now().Add(time.Hour), MaxConnections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(61 * time.Second)
	if _, _, err := registry.Exchange("a.router.test", capability); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired bootstrap capability error = %v", err)
	}
}

func TestConfiguredBootstrapTTLIsCappedByExposure(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC))
	registry := NewRegistry(time.Hour, nil, withClock(clock), WithBootstrapTTL(time.Hour))
	defer registry.Close()
	expiresAt := clock.Now().Add(30 * time.Second)
	capability, err := registry.Register(Registration{
		ExposureID: "a", Host: "a.router.test", ExpiresAt: expiresAt, MaxConnections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, sessionExpiry, err := registry.Exchange("a.router.test", capability)
	if err != nil {
		t.Fatal(err)
	}
	if sessionExpiry != expiresAt {
		t.Fatalf("session expiry = %v, want %v", sessionExpiry, expiresAt)
	}
	clock.Advance(31 * time.Second)
	if _, err := registry.Authenticate("a.router.test", session); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired session error = %v", err)
	}
}

func TestRestartDeniesOldCredentials(t *testing.T) {
	registry := NewRegistry(0, nil)
	capability, err := registry.Register(Registration{ExposureID: "a", Host: "a.router.test", ExpiresAt: time.Now().Add(time.Minute), MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := registry.Exchange("a.router.test", capability)
	if err != nil {
		t.Fatal(err)
	}
	registry.Close()

	restarted := NewRegistry(0, nil)
	defer restarted.Close()
	if _, err := restarted.Authenticate("a.router.test", session); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty restarted registry error = %v", err)
	}
}

func TestRevokeClosesTrackedConnectionAndEnforcesLimit(t *testing.T) {
	registry := NewRegistry(0, nil)
	defer registry.Close()
	_, err := registry.Register(Registration{ExposureID: "a", Host: "a.router.test", ExpiresAt: time.Now().Add(time.Minute), MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	exposure, err := registry.LookupHost("a.router.test")
	if err != nil {
		t.Fatal(err)
	}
	release, err := registry.Acquire(exposure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Acquire(exposure); !errors.Is(err, ErrConnectionLimit) {
		t.Fatalf("second acquire error = %v", err)
	}
	release()

	client, server := net.Pipe()
	defer client.Close()
	tracked, err := registry.TrackConnection(exposure, server)
	if err != nil {
		t.Fatal(err)
	}
	registry.Revoke("a")
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("tracked connection remained open")
	}
	_ = tracked.Close()
	if _, err := registry.LookupHost("a.router.test"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked lookup error = %v", err)
	}
}

func TestExpiryClosesTrackedConnectionDeterministically(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC))
	registry := NewRegistry(0, nil, withClock(clock))
	defer registry.Close()
	_, err := registry.Register(Registration{ExposureID: "a", Host: "a.router.test", ExpiresAt: clock.Now().Add(time.Minute), MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	exposure, _ := registry.LookupHost("a.router.test")
	conn := &countingConn{}
	if _, err := registry.TrackConnection(exposure, conn); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	clock.timers[0].Fire()
	if conn.CloseCount() != 1 {
		t.Fatalf("connection close count = %d", conn.CloseCount())
	}
}

func TestRegistrationGenerationPreventsABAInterleavings(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC))
	registry := NewRegistry(0, nil, withClock(clock), WithBootstrapTTL(time.Hour))
	defer registry.Close()

	oldCapability, err := registry.Register(Registration{ExposureID: "same", Host: "same.router.test", ExpiresAt: clock.Now().Add(time.Hour), MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	oldTimer := clock.timers[0]
	oldSession, _, err := registry.Exchange("same.router.test", oldCapability)
	if err != nil {
		t.Fatal(err)
	}
	oldExposure, err := registry.Authenticate("same.router.test", oldSession)
	if err != nil {
		t.Fatal(err)
	}
	oldRelease, err := registry.Acquire(oldExposure)
	if err != nil {
		t.Fatal(err)
	}
	conn := &countingConn{}
	oldTracked, err := registry.TrackConnection(oldExposure, conn)
	if err != nil {
		t.Fatal(err)
	}

	registry.Revoke("same")
	newCapability, err := registry.Register(Registration{ExposureID: "same", Host: "same.router.test", ExpiresAt: clock.Now().Add(2 * time.Hour), MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	newSession, _, err := registry.Exchange("same.router.test", newCapability)
	if err != nil {
		t.Fatal(err)
	}
	newExposure, err := registry.Authenticate("same.router.test", newSession)
	if err != nil {
		t.Fatal(err)
	}
	newRelease, err := registry.Acquire(newExposure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.TrackConnection(newExposure, conn); err != nil {
		t.Fatal(err)
	}

	// These callbacks all belong to the old registration. Fire the stopped
	// timer as if its callback had already won the Stop race.
	oldRelease()
	_ = oldTracked.Close()
	oldTimer.Fire()

	if _, err := registry.Authenticate("same.router.test", oldSession); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old session authenticated against new generation: %v", err)
	}
	if _, err := registry.Authenticate("same.router.test", newSession); err != nil {
		t.Fatalf("new generation was revoked by old timer: %v", err)
	}
	if _, err := registry.Acquire(newExposure); !errors.Is(err, ErrConnectionLimit) {
		t.Fatalf("old release changed new active count: %v", err)
	}
	registry.Revoke("same")
	newRelease()
	if conn.CloseCount() != 3 {
		t.Fatalf("old close callback untracked new connection; close count = %d", conn.CloseCount())
	}
}

func TestCanonicalHostRejectsAmbiguity(t *testing.T) {
	invalid := []string{"", "a.router.test:443", "a.router.test.", "127.0.0.1", "[::1]", "a.router.test,evil.test", "user@a.router.test", " a.router.test"}
	for _, host := range invalid {
		if _, err := CanonicalHost(host); err == nil {
			t.Errorf("CanonicalHost(%q) succeeded", host)
		}
	}
	if got, err := CanonicalHost("A.Router.Test"); err != nil || got != "a.router.test" {
		t.Fatalf("canonical host = %q, %v", got, err)
	}
}

type fakeClock struct {
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }
func (c *fakeClock) Now() time.Time         { return c.now }
func (c *fakeClock) Advance(duration time.Duration) {
	c.now = c.now.Add(duration)
}
func (c *fakeClock) AfterFunc(_ time.Duration, callback func()) timer {
	timer := &fakeTimer{callback: callback}
	c.timers = append(c.timers, timer)
	return timer
}

type fakeTimer struct {
	callback func()
	stopped  bool
}

func (t *fakeTimer) Stop() bool {
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

// Fire intentionally runs stopped timers to model a callback already racing
// with Timer.Stop.
func (t *fakeTimer) Fire() { t.callback() }

type countingConn struct {
	mu     sync.Mutex
	closed int
}

func (c *countingConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (c *countingConn) Write(buffer []byte) (int, error) { return len(buffer), nil }
func (c *countingConn) Close() error {
	c.mu.Lock()
	c.closed++
	c.mu.Unlock()
	return nil
}
func (c *countingConn) LocalAddr() net.Addr              { return dummyAddr("local") }
func (c *countingConn) RemoteAddr() net.Addr             { return dummyAddr("remote") }
func (c *countingConn) SetDeadline(time.Time) error      { return nil }
func (c *countingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *countingConn) SetWriteDeadline(time.Time) error { return nil }
func (c *countingConn) CloseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }
