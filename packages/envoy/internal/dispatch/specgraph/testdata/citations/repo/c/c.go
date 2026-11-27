// Package c imports a, so a test of c compiles a too.
package c

import "example.com/citations/a"

// Twice is twice a.Answer.
func Twice() int { return 2 * a.Answer() }
