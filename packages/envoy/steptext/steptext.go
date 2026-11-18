// Package steptext holds the form of the agent, the session and the model a Dispatch step record
// names, for a caller outside this module: printable ASCII with no space, from 1 to 128 characters
// (contracts.StepAgentTextPattern, generated from packages/contracts). Dispatch's step write refuses
// text outside it, and the Legion daemon reads it here to leave such text out of the records it
// writes, so the daemon never sends a record that Dispatch would refuse whole.
//
// It exists because the Legion daemon (packages/daemon, module github.com/sjawhar/legion/daemon)
// is a Go module of its own, and Go's internal rule bars it from
// github.com/sjawhar/envoy/internal/contracts, where the form is generated. This package is the one
// public door onto it, so the daemon holds text to the generated form rather than to a copy.
//
// No package of the envoy module imports this one. packages/envoy/docker/Dockerfile copies only
// go.mod, go.sum, cmd, internal and listenfd into the listener image's build context, so an
// internal/... import of this top-level directory would compile here and break that image build.
// An envoy package that needs the form reads internal/contracts directly.
package steptext

import (
	"regexp"

	"github.com/sjawhar/envoy/internal/contracts"
)

var form = regexp.MustCompile(contracts.StepAgentTextPattern)

// Holds reports whether text is in the form a step record's agent, session and model take.
func Holds(text string) bool {
	return form.MatchString(text)
}
