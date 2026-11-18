// packages/envoy/cmd/agent-secrets/register.go
//
// cmdRegister implements "agent-secrets register [--wait SECONDS] [--session-id ID] [--exec --
// COMMAND [ARGS...]]": the one command a machine session (not an agent box) runs at launch to
// become a registered session root with agent-secrets-helper. With --exec the helper pins THIS
// process by pidfd, and the command execs as the very pid that was registered — shims/omp wraps
// omp this way, and omp becomes the session root every later `agent-secrets` invocation in its
// process tree signs through. Without --exec the command names the session it runs inside, or
// the launcher above it, and the helper never pins this process, which ends with the command.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// helperConnectPatience is how long cmdRegister retries connecting to a still-starting helper
// before giving up (or, under --exec, warning and launching anyway) — the launch-ordering case
// tmux-resurrect hits at boot.
const helperConnectPatience = 10 * time.Second

func cmdRegister(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("register", stderr)
	wait := flags.Int("wait", 0, "wait up to this many seconds for the helper to enroll the session (0 returns at once, as does a helper holding no launcher credential)")
	doExec := flags.Bool("exec", false, "after registering, exec the command that follows as this same process")
	// --session-id is what makes a broker identity outlive the process. Without it the helper
	// names the session <hostname>:<pid>:<start ticks since boot>, which is a different string
	// after a restart and necessarily a different one after a reboot, so the session enrols
	// afresh and the requests it had pending and the grants it had been given — all bound to the
	// enrollment id it no longer has — are cancelled with the old enrollment. With it the helper
	// names the session `session-<id>`, keeps its key, and hands the same identity back to the
	// same session id when it comes back, within DormantGrace (dispatch://INFRA-1167).
	//
	// A launcher that knows the id only once the agent has started (omp opens its transcript
	// seconds in, so only a resumed session's id is in the launch argv) registers without one and
	// runs `agent-secrets register --session-id <id>` again from inside the session; the helper
	// takes the name then, at a point where no grant exists to lose.
	//
	// The helper hands back an identity it kept on this box; the broker carries the rest. A
	// session's enrolment states this id (helper.Broker.EnrollIdentity), and the broker gives a
	// new enrolment of the same name under the same principal the grants and the pending
	// requests of the one before it, so the session keeps them even where its key and its
	// runtime id are new: another box, a container, a machine rebuilt from nothing.
	sessionID := flags.String("session-id", "", "the omp session id this session keeps its broker identity under, across a restart of the helper or the machine")
	if err := flags.Parse(args); err != nil {
		return exitUsage(err)
	}
	command := flags.Args()
	if *doExec && len(command) == 0 {
		fmt.Fprintln(stderr, "agent-secrets register --exec needs -- COMMAND [ARGS...]")
		return exitUsageError
	}
	if !*doExec && len(command) > 0 {
		fmt.Fprintf(stderr, "agent-secrets register: unexpected argument %q\n", command[0])
		return exitUsageError
	}

	// --exec looks its command up before it asks the helper anything, so a command that is not on
	// PATH makes no enrolment for a session that never starts.
	var path string
	if *doExec {
		var err error
		if path, err = exec.LookPath(command[0]); err != nil {
			fmt.Fprintf(stderr, "agent-secrets register: %v\n", err)
			return exitNotFound
		}
	}

	// The helper's socket as every other helper-mode form finds it (identity.go): the variable when
	// set, else the default socket when it is there. An agent box has neither: its key dir sits
	// where the default socket's directory would be, and no helper listens in it.
	sock, named := helperSocket()
	if !named && !exists(sock) {
		fmt.Fprintf(stderr, "agent-secrets register: AGENT_SECRETS_HELPER_SOCK is unset and no helper socket is at %s (machine sessions only; an agent box has a key dir instead)\n", sock)
		return exitUsageError
	}

	// Without --exec the register command's own process ends with the command, so it attaches:
	// the helper never pins it. It names, as the root to pin, the nearest
	// session launcher above it (an `omp`), or none when it finds none. Only the helper knows
	// whether the caller is inside a session already (one `register --exec -- claude` started,
	// say), so it decides: it answers that session, else pins the launcher named, else refuses.
	// With --exec nothing of this runs: the caller's pid is the process --exec replaces, which is
	// the session's root.
	req := helper.Request{WaitSeconds: *wait}
	if *sessionID != "" {
		req.SessionID = sessionID
	}
	var launcherErr error
	if !*doExec {
		req.Attach = true
		req.RootPID, launcherErr = nearestLauncher()
	}

	resp, err := registerWithPatience(sock, req, helperConnectPatience)
	if !*doExec {
		if err != nil {
			fmt.Fprintf(stderr, "agent-secrets register: %v\n", err)
			if launcherErr != nil && !errors.Is(launcherErr, errNoLauncher) {
				fmt.Fprintf(stderr, "agent-secrets register: it named no session launcher, since finding one above this process failed: %v\n", launcherErr)
			}
			return 1
		}
		// The session's root must be above this process: a helper from before this fix pins the
		// caller, which ends with the command, and names no root, and a root that is not an
		// ancestor is another session's. Neither's `enrolled` is printed as success.
		if resp.RootPID == 0 {
			fmt.Fprintf(stderr, "agent-secrets register: the agent-secrets-helper at %s did not attach the session to its launcher (it is from before this fix); restart it on this release, or relaunch the session under agent-secrets register --exec -- omp …\n", sock)
			return 1
		}
		if above, err := isAncestor(resp.RootPID); !above {
			why := ""
			if err != nil {
				why = fmt.Sprintf(" (reading this process's ancestry: %v)", err)
			}
			fmt.Fprintf(stderr, "agent-secrets register: the agent-secrets-helper at %s answered pid %d as the session's root, which is not an ancestor of this process%s, so that session is not this one; relaunch the session under agent-secrets register --exec -- omp …\n", sock, resp.RootPID, why)
			return 1
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", resp.RuntimeID, resp.EnrollmentID, resp.State)
		if *wait > 0 && resp.State != "enrolled" {
			fmt.Fprintf(stderr, "agent-secrets register: not enrolled yet: %s\n", resp.Error)
			return 1
		}
		return 0
	}
	// --exec never blocks a launch on the broker, but never launches silently either: the agent
	// starts with no secrets access (helper unreachable), with broker calls that fail NOT_ENROLLED
	// until the helper's enroll loop succeeds, or, while the helper holds no launcher credential,
	// with no broker identity at all: its agent-secrets calls fail NO_CREDENTIAL, and identity
	// exits 1, until the machine is logged in.
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "agent-secrets: helper at %s unreachable (%v); this session has no secrets access until it is relaunched with the helper running\n", sock, err)
	case *wait > 0 && resp.State != "enrolled" && resp.Code == helper.CodeNoCredential && resp.EnrollmentID != "":
		// A session that came back to an identity its record kept holds that enrollment and
		// can sign with it, and the renew that confirms it is signed by that same key, so a
		// login is not what stands between this session and its own enrollment. What the
		// helper cannot do without one is make a REPLACEMENT if the broker has ended it.
		fmt.Fprintln(stderr, "agent-secrets register: this machine is not logged in to the secrets broker; launching anyway with the enrollment this session had, which the helper cannot replace if the broker has ended it until it is (run: agent-secrets machine login)")
	case *wait > 0 && resp.State != "enrolled" && resp.Code == helper.CodeNoCredential:
		fmt.Fprintln(stderr, "agent-secrets register: this machine is not logged in to the secrets broker; launching anyway, and until it is (run: agent-secrets machine login) this session's agent-secrets calls fail")
	case *wait > 0 && resp.State != "enrolled":
		fmt.Fprintf(stderr, "agent-secrets register: not enrolled yet (%s); launching anyway, and this session's secrets calls fail until the helper enrolls it\n", resp.Error)
	}
	if err := syscall.Exec(path, command, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "agent-secrets register: exec: %v\n", err)
		return exitCannotRun
	}
	return 0 // unreachable: syscall.Exec replaces this process image on success
}

// registerWithPatience retries the connection while the helper is absent (it may be starting
// with the user manager at boot), for patience at most, and retries a DRAINING answer (a helper
// install is about to restart) until the larger of patience and the --wait seconds has passed, so
// the session registers with the restarted helper rather than launching with no broker identity.
// req is the register request, its op set here.
func registerWithPatience(sock string, req helper.Request, patience time.Duration) (helper.Response, error) {
	req.Op = "register"
	start := time.Now()
	deadline := start.Add(patience)
	drainDeadline := start.Add(max(patience, time.Duration(req.WaitSeconds)*time.Second))
	for {
		resp, err := helper.Call(sock, req, time.Duration(req.WaitSeconds+10)*time.Second)
		if err == nil {
			if !resp.OK {
				if resp.Code == helper.CodeDraining && time.Now().Before(drainDeadline) {
					time.Sleep(250 * time.Millisecond)
					continue
				}
				return resp, fmt.Errorf("%s: %s", resp.Code, resp.Error)
			}
			return resp, nil
		}
		if time.Now().After(deadline) {
			return helper.Response{}, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}
