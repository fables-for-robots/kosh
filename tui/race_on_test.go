//go:build race

package tui

// slowdown is how much slower code runs in this build: the race
// detector's.
const slowdown = 15
