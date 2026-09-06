//go:build !race

package media

// buildRace is true when the race detector is compiled in. Timing-sensitive
// realtime tests use it to skip: race instrumentation multiplies the cost of
// the read path and breaks real-time pacing assumptions.
const buildRace = false
