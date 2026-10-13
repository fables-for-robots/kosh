//go:build !race

package tui

// slowdown is how much slower code runs in this build than in a plain
// one.
const slowdown = 1
