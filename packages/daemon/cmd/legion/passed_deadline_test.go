package main

import "github.com/sjawhar/legion/daemon/internal/testwait"

// passedDeadline is testwait.PassedDeadline under the name and the method this package's tests
// call it by (probe_image_test.go's probe of an Oh My Pi that never answers, and
// gh_gitconfig_test.go's git reads that never answer), so the type is written once, beside
// internal/ghroute's help read test, which uses it too.
type passedDeadline struct{ *testwait.PassedDeadline }

func newPassedDeadline() passedDeadline { return passedDeadline{testwait.NewPassedDeadline()} }

func (d passedDeadline) pass() { d.Pass() }
