// packages/envoy/cmd/cipher/identity.go
//
// Where this process's session identity lives, and `agent-secrets identity`, the one local test
// of whether it has one. An agent box or pod keeps its key under AGENT_SECRETS_KEY_DIR (file
// mode); a machine session asks agent-secrets-helper over AGENT_SECRETS_HELPER_SOCK (helper mode).
// A process can be inside its session with neither variable set — omp's eval kernel starts every
// run with a filtered environment that drops AGENT_SECRETS_* — so each falls back to the path its
// launcher uses: a box's key dir is $XDG_RUNTIME_DIR/agent-secrets (scripts/agentbox mounts it
// there), and the helper's socket is helper.DefaultSocket, in that same directory on the host,
// where the helper keeps no key.pem. A variable that is set is used as given; a default is used
// only when its file is there.
//
// A box's key and enrollment arrive after the box starts: its launcher runs keygen inside it and
// enrolls it once omp's session id appears, while omp is already starting MCP servers whose first
// call needs them. The launcher marks that window explicitly, writing enrollment.pending into the
// key dir before the box starts and removing it once enrollment is written, or once it has
// written enrollment.error. A call waits only while that marker is there and fresh; every other
// state fails as fast as it always did.
package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

const (
	// keyFile and enrollmentFile are a box's or pod's identity in its key dir: the key keygen
	// writes, and the enrollment id enroll (or the pod's launcher) writes beside it.
	keyFile        = "key.pem"
	enrollmentFile = "enrollment"
	// enrollmentErrorFile is what whoever wrote the key records a refused enrollment in, beside
	// it: key.pem with no enrollment is the one state selectSigner cannot read on its own, since
	// it takes the key dir's branch on the key alone and then finds no id. Its content is the
	// refusal, which selectSigner appends to what it answers, so a later call says what the
	// broker said rather than naming a missing file.
	enrollmentErrorFile = "enrollment.error"
	// machineSessionMarker says this key dir's key and enrollment are a session this command
	// registered for a machine that proves an identity of its own, which is what makes the
	// enrollment one this command may register again when the broker says it is not live. A
	// launcher's key dir holds no such file: its enrollment is the launcher's to make again.
	machineSessionMarker = "machine-session"
	// pendingMarker is the file a box's launcher keeps in the key dir while it sets the box up.
	pendingMarker = "enrollment.pending"
	// pendingMarkerLife bounds how long a client trusts a marker, by its mtime. It covers the
	// launcher's usual setup (120 s for omp's session id, ten enrollment attempts about 3 s apart,
	// and keygen), not its worst case: against a slow broker the launcher can keep retrying for
	// about 450 s. A setup slower than this bound, or a marker a dead launcher left, makes a call
	// fail fast with the message it would give without the marker.
	pendingMarkerLife = 160 * time.Second
	// defaultEnrollWait bounds how long a call waits on a live marker (AGENT_SECRETS_ENROLL_WAIT
	// overrides it). It stays under omp's 30 s MCP connect timeout: an MCP server's first call is
	// the one that meets the window, and omp never retries a server that failed to start.
	defaultEnrollWait  = 20 * time.Second
	enrollPollInterval = 100 * time.Millisecond
	// identityDeadline bounds identity's one question to the helper, dial included.
	identityDeadline = 2 * time.Second
	// identityProbeURL is the htu of the proof identity asks the helper for. It is not a URL, so
	// the proof matches no broker route and is worthless if it ever leaked; identity discards it
	// unread.
	identityProbeURL = "agent-secrets:identity"
	// brokerURLVar is the one variable every form takes the broker's address from. A refusal
	// that could not reach the broker names it, so a session that inherited a stale address
	// reads a stale address rather than a broker that is down.
	brokerURLVar = "AGENT_SECRETS_URL"
)

// keyDir is AGENT_SECRETS_KEY_DIR, else the default $XDG_RUNTIME_DIR/agent-secrets — the
// directory helper.DefaultSocket names, so both defaults read XDG_RUNTIME_DIR one way.
func keyDir() string {
	if dir := os.Getenv("AGENT_SECRETS_KEY_DIR"); dir != "" {
		return dir
	}
	return filepath.Dir(helper.DefaultSocket(os.Getenv))
}

// keyDirName is how a message names the key dir (keyDir): AGENT_SECRETS_KEY_DIR where that is set,
// else its default's own expression as helper.DefaultSocket resolves it - under XDG_RUNTIME_DIR,
// or under /run/user/$UID where that is unset too - never the directory's path. The key dir holds
// key.pem, and a setting meant for a credential's place is where a person who pasted the
// credential puts it.
func keyDirName() string {
	switch {
	case os.Getenv("AGENT_SECRETS_KEY_DIR") != "":
		return "AGENT_SECRETS_KEY_DIR"
	case os.Getenv("XDG_RUNTIME_DIR") != "":
		return "$XDG_RUNTIME_DIR/agent-secrets (AGENT_SECRETS_KEY_DIR is unset)"
	default:
		return "/run/user/$UID/agent-secrets (AGENT_SECRETS_KEY_DIR and XDG_RUNTIME_DIR are unset)"
	}
}

// keyDirFile is how a message names the file name in the key dir: name in keyDirName.
func keyDirFile(name string) string { return name + " in " + keyDirName() }

// brokerBaseURL is the broker's address as brokerURLVar gives it (clientGetenv: the environment,
// else the settings file), without the slashes it ends in (cipherclient.WithoutTrailingSlashes,
// the rule both halves of this client read an address by), and the empty Address when neither
// gives one or it holds nothing but slashes: the one place every form reads it, and as a
// cipheraddr.Address, so no form holds it as a string it could print.
func brokerBaseURL() cipheraddr.Address {
	return cipheraddr.New(cipherclient.WithoutTrailingSlashes(clientGetenv(brokerURLVar)))
}

// helperSocket is AGENT_SECRETS_HELPER_SOCK, else the helper's default socket; named reports
// which, since a default counts only when the socket is there.
func helperSocket() (sock string, named bool) {
	if sock := os.Getenv("AGENT_SECRETS_HELPER_SOCK"); sock != "" {
		return sock, true
	}
	return helper.DefaultSocket(os.Getenv), false
}

// approveURL is AGENT_SECRETS_APPROVE_URL, Dispatch's origin, without the slashes it ends in (the
// same cipherclient.WithoutTrailingSlashes every other address here is read by): the base of the
// machine-login page (/credentials/machine) where `machine login` sends a person to type its code
// (machine.go). That page is the only thing this setting names: a pending secret request names
// the page on the secrets broker its own answer carries as approve_url, and reads nothing from
// here. Empty when unset.
func approveURL() string {
	return cipherclient.WithoutTrailingSlashes(os.Getenv("AGENT_SECRETS_APPROVE_URL"))
}

// requestSessionID answers the session id every request and exec form sends with its request
// (LEGION-587): OMP_SESSION_ID, the id Oh My Pi's session-env extension sets, when present; else
// ENVOY_SESSION_ID, an Envoy-registered harness that does not set OMP_SESSION_ID; else
// CLAUDE_CODE_SESSION_ID, which Claude Code's Bash tool and hooks re-read fresh at every spawn
// and update on /clear (code.claude.com/docs/en/env-vars, "CLAUDE_CODE_SESSION_ID"), so a Claude
// Code session names the same id it registered with Envoy under even across a /clear — only its
// own long-lived MCP server keeps the id it was first spawned with. A Claude Code session
// started from an Oh My Pi shell still names the outer Oh My Pi session: OMP_SESSION_ID is
// inherited from that shell and wins over CLAUDE_CODE_SESSION_ID in the order above. Empty when
// none of the three is set — a session agent-secrets-helper enrolled for itself carries no
// enrollment session_id of its own, so an unset result here is what the broker's requests.
// Session answers for both Request and Enrollment, and the credential request names no session
// at all.
//
// The Codex CLI sets CODEX_THREAD_ID in every shell command it runs. A shell that
// carries it beside another session's id, because a Codex session was started from a Claude Code
// or Oh My Pi shell or the other way round, names whichever of `codex`, `claude` and `omp` is
// this process's nearest ancestor; where the walk finds none of them, the order above, with
// CODEX_THREAD_ID after CLAUDE_CODE_SESSION_ID, as before Codex was a host.
func requestSessionID() string {
	codex := os.Getenv("CODEX_THREAD_ID")
	others := cmp.Or(os.Getenv("OMP_SESSION_ID"), os.Getenv("ENVOY_SESSION_ID"), os.Getenv("CLAUDE_CODE_SESSION_ID"))
	if codex != "" && (others == "" || nearestHarness("codex", "claude", "omp") == "codex") {
		return codex
	}
	return others
}

// harnessName is the name nearestHarness compares for a process's comm: the command name, with a
// trailing ".exe" taken off.
//
// A name is the base name of the binary the process executed, cut to 15 characters, with a
// trailing ".exe" taken off: Claude Code's npm package runs its binary as bin/claude.exe. A host
// started through an interpreter (an npm install that runs as node) has the interpreter's name,
// so the walk passes it and may stop at a host further out, and a Claude Code run that way inside
// a Codex shell names the Codex thread. dispatch's walk (nearestAncestorNamed,
// packages/envoy-client/src/process-ancestry.ts) reads names by the same rule.
func harnessName(comm string) string {
	return strings.TrimSuffix(strings.TrimSpace(comm), ".exe")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// freshMarker reports whether dir holds a launcher's enrollment.pending younger than
// pendingMarkerLife.
func freshMarker(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, pendingMarker))
	return err == nil && time.Since(info.ModTime()) < pendingMarkerLife
}

// launcherKeyDir reports whether dir is a launcher's key dir, which puts the process that reads
// it inside a session: dir holds a key.pem with no machine-session marker beside it, or a fresh
// enrollment.pending while the launcher is still enrolling it. A key.pem with the marker is a
// machine's own session that a client before #4143 left there, not a launcher's key, so it
// puts no process inside a session. identity and grant revoke both read the key dir by this rule.
func launcherKeyDir(dir string) bool {
	return (exists(filepath.Join(dir, keyFile)) && !exists(filepath.Join(dir, machineSessionMarker))) || freshMarker(dir)
}

// enrollWait is AGENT_SECRETS_ENROLL_WAIT (a Go duration), else defaultEnrollWait. A value that is
// no duration of zero or more is refused naming the variable and its rule, never the value, which
// the environment holds whatever was put there.
func enrollWait() (time.Duration, error) {
	v := os.Getenv("AGENT_SECRETS_ENROLL_WAIT")
	if v == "" {
		return defaultEnrollWait, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, errors.New("AGENT_SECRETS_ENROLL_WAIT is not a Go duration of zero or more, such as 20s")
	}
	return d, nil
}

// awaitEnrollment waits, for wait at most, while dir holds a fresh marker and not yet both
// key.pem and enrollment. With a wait above 0 it says on stderr what it waits for before it
// waits, and again each time that changes (the launcher wrote key.pem and not yet enrollment), so
// a call held at a box's start says why it is held and how far the launcher got; it names the key
// dir by its setting (keyDirName), never its path. It says so before it reads its deadline, so a
// call that gives up has always first said what it waited for, however late a loaded machine
// runs it; a wait of 0 says nothing. It reports whether it gave up with the marker still there,
// and then how long it waited, from the moment it set its deadline: a loaded machine's late start
// of the call is not in it, so a caller that prints it says how long this wait lasted.
func awaitEnrollment(stderr io.Writer, dir string, wait time.Duration) (waited time.Duration, gaveUp bool) {
	begin := time.Now()
	deadline := begin.Add(wait)
	said := ""
	for freshMarker(dir) {
		missing := missingSessionFiles(dir)
		if missing == "" {
			return 0, false
		}
		if wait > 0 {
			switch {
			case said == "":
				fmt.Fprintf(stderr, "agent-secrets: the launcher is still enrolling this process (%s holds %s); waiting up to %s for its %s\n",
					keyDirName(), pendingMarker, wait, missing)
			case missing != said:
				fmt.Fprintf(stderr, "agent-secrets: still waiting for the launcher's %s\n", missing)
			}
			said = missing
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return time.Since(begin), true
		}
		time.Sleep(min(enrollPollInterval, remaining))
	}
	return 0, false
}

// missingSessionFiles names the files of key.pem and enrollment that dir does not hold yet, as
// "key.pem and enrollment", "key.pem" or "enrollment", and "" when it holds both.
func missingSessionFiles(dir string) string {
	var missing []string
	for _, name := range []string{keyFile, enrollmentFile} {
		if !exists(filepath.Join(dir, name)) {
			missing = append(missing, name)
		}
	}
	return strings.Join(missing, " and ")
}

// helperInstalled reports whether this machine has the helper's user unit, the file
// `agent-secrets-helper install` writes and the agent launchers' shims also read as "the helper is installed",
// under this user's home (homeGetenv). Its socket being absent then means the helper is stopped
// (a clean stop removes the socket), not that this machine has none.
func helperInstalled() bool {
	return exists(helper.UnitPath(homeGetenv))
}

// cmdIdentity implements "identity": exit 0 when this process has a broker identity, 1 when it
// does not, without the network and without changing anything. It never prints to stdout, so a
// caller can put it in front of a command whose stdout is the caller's own (dotfiles'
// secret-run, legion's envoy scripts), and it never registers anything: `register` from an
// unregistered process would create a session, and self or whoami would ask the broker.
//
// A process has an identity when its key dir holds a launcher's key.pem (one with no
// machine-session marker beside it, which a client before #4143 left for a machine's own
// session) or a fresh enrollment.pending, or when the helper's sign op — which signs a throwaway
// proof and changes nothing — answers OK (enrolled) or NOT_ENROLLED (registered and still
// enrolling) for it. Such a process uses the broker even if its enrollment later fails, and the
// failure is the broker call's. It reads neither a person's sign-in nor the machine cache, and
// asks no metadata server: those are the chain's later sources, which --source reports. A
// registered session on a helper holding no launcher credential (NO_CREDENTIAL: never logged in,
// its login lapsed or was refused, or, in identity mode, its trade loop has not traded or has
// stopped) has none, since that helper enrolls no one; it is exit 1, quoting the helper's own
// answer, which names the cure of its mode (cipherclient.Hint). A helper that cannot be asked
// — its socket refuses connections, is absent although AGENT_SECRETS_HELPER_SOCK names it or the
// helper is installed, or gives no answer within identityDeadline — is exit 1 with a notice on
// stderr, so a caller that then uses its other backend does not do so silently. With no key dir,
// no socket named or present, and no helper installed (a laptop), it is exit 1 and silent.
//
// With --source it walks the chain the use command walks (chain.go), may ask the broker and the
// metadata server, and registers nothing: it prints the source it would use on stdout as
// "<name>: <detail>", then "helper: <socket>" whenever the helper answered and is not that source,
// and exits 0; with no source, stdout is empty, the chain's refusal is on stderr and it exits 1.
func cmdIdentity(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	flags := newFlagSet("identity", stderr)
	source := flags.Bool("source", false, "print the source of this process's broker identity a use command would take, asking the broker and the metadata server where it must")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		return refuseUnexpectedArgument(stderr, "identity")
	}
	if *source {
		return identitySource(stdout, stderr)
	}
	if launcherKeyDir(keyDir()) {
		return 0
	}
	answer := probeHelper()
	if !answer.there {
		if _, named := helperSocket(); named || helperInstalled() {
			fmt.Fprintf(stderr, "agent-secrets: helper unreachable at %s; not an agent session (the socket is absent)\n", answer.sock)
		}
		return 1
	}
	if answer.err != nil {
		fmt.Fprintf(stderr, "agent-secrets: helper unreachable at %s; not an agent session (%v)\n", answer.sock, answer.err)
		return 1
	}
	switch resp := answer.resp; {
	case resp.OK, resp.Code == helper.CodeNotEnrolled:
		return 0
	case resp.Code == helper.CodeNotASession:
		return 1
	case resp.Code == helper.CodeNoCredential:
		fmt.Fprintf(stderr, "agent-secrets: %s\n", noCredential(resp).sessionLine())
		return 1
	default:
		fmt.Fprintf(stderr, "agent-secrets: helper at %s answered %s: %s; not an agent session\n", answer.sock, resp.Code, resp.Error)
		return 1
	}
}

// identitySource is `identity --source`: the source the use command would take this process's
// identity from, waiting out a launcher's enrollment as the use command does.
func identitySource(stdout, stderr io.Writer) int {
	wait, err := enrollWait()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets identity: %v\n", err)
		return exitUsageError
	}
	dir := keyDir()
	awaitEnrollment(stderr, dir, wait)
	name, detail, err := describeSource(dir)
	if err != nil {
		reportError(stderr, "agent-secrets identity", err)
		return 1
	}
	if name == "helper" {
		fmt.Fprintf(stdout, "helper: %s%s\n", detail, helperVersionSuffix(detail))
	} else {
		fmt.Fprintf(stdout, "%s: %s\n", name, detail)
		if answer := probeHelper(); answer.answered() {
			fmt.Fprintf(stdout, "helper: %s%s\n", answer.sock, helperVersionSuffix(answer.sock))
		}
	}
	return 0
}
