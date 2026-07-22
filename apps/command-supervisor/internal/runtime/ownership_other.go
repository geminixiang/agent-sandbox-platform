//go:build !linux

package runtime

// Non-Linux support exists only for portable protocol/state tests. Production
// startup is rejected by cmd/supervisor before constructing a Manager.
func ensureRootOwned(path string) error { return nil }
