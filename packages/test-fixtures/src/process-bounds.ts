// Bounds for a test that starts a real `bun` process and waits for it.
//
// A process started on a loaded machine can take most of Bun's 5 s per-test default before it
// runs a line: one `bun` start took 7.3 s on a dev box at a 1-minute load of about 1035. A test of
// real process I/O carries a bound of its own sized to that work
// (docs/solutions/testing/await-the-event-not-a-tick-budget.md, "Two clock pitfalls"), and a
// bound is three times a measurement taken under load (docs/testing.md).

/** What one `bun` start may take before it runs a line: three times the 7.3 s measured above. */
export const BUN_START_MS = 22_000;
