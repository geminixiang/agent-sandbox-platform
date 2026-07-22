//go:build !linux

package main

func childSecurityProbeMode() (bool, int) { return false, 0 }
func capabilityProbeMode() (bool, int)    { return false, 0 }
