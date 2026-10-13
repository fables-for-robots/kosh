//go:build !race

package tui

// textRaceFactor is how many times longer the tests of time give the
// text views under the race detector.
const textRaceFactor = 1
