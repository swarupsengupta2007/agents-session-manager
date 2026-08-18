//go:build !linux

package agents

func flockHeld(string) bool { return false }

func flockHolderPIDs(string) []int { return nil }
