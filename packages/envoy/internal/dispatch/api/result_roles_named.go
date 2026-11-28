package api

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/metrics"
)

// A result's stored role is the role its session holds on the recording issue (heldRole), never
// the role the request names (dispatch://INFRA-3994). Until every caller stops sending one, a
// request may still name a role: Dispatch does not store it and refuses nothing for it, and it
// counts each such write here and logs it, by whether the session held the role it named. A later
// release refuses a body that names a role, once this counter and this log line show that no
// caller sends one: the counter rises nowhere for 72 hours, and the logs, which span the restarts
// that set the counter back to 0, hold no such line.

// resultRolesNamedMetric is the counter of result writes whose body named a role, labelled held:
// yes where the named role was the one the session held, and no where it was not.
const resultRolesNamedMetric = "envoy_dispatch_result_roles_named_total"

// resultRoleNamedMessage is the INFO line each such write logs once it has committed.
const resultRoleNamedMessage = "dispatch: a result write named its role"

// registerResultRolesNamed adds the counter to registry and answers it. A nil registry, as this
// package's tests run, adds none and answers nil, which counts nothing (metrics.Counter.Add).
func registerResultRolesNamed(registry *metrics.Registry) *metrics.Counter {
	if registry == nil {
		return nil
	}
	return registry.NewCounter(resultRolesNamedMetric,
		"Result writes whose body named a role, by whether the session held it: yes (the named role was the held one) or no (not taken: the held role, or no role, stored). A later release of dispatch://INFRA-3994 refuses a named role once this stays at 0.")
}

// noteResultRoleNamed counts a committed result write on issue whose body named a role and logs
// it with the session that wrote it and whether that session held the named role.
func (s *server) noteResultRoleNamed(issue, session string, held bool) {
	answer := "no"
	if held {
		answer = "yes"
	}
	s.resultRolesNamed.Inc([2]string{"held", answer})
	slog.Info(resultRoleNamedMessage, "issue", issue, "session", session, "held", answer)
}
