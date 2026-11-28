// packages/envoy/cmd/cipher/enroll.go
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// ---------------------------------------------------------------------------
// enroll / unenroll (a session key a launcher holds, through the local agent-secrets-helper)
// ---------------------------------------------------------------------------

// cmdEnroll implements the three enrollments this CLI makes.
//
// --helper is the one a launcher asks for: the broker's HTTP API has no launcher bearer token,
// so the host's one launcher credential lives only in agent-secrets-helper's memory (a human
// installs it with `agent-secrets machine login`). It enrolls a session key a launcher keeps
// outside the helper's process tree (agentbox's, for a box); a session the helper registers
// enrolls itself through `agent-secrets register`, and a Legion pod's enrollment is its own
// launcher's job.
//
// --pod is a Kubernetes pod enrolling itself (pod.go): it holds no credential a person
// installed, and proves who it is with the projected ServiceAccount token its own kubelet
// mounts, so it needs no helper and no launcher. It is how a pod an operator launches by hand
// gets a broker identity with no daemon enrolling it.
//
// The two are mutually exclusive: each is a whole, different way of proving the caller may
// enrol.
func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{
		"runtime-id": true, "thumbprint": true, "session-id": true,
		"service": true, "out": true, "pod-token-file": true, "projects": true,
	})
	flags := newFlagSet("enroll", stderr)
	runtimeID := flags.String("runtime-id", "", "this runtime's stable id")
	thumbprint := flags.String("thumbprint", "", "the signing key's RFC 7638 thumbprint")
	sessionID := flags.String("session-id", "", "the agent session the broker notifies if one of this session's pending requests expires")
	helperFlag := flags.Bool("helper", false, "enroll through this machine's agent-secrets-helper, which holds its launcher credential (required without --pod: a launcher credential is a key held only by a helper or a launcher, never a token this CLI could read)")
	podFlag := flags.Bool("pod", false, "enroll this Kubernetes pod with the projected service-account token its own kubelet mounts")
	service := flags.String("service", "", "--pod: the broker-side service this pod's service account is bound to")
	// PrintDefaults appends the default itself, so neither usage names it twice.
	podTokenFile := flags.String("pod-token-file", defaultPodTokenFile, "--pod: a pod's projected service-account token file")
	// --out's default, AGENT_SECRETS_KEY_DIR, is read after Parse rather than given to the flag:
	// PrintDefaults, which -h and every usage error print, would print it, and it is the
	// directory of key.pem, a file that holds a credential.
	out := flags.String("out", "", "--pod: the directory holding key.pem and enrollment; AGENT_SECRETS_KEY_DIR when not given")
	// --projects names the Dispatch projects the session works on, defaulting to
	// AGENT_SECRETS_PROJECTS, as register's does. --helper sends them for the box's session; --pod
	// sends them for the pod, which the broker takes only from a person's pod, the one pod that
	// acts for a person, and refuses from a service's pod (PROJECTS_NEED_A_PERSON).
	projectsFlag := flags.String("projects", "", "the Dispatch projects the session or the person's pod works on, project keys separated by spaces (\"PRIV CORE\"); default "+cipherclient.ProjectsVar)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if !flagGiven(flags, "out") {
		*out = os.Getenv("AGENT_SECRETS_KEY_DIR")
	}
	if len(positional) > 0 {
		return refuseUnexpectedArgument(stderr, "enroll")
	}
	if *podFlag && *helperFlag {
		fmt.Fprintln(stderr, "agent-secrets enroll: --pod and --helper are mutually exclusive: a pod proves who it is with its own projected token, and needs no machine login")
		return exitUsageError
	}
	if *podFlag {
		// --runtime-id, --thumbprint and --session-id are --helper's. A pod's runtime id is the
		// pod UID its own token carries, its thumbprint is the key this command writes, and it
		// registers no agent session, so each of them given here asked for something this mode
		// does not do. Refused by name, as --helper beside --pod is one line above, rather than
		// silently ignored.
		for _, name := range []string{"runtime-id", "thumbprint", "session-id"} {
			if flagGiven(flags, name) {
				fmt.Fprintf(stderr, "agent-secrets enroll: --%s is --helper's and --pod takes none of it: a pod's runtime id is the pod UID of the token it presents, its thumbprint is the key this writes into --out, and it registers no agent session\n", name)
				return exitUsageError
			}
		}
	} else if !*helperFlag {
		fmt.Fprintln(stderr, "agent-secrets enroll: --helper is required (this CLI enrolls a session key only through the local agent-secrets-helper socket; a Kubernetes pod enrolls itself with --pod)")
		return exitUsageError
	}
	// Read for either mode before it asks anyone, so a list that is no list is refused, naming the
	// setting and never the value, before the helper or the broker is asked.
	projects, err := sessionProjectsFlag(*projectsFlag, flags)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return exitUsageError
	}
	if *podFlag {
		// The list goes with whether --projects named it, so a refusal of the list names the
		// setting it was read from (podProjects.refusal).
		return cmdEnrollPod(*service, *podTokenFile, *out, podProjects{keys: projects, flag: flagGiven(flags, "projects")}, stdout, stderr)
	}
	return cmdEnrollHelper(*runtimeID, *thumbprint, *sessionID, projects, stdout, stderr)
}

// cmdEnrollHelper asks the local agent-secrets-helper daemon, over its unix socket, to enroll a
// session key using the machine credential `agent-secrets machine login` installed. It makes
// AGENT_SECRETS_KEY_DIR before it asks, so a directory it cannot make fails before the broker
// makes any enrollment. On success it writes the enrollment id into
// AGENT_SECRETS_KEY_DIR/enrollment (buildSigner reads it back for every later
// session-authenticated subcommand) and prints the enrollment id alone on stdout — the contract
// scripts/agentbox parses.
//
// A failure after the usage checks exits 1. A refusal prints the helper's code and error as they
// came: ENROLL_OUTCOME_UNKNOWN's error says the broker may have made the enrollment. Two failures
// of the command's own come after the broker may have made it, and each says so on stderr. A
// helper that gives no answer (helper.ErrNoReply) leaves it unknown whether the broker made the
// enrollment: the same command again gets the enrollment the broker made, if it made one,
// because the broker answers the same key under the same launcher credential and runtime id with
// its live enrollment. And when the helper answers with an enrollment that this command then
// cannot write, the error names that enrollment's id and the write that failed.
func cmdEnrollHelper(runtimeID, thumbprint, sessionID string, projects []string, stdout, stderr io.Writer) int {
	if runtimeID == "" || thumbprint == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: --helper needs --runtime-id and --thumbprint")
		return exitUsageError
	}
	dir := os.Getenv("AGENT_SECRETS_KEY_DIR")
	if dir == "" {
		fmt.Fprintln(stderr, "agent-secrets enroll: AGENT_SECRETS_KEY_DIR is required")
		return exitUsageError
	}
	// The directory holds key.pem, so it is named by its variable, never by its path, and its
	// errors print the system's reason alone (redact.File).
	const keyDirNamed = "the directory AGENT_SECRETS_KEY_DIR names"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: make %s: %v\n", keyDirNamed, redact.File(err))
		return 1
	}
	sock, _ := helperSocket()
	var sessionIDPtr *string
	if sessionID != "" {
		sessionIDPtr = &sessionID
	}
	resp, err := enrollRetryingDrain(sock, helper.Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: thumbprint, SessionID: sessionIDPtr, Projects: projects})
	if errors.Is(err, helper.ErrNoReply) {
		fmt.Fprintf(stderr, "agent-secrets enroll: the helper gave no answer (%v). So it is not known if the broker made the enrollment. Run the same command again: if the broker made the enrollment, the command gets that enrollment.\n", err)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets enroll: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	path := filepath.Join(dir, enrollmentFile)
	// The file keeps the id as the broker made it, and the lines print it on one line
	// (redact.OneLine), as every broker-answer value this CLI prints does. A write that fails
	// exits 1 and names the enrollment, the file and the directory by its setting, never the
	// path, and the system's reason alone (redact.File): "the broker made enrollment <id>, but
	// this command could not write its id to enrollment in the directory AGENT_SECRETS_KEY_DIR
	// names: <operation>: <reason>".
	if err := writeFileAtomic(path, []byte(resp.EnrollmentID+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "agent-secrets enroll: the broker made enrollment %s, but this command could not write its id to %s in %s: %v\n", redact.OneLine(resp.EnrollmentID), enrollmentFile, keyDirNamed, redact.File(err))
		return 1
	}
	fmt.Fprintln(stdout, redact.OneLine(resp.EnrollmentID))
	return 0
}

// enrollDrainWait is how long `enroll --helper` retries a DRAINING answer (a helper install is
// about to restart the helper): helper.BrokerCallWait, the wait it gives one call, so a box
// launched inside install's drain-to-restart window enrolls with the restarted helper. Only
// tests shorten it.
var enrollDrainWait = helper.BrokerCallWait

// enrollRetryingDrain asks the helper req, again every 250 ms while it answers DRAINING, until
// enrollDrainWait has passed; the last answer is returned as it came. Each call waits
// helper.BrokerCallWait, the wait of a broker-call op.
func enrollRetryingDrain(sock string, req helper.Request) (helper.Response, error) {
	deadline := time.Now().Add(enrollDrainWait)
	for {
		resp, err := helper.Call(sock, req, helper.BrokerCallWait)
		if err != nil || resp.OK || resp.Code != helper.CodeDraining || !time.Now().Before(deadline) {
			return resp, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// cmdUnenroll implements "unenroll --helper --enrollment <id>". --helper is required for the
// same reason cmdEnroll requires it: the broker's HTTP API has no launcher bearer token.
func cmdUnenroll(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"enrollment": true})
	if len(positional) > 0 {
		return refuseUnexpectedArgument(stderr, "unenroll")
	}
	flags := newFlagSet("unenroll", stderr)
	enrollmentID := flags.String("enrollment", "", "the enrollment id to revoke")
	helperFlag := flags.Bool("helper", false, "unenroll through the local agent-secrets-helper socket")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if !*helperFlag {
		fmt.Fprintln(stderr, "agent-secrets unenroll: --helper is required (this CLI unenrolls only through the local agent-secrets-helper socket)")
		return exitUsageError
	}
	if *enrollmentID == "" {
		fmt.Fprintln(stderr, "agent-secrets unenroll: --enrollment is required")
		return exitUsageError
	}
	return cmdUnenrollHelper(*enrollmentID, stderr)
}

// cmdUnenrollHelper asks the local agent-secrets-helper daemon to end an enrollment
// `enroll --helper` made, over its unix socket, using the machine credential `agent-secrets
// machine login` installed. Idempotent: the helper treats the broker's 204 and 404 as done.
// Prints nothing on success, matching the exit-code-only unenroll contract. A helper that gives
// no answer (helper.ErrNoReply) leaves it unknown whether the broker revoked the enrollment, and
// stderr says so and that the same command is safe to run again.
func cmdUnenrollHelper(enrollmentID string, stderr io.Writer) int {
	sock, _ := helperSocket()
	resp, err := helper.Call(sock, helper.Request{Op: "unenroll-session-key", EnrollmentID: enrollmentID}, helper.BrokerCallWait)
	if errors.Is(err, helper.ErrNoReply) {
		fmt.Fprintf(stderr, "agent-secrets unenroll: the helper gave no answer (%v). So it is not known if the broker revoked the enrollment. It is safe to run the same command again.\n", err)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(stderr, "agent-secrets unenroll: %s: %s\n", resp.Code, resp.Error)
		return 1
	}
	return 0
}
