package exchange_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestADaemonSessionIsOnlyAnEntrysFixedOne: a resource server takes a named-scope token of the
// session legion-daemon:<P> with the one project P for the Legion daemon of P (the listener's
// daemonCaller), so the exchange gives that session only as the fixed session of the daemon's own
// entry. A caller that names it itself is refused, whatever names it: a pod of an account entry
// that fixes no session, a workflow, a Google machine, and a registration for a pod. Each of these
// can otherwise carry the named scope and project P. The control is the daemon's own entry, which
// fixes legion-daemon:ACME and trades for it whether its request names the session or not, and the
// same callers naming any other session.
func TestADaemonSessionIsOnlyAnEntrysFixedOne(t *testing.T) {
	set := listed(t)
	daemonOf := func(project string) string { return oauthwire.DaemonSessionPrefix + project }
	release := workflowToken(repository, runRef(workflow), runRef(workflow), ref)
	machine := googleToken(pinnedAccount, true, cloudProject, instanceName)
	for name, refuse := range map[string]func(session string) error{
		"a pod of an entry that fixes no session": func(s string) error {
			_, err := set.Pod(podToken(workerAccount, podUID), s, nil)
			return err
		},
		"a workflow": func(s string) error {
			_, err := set.Workflow(release, s, nil)
			return err
		},
		"a Google machine": func(s string) error {
			_, err := set.Google(machine, s, nil)
			return err
		},
		"a pod-session registration": func(s string) error {
			return set.Registration(workerAccount, podUID, s)
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, session := range []string{daemonOf(project), daemonOf("OTHER")} {
				err := refuse(session)
				if !errors.Is(err, exchange.ErrSessionInvalid) || !strings.Contains(err.Error(), oauthwire.DaemonSessionPrefix) {
					t.Fatalf("%s naming %s = %v, want ErrSessionInvalid saying the %s names are a daemon entry's alone", name, session, err, oauthwire.DaemonSessionPrefix)
				}
			}
			if err := refuse("implementer-g1"); err != nil {
				t.Fatalf("control: %s naming implementer-g1 = %v, want it accepted", name, err)
			}
		})
	}

	for _, named := range []string{"", fixedSession} {
		claims, err := set.Pod(podToken(daemonAccount, podUID), named, nil)
		if err != nil || claims.Session != daemonOf(project) {
			t.Fatalf("control: the daemon's own entry, the request naming %q = %+v, %v; want the session %s it fixes", named, claims, err, daemonOf(project))
		}
	}
}
