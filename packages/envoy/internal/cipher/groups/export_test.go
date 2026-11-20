package groups

import "time"

// Settle waits for every read Want started, so a test reads what they did.
func Settle(r *Reader) { r.inflight.Wait() }

// SetEvery makes Run read at every tick of every, so a test sees ticks without waiting an hour.
func SetEvery(r *Reader, every time.Duration) { r.every = every }

// SetReadTimeout makes one group's read give up after timeout, so a test sees a hung read end.
func SetReadTimeout(r *Reader, timeout time.Duration) { r.readTimeout = timeout }
