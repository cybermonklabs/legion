package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// test_model.go is a test process's choice of model for one request (INFRA-3231's harness,
// cmd/writing-check/harness): the Dispatch-Test-Model header names one of the models
// DISPATCH_TEST_MODELS lists (Deps.TestModels), and the reviewer's draft, a person's Rewrite of an
// ask and a person's Define call that model, the detector's reader included, in place of their own
// model group. The harness scores each change with each candidate model against one server this
// way. A deployment runs without the test hooks, so it refuses the header: a request that names a
// test model must never be answered by the deployment's own model.

// TestModelHeader is the header that names a model of DISPATCH_TEST_MODELS for one request.
const TestModelHeader = "Dispatch-Test-Model"

// requestModel is the model a request's model calls use: base, the model group of the route, or
// the test model the request's Dispatch-Test-Model header names, with base's bounds. A request
// with the header is refused when the server runs without the test hooks, and when the header
// names a model DISPATCH_TEST_MODELS does not list.
func (s *server) requestModel(r *http.Request, base reviewer.Model) (reviewer.Model, error) {
	name := strings.TrimSpace(r.Header.Get(TestModelHeader))
	if name == "" {
		return base, nil
	}
	if !s.deps.TestHooksEnabled {
		return reviewer.Model{}, errorf(http.StatusBadRequest, "TEST_MODEL_REFUSED",
			"the %s header names a test model, and this server runs without its test hooks (DISPATCH_TEST_HOOKS=1)", TestModelHeader)
	}
	chosen, ok := s.deps.TestModels[name]
	if !ok {
		return reviewer.Model{}, errorf(http.StatusBadRequest, "TEST_MODEL_REFUSED",
			"the %s header names %q, which DISPATCH_TEST_MODELS does not list", TestModelHeader, name)
	}
	base.Model, base.Pace = chosen.Model, chosen.Pace
	return base, nil
}
