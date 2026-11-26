// Command agent-secrets is the client for the secrets broker: an agent box or pod
// signs with the key.pem and enrollment file it keeps under AGENT_SECRETS_KEY_DIR (file mode); a
// machine session has no key of its own and asks agent-secrets-helper, over
// AGENT_SECRETS_HELPER_SOCK, to sign on its behalf (helper mode). It enrolls a runtime (box
// enrollment only, through the local helper — see cmdEnrollHelper below), requests and reads
// back secret grants, and — in its most common shape — requests one or more secrets and execs a
// command with them in its environment. The agent secrets themselves are managed with the secret
// forms (secret.go), which call the broker's own store (AWS Secrets Manager or GCP Secret Manager)
// under the cloud credential their environment gives them, the store's own IAM deciding what that
// credential may do, and then ask the broker to reread what they wrote.
//
// Every answer and every refusal about a secret names where it came from (source.go): the broker
// AGENT_SECRETS_URL names, and the kind of that broker's store, where its settings give one. This
// CLI reads no local secrets file, and another secrets CLI on the same machine can answer the
// same NAME from a store of its own, so the injection form writes one line to stderr before it
// execs — the names it injected and their source, never a value — and the status forms print
// that source beside what they report. None of those lines names the broker's host or the store's
// account, region or project, since they are quoted wherever an agent's output is; the secret
// forms, which call the store itself, name the account or project they read and write. No form
// prints a credential AGENT_SECRETS_URL carries (internal/redact).
//
//	agent-secrets keygen --out <dir>
//	agent-secrets enroll --helper --runtime-id <id> --thumbprint <tp> [--session-id <id>] [--projects <keys>]
//	agent-secrets enroll --pod --service <name> [--pod-token-file <file>] [--out <dir>] [--projects <keys>]
//	agent-secrets unenroll --helper --enrollment <id>
//	agent-secrets machine login|login-status|list [--json]|revoke <credential_id>
//	agent-secrets grant list [--json]|revoke <grant_id>
//	agent-secrets login
//	agent-secrets register [--wait SECONDS] [--session-id ID] [--projects KEYS] [--exec -- COMMAND [ARGS...]]
//	agent-secrets identity [--source]
//	agent-secrets renew [--pod-token-file FILE]
//	agent-secrets request NAME... [--reason TEXT] [--ttl DURATION] [--json]
//	agent-secrets status <request_id> [--json]
//	agent-secrets cancel <request_id>
//	agent-secrets revoke <grant_id>
//	agent-secrets self [--json]
//	agent-secrets whoami [--json]
//	agent-secrets sign --method M --url U [--enrollment E]
//	agent-secrets secret list|show|create|set|retag|delete|restore ...
//	agent-secrets NAME... [--reason TEXT] [--wait DURATION] [--ttl DURATION] [--target ACCOUNT[/REPOSITORY]]... -- <command> [args...]
//
// Environment: AGENT_SECRETS_URL (the broker's base URL). Every session-authenticated subcommand
// takes this process's broker identity from the first of four sources that has one (chain.go):
// AGENT_SECRETS_KEY_DIR (an agent box or pod's key.pem and enrollment, the two files keygen and
// enroll write), a person's `agent-secrets login` sign-in, AGENT_SECRETS_HELPER_SOCK (a machine
// session's agent-secrets-helper socket, joined by `agent-secrets register`) for a caller inside
// a session it registered, and the machine's own cloud identity, found with no setting from the
// exchange audiences the broker answers. When unset, the key dir and the socket fall back to
// their launcher's default path, $XDG_RUNTIME_DIR/agent-secrets and the helper socket inside it,
// and a default counts only when its file is there (identity.go). A process started with no
// environment of its own (a systemd unit, a cron job) reads the broker's address, the sign-in
// client and the machine identity settings from ~/.config/agent-secrets/config, where a variable
// that is set wins (config.go). AGENT_SECRETS_ENROLL_WAIT (a duration, default 20s) bounds how
// long a call waits while a box's launcher is still enrolling it. AGENT_SECRETS_APPROVE_URL
// (Dispatch's origin) makes `machine login` print the Dispatch page
// where a person types its code, and nothing else: a pending secret request (request, status, the
// exec form's wait) names the approver the broker names, and the record's page on the secrets
// broker that the broker's own answer carries as approve_url. The exec form's child keeps
// AGENT_SECRETS_URL, AGENT_SECRETS_KEY_DIR and AGENT_SECRETS_HELPER_SOCK, since it is the same
// session (buildChildEnv). For a name the broker holds back behind its airlock, that child's
// environment holds a placeholder in place of the value and the way to spend it: the broker's
// proxy in HTTPS_PROXY and https_proxy, a NO_PROXY that keeps the secret's own hosts on that
// proxy, NODE_USE_ENV_PROXY, and a trust bundle naming both the trust the command would have had
// and the proxy's certificate, in every bundle variable and in a WGETRC (airlock.go).
// A machine session enrolls automatically through the helper's own
// enroll loop, and a Legion pod's enrollment is its daemon's job — this CLI has no direct
// enrollment path for either; on a devbox only a session key a launcher holds (a box's) enrolls
// through it, and only via --helper (the shared broker contract has no launcher bearer token:
// nothing on a devbox can enroll except through a helper or the Legion daemon, the two processes
// that hold a launcher's proof-signing key). One runtime enrols itself: a Kubernetes pod an
// operator launches by hand (`enroll --pod`, pod.go). It holds no credential a person installed;
// the projected service-account token its own kubelet mounts is what the broker trusts instead,
// so it needs no helper, no launcher and no approval. The secret forms sign in to the store with
// the cloud credential the environment gives them, as the cloud's own tools read it - against an
// AWS broker the AWS SDK's own chain (access keys, a web identity token file, AWS_PROFILE,
// AWS_DEFAULT_PROFILE, the rest of its default chain), against a GCP one the first Google source
// present (newGCPBackend) - and need no session identity at all. The machine and grant forms act
// under this machine's login: the helper signs each call with its launcher credential
// (sign-launcher) for a process it can show is outside every registered session's process tree,
// the operator's own shell, and only for the broker's four operator routes.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/buildversion"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// Exit codes beyond 0 and 1 use BSD sysexits.h values where applicable. Every value returned by
// this command is listed here so broker-refgen can document it.
const (
	exitUsageError   = 2   // usage error: a flag, argument, token file, or required environment value is invalid
	exitPending      = 75  // request still waiting for a person or for the model judge, or its status could not be read when --wait ran out
	exitRenewRefused = 76  // renew reached a refusal in terminalRenewal; the worker shim must not restart it
	exitDenied       = 77  // request denied
	exitCannotRun    = 126 // register --exec command exists but could not run
	exitNotFound     = 127 // register --exec command was not found
	exitHangup       = 129 // secret prompt received SIGHUP
	exitInterrupted  = 130 // secret prompt received SIGINT
	exitQuit         = 131 // secret prompt received SIGQUIT
	exitTerminated   = 143 // secret prompt received SIGTERM
)

// command is one form of agent-secrets, as usage lists it and its own -h describes it.
type command struct {
	name     string // the words after "agent-secrets"; "" for the NAME... -- <command> form
	synopsis string
	summary  string // one or more lines, each under 90 characters
}

// commands is every form, in the order usage lists them.
var commands = []command{
	{"keygen", "agent-secrets keygen --out <dir>",
		"Write a new signing key to <dir>/key.pem and print its thumbprint. A box's launcher runs it\ninside the box before enrolling it."},
	{"enroll", "agent-secrets enroll --helper --runtime-id <id> --thumbprint <tp> [--session-id <id>] [--projects <keys>] | --pod --service <name> [--pod-token-file <file>] [--out <dir>] [--projects <keys>]",
		"Enroll a session key a launcher holds (a box's) through this machine's agent-secrets-helper,\nor, with --pod, enroll this Kubernetes pod itself, for the broker-side service --service\nnames: a pod proves who it is with the projected service-account token its own kubelet\nmounts (--pod-token-file, default " + defaultPodTokenFile + ").\nEach writes the enrollment id to AGENT_SECRETS_KEY_DIR/enrollment and prints it, alone, on\nstdout. --pod also writes key.pem there, reusing the one already in --out: a re-run keeps\nthat key's enrollment and renews its lease, and runs on one directory take turns. A pod keeps\nits own lease alive with the command --pod prints, which names the key directory because renew\nreads AGENT_SECRETS_KEY_DIR and never --out:\n`AGENT_SECRETS_KEY_DIR=<dir> agent-secrets renew --pod-token-file <file>`.\nWith --projects (default AGENT_SECRETS_PROJECTS), the box's session, or a person's pod, names\nthe Dispatch projects it works on, as register does; a service's pod names none."},
	{"unenroll", "agent-secrets unenroll --helper --enrollment <id>",
		"End an enrollment `enroll --helper` made, through this machine's agent-secrets-helper."},
	{"machine login", "agent-secrets machine login",
		"Log this machine in: print the confirmation code its operator types into Dispatch, then wait\nfor the decision (exit 0 once approved, 1 when denied or expired)."},
	{"machine login-status", "agent-secrets machine login-status",
		"Print \"issued\" and exit 0 while this machine's helper holds a launcher credential; otherwise\nprint the last login's state and exit 1."},
	{"machine list", "agent-secrets machine list [--json]",
		"List your own machines' logins, and the logins of your pods (pods whose Kubernetes account the\nbroker's BROKER_POD_OPERATORS maps to you), that can still reach a secret, under this machine's\nlogin: each one's id, host, who approved it, when it was issued and expires, and whether it\nhas expired with sessions still running. Dispatch's machine-login page lists the same, and\nevery service's login beside them."},
	{"machine revoke", "agent-secrets machine revoke <credential_id>",
		"End one of your own machines' logins, or a login of one of your pods, under this machine's\nlogin, as Dispatch's machine-login page does: it enrolls nothing more, and every session it\nenrolled ends. Revoking this machine's own login ends this machine's broker access too."},
	{"grant list", "agent-secrets grant list [--json]",
		"List the live grants of your sessions and those you approved, under this machine's login:\neach one's id, secrets, how it was granted, who approved it, who decided it (the model judge\nfor a grant it approved), its session and operator, and when it expires. Dispatch's Live\ngrants page lists the same."},
	{"grant revoke", "agent-secrets grant revoke <grant_id>",
		"End a grant. From your own shell, end it as its operator or approver, under this\nmachine's login, as Dispatch's Live grants page does; ending an automatic grant also\nmakes its session ask before it gets those secrets again. Inside a session, end a grant\ngiven to the broker identity this session signs as, as `revoke` does; any other grant is\nrefused NOT_YOURS, or answered as an unknown grant (NOT_FOUND) when it is a private\nproject's grant this session does not name."},
	{"login", "agent-secrets login",
		"Sign in on this machine as yourself: print the code to type in at Google, then record the\nsign-in at the secrets broker, which binds it to a box key this machine makes. Every session\nof yours on this machine then acts as you. A second person signs in beside you, and\nAGENT_SECRETS_PERSON then says which of you a session acts for."},
	{"register", "agent-secrets register [--wait SECONDS] [--session-id ID] [--projects KEYS] [--exec -- COMMAND [ARGS...]]",
		"Register a machine session with agent-secrets-helper. With --exec, register this process and\nrun COMMAND as this same process, so it and everything it starts are that session. Without\n--exec, name the session this process runs inside, or else the session launcher (omp) above\nit, never this process, which ends with the command; with neither, exit 1 and say to relaunch\nunder --exec. With --session-id, the session keeps one broker identity — and the requests and\ngrants bound to it — across a restart of the helper or of the machine, rather than enrolling\nafresh as a new pid; run it again with the id from inside a session that could not name one\nat launch. With --projects (default AGENT_SECRETS_PROJECTS), the session names the Dispatch\nprojects it works on, fixed at its first register."},
	{"identity", "agent-secrets identity [--source]",
		"Exit 0 when this process has a launcher's key dir or a helper session and 1 when it does not,\nasking the broker nothing. With --source, print the source of the broker identity a use\ncommand would take (key-dir, sign-in, helper or machine) and what it is, asking the broker\nand the metadata server where it must and registering nothing; exit 1 with every source's\nreason when none has one."},
	{"--version", "agent-secrets --version",
		"Print the release this binary was built as."},
	{"renew", "agent-secrets renew [--pod-token-file FILE]",
		"Keep an agent box's or pod's enrollment lease alive, renewing it until interrupted. A pod\nnames its projected service-account token's file, read again before each renewal, so each\nrenewal sends the token of that moment, which the broker checks names the pod the enrollment\nwas made for. POD_TOKEN_REQUIRED, ENROLLMENT_NOT_LIVE, or an older broker's PROOF_INVALID\nsaying \"not live\" exits 76, naming the enrollment, HTTP status and broker code; every other\nrefusal retries."},
	{"request", "agent-secrets request NAME... [--reason TEXT] [--ttl DURATION] [--json]",
		"Request secrets and print the request's id, state and source, the broker's own detail when\nit denied them, and who approves it and where while it waits; exit 75 while it waits for\napproval and 77 when it is denied. A name a person must approve needs\n--reason; with none the whole request is denied. The\ngrant lasts --ttl, or 15 minutes when any secret is human tier and 12 hours\notherwise (until this session ends when every secret is tagged\ndefault-grant=session)."},
	{"status", "agent-secrets status <request_id> [--json]",
		"Print a request's state, the broker and store it is at, who approves it and where while it\nwaits, its grant once granted, who decided it, and the broker's own detail when it was\ndenied."},
	{"cancel", "agent-secrets cancel <request_id>",
		"Cancel one of this session's pending requests."},
	{"revoke", "agent-secrets revoke <grant_id>",
		"End one of this session's grants."},
	{"self", "agent-secrets self [--json]",
		"Print this session's enrollment, operator, lease, live grants, and the broker and store\nevery secret of this session comes from."},
	{"whoami", "agent-secrets whoami [--json]",
		"Print this session's enrollment as the broker's JSON, as self --json does."},
	{"sign", "agent-secrets sign --method M --url U [--enrollment E]",
		"Print the proof this session would sign for one broker call, without making the call."},
	{"", "agent-secrets NAME... [--reason TEXT] [--wait DURATION] [--ttl DURATION] [--target ACCOUNT[/REPOSITORY]]... -- <command> [args...]",
		"Request secrets, wait up to --wait (default 30m) for a person to decide, and run <command>\nwith each granted secret in its environment under its NAME; nothing runs unless all are\ngranted. One line on stderr names those secrets and the broker and store they came from,\nnever a value. A name a person must approve needs --reason; with none the\nwhole request is denied. The grant lasts --ttl, or 15 minutes when any secret is human tier and 12 hours otherwise (until this session ends when every secret is tagged default-grant=session). The release\nnames <command>'s first 32 arguments, each cut to 256 bytes, and this call's --reason to the\nbroker, which logs them with the reason of the request the grant came of, each value of 8 bytes\nor more the release hands out and each credential of a known shape replaced by [redacted]. Any\nother secret on the command line, a shorter value among them, is logged as typed: pass secrets\nin the environment, never as arguments.\n--target narrows the one NAME's GitHub profile to ACCOUNT, or to ACCOUNT/REPOSITORY,\ngiven once for each repository of one account, at most 100 times. It never widens the\nprofile; a run of two NAMEs with a target, or targets of two accounts, exits 2 and sends\nnothing. A GitHub profile's NAME holds the JSON object of installations the release\nmakes, or one token where the profile says \"value\": \"token\". One line on stderr names\nfor each account of a GitHub profile the permissions its token leaves out, since the\ninstallation does not hold them."},
}

// usageEnvironment closes usage: the variables every form reads, and the exit codes.
const usageEnvironment = `
environment (a process given none of the AGENT_SECRETS_URL, AGENT_SECRETS_SIGNIN_* and
AGENT_SECRETS_MACHINE_* / AGENT_SECRETS_POOL_* settings reads them from
~/.config/agent-secrets/config, KEY=value lines; a variable that is set wins):
  AGENT_SECRETS_URL          the broker's base URL; every form that calls the broker needs it
  AGENT_SECRETS_KEY_DIR      an agent box's or pod's key.pem and enrollment, the first source of
                             a session form's broker identity; then a person's sign-in, then
                             the helper for a caller in its session, then this machine's own
                             identity (default $XDG_RUNTIME_DIR/agent-secrets, used when
                             key.pem is there)
  AGENT_SECRETS_HELPER_SOCK  a machine session's agent-secrets-helper socket
  AGENT_SECRETS_ENROLL_WAIT  how long a call waits while a box's launcher is still enrolling it
                             (default 20s)
  AGENT_SECRETS_APPROVE_URL  Dispatch's address; machine login names the page under it where the
                             operator types the code
  OMP_SESSION_ID             the agent session the broker names and notifies if a pending request
                             expires; falls back to ENVOY_SESSION_ID, then CLAUDE_CODE_SESSION_ID,
                             so a harness that sets one of those instead still names its session
  AGENT_SECRETS_PERSON       which person a session acts for, where more than one has signed in
                             on this machine with agent-secrets login; with one sign-in it is
                             that person, and with more and this unset every form that needs a
                             session identity refuses and names them
  AGENT_SECRETS_SIGNIN_DIR   where agent-secrets login keeps this machine's sign-ins
                             (default $XDG_STATE_HOME/agent-secrets/signins, which a reboot
                             keeps; the key dir above is runtime state and does not)
  AGENT_SECRETS_SIGNIN_CLIENT_ID
                             the device-flow client agent-secrets login signs a person in as
  AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE
                             a file holding that client's secret
  AGENT_SECRETS_SIGNIN_ISSUER
                             where a person signs in (default https://accounts.google.com); a dev
                             stack names its own issuer
  AGENT_SECRETS_MACHINE_IDENTITY
                             the one identity this machine proves at the broker's token
                             exchange, which a person's sign-in presents as the actor token:
                             metadata (AGENT_SECRETS_MACHINE_AUDIENCE) or pool (that, with
                             AGENT_SECRETS_MACHINE_TOKEN_FILE, AGENT_SECRETS_POOL_AUDIENCE and
                             AGENT_SECRETS_POOL_ACCOUNT); kubernetes and github-actions are
                             refused, since the broker mints no launcher credential for either;
                             unset, it is found from the Google exchange audience the broker
                             names: an external_account file GOOGLE_APPLICATION_CREDENTIALS
                             names, or the Google metadata server, and kept in
                             $XDG_STATE_HOME/agent-secrets/machine
  AGENT_SECRETS_MACHINE_TOKEN_FILE
                             the projected service-account token pool federates
  AGENT_SECRETS_MACHINE_AUDIENCE
                             the audience of this machine's Google identity token
  AGENT_SECRETS_POOL_AUDIENCE
                             the workload identity pool provider an external credential is
                             traded at, the full //iam.googleapis.com/projects/... resource name
  AGENT_SECRETS_POOL_ACCOUNT the managed identity generateIdToken is called on
  AGENT_SECRETS_SESSION      the session name a trade writes under, where the deployment's entry
                             fixes none
  AWS_PROFILE                the AWS profile the secret forms sign in with, against a broker on
                             AWS; access keys in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and
                             AWS_WEB_IDENTITY_TOKEN_FILE, outrank it, as in the AWS CLI, and
                             without it the AWS SDK's default credential chain signs in; every
                             secret form needs a sign-in in the broker's account, and the
                             store's IAM decides what it may do there
  AWS_DEFAULT_PROFILE        the AWS profile the secret forms sign in with where AWS_PROFILE is
                             unset; a profile either names that the AWS config does not hold
                             stops the command
  CLOUDSDK_AUTH_ACCESS_TOKEN a Google access token the secret forms sign in with against a broker
                             on Google Cloud; it outranks every other Google source
  GOOGLE_APPLICATION_CREDENTIALS
                             a Google credentials file the secret forms sign in with where no
                             token is lent; it outranks the gcloud variables below
  CLOUDSDK_ACTIVE_CONFIG_NAME
                             the gcloud configuration whose login the secret forms sign in with,
                             before the well-known credentials file, the active gcloud account
                             and the metadata server
  CLOUDSDK_CORE_ACCOUNT      the gcloud login the secret forms sign in with, at the same place in
                             that order; the store's IAM decides what each may do

exit codes: 0 done, 1 failed, 2 usage error, 75 still waiting for approval (or its status unreadable when --wait ran out), 76 permanent renewal refusal, 77 denied;
register --exec exits 127 when COMMAND is not found and 126 when it cannot run;
secret create and set end by the signal on hang-up (SIGHUP), interrupt (Ctrl-C), quit (Ctrl-\)
or termination (SIGTERM) at the value prompt: a shell reports 129, 130, 131 or 143
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage())
		return exitUsageError
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage())
		return 0
	case "--version":
		fmt.Fprintf(stdout, "agent-secrets %s\n", buildversion.String())
		return 0
	}
	if err := loadClientConfig(); err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return exitUsageError
	}
	switch args[0] {
	case "keygen":
		return cmdKeygen(args[1:], stdout, stderr)
	case "enroll":
		return cmdEnroll(args[1:], stdout, stderr)
	case "unenroll":
		return cmdUnenroll(args[1:], stdout, stderr)
	case "machine", "grant":
		return cmdGroup(args[0], args[1:], stdout, stderr)
	case "login":
		return cmdLogin(args[1:], stdout, stderr)
	case "register":
		return cmdRegister(args[1:], stdout, stderr)
	case "renew":
		return cmdRenew(args[1:], stdout, stderr)
	case "request":
		return cmdRequest(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "cancel":
		return cmdCancel(args[1:], stdout, stderr)
	case "revoke":
		return cmdRevoke(args[1:], stdout, stderr)
	case "self":
		return cmdSelf("self", args[1:], stdout, stderr)
	case "whoami":
		// whoami is GET /v1/enrollments/self, but must always print the broker's raw JSON body
		// (scripts/agentbox's box-doctor and host-doctor both pipe bare `agent-secrets whoami`
		// straight into jq): identical to self --json, so it aliases cmdSelf with --json forced
		// on rather than duplicating it.
		return cmdSelf("whoami", append([]string{"--json"}, args[1:]...), stdout, stderr)
	case "sign":
		return cmdSign(args[1:], stdout, stderr)
	case "identity":
		return cmdIdentity(args[1:], stdout, stderr)
	case "secret":
		return cmdSecret(args[1:], stdout, stderr)
	default:
		return cmdExec(args, stdout, stderr)
	}
}

func usage() string {
	var b strings.Builder
	b.WriteString("usage:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %s\n", c.synopsis)
		for line := range strings.SplitSeq(c.summary, "\n") {
			fmt.Fprintf(&b, "      %s\n", line)
		}
	}
	b.WriteString(usageEnvironment)
	return b.String()
}

func lookupCommand(name string) command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	panic(fmt.Sprintf("agent-secrets: commands has no entry %q", name))
}

// groupForms is the forms of a command group (machine, grant): the commands rows named
// "<group> <verb>", in usage's order.
func groupForms(group string) []command {
	var forms []command
	for _, c := range commands {
		if strings.HasPrefix(c.name, group+" ") {
			forms = append(forms, c)
		}
	}
	return forms
}

// cmdGroup dispatches the forms of a command group (machine, grant). The verbs it names and the
// help it prints come from groupForms, so usage, -h and the refusal of an unknown verb agree; a
// commands row with no case here is refused as unknown, which TestEveryFormAnswersHelp catches.
func cmdGroup(group string, args []string, stdout, stderr io.Writer) int {
	forms := groupForms(group)
	verbs := make([]string, len(forms))
	for i, c := range forms {
		verbs[i] = strings.TrimPrefix(c.name, group+" ")
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, "agent-secrets %s: a form is required: %s\n", group, strings.Join(verbs, ", "))
		return exitUsageError
	}
	switch form := group + " " + args[0]; form {
	case group + " -h", group + " -help", group + " --help":
		for i, c := range forms {
			if i > 0 {
				fmt.Fprintln(stderr)
			}
			writeCommandHelp(stderr, c)
		}
		return 0
	case "machine login":
		return cmdMachineLogin(args[1:], stdout, stderr)
	case "machine login-status":
		return cmdMachineLoginStatus(args[1:], stdout, stderr)
	case "machine list":
		return cmdMachineList(args[1:], stdout, stderr)
	case "machine revoke":
		return cmdMachineRevoke(args[1:], stdout, stderr)
	case "grant list":
		return cmdGrantList(args[1:], stdout, stderr)
	case "grant revoke":
		return cmdGrantRevoke(args[1:], stdout, stderr)
	}
	// The argument is not printed: a word that is no form is where a credential pasted in the
	// wrong place lands.
	fmt.Fprintf(stderr, "agent-secrets %s: unknown form (not printed); the forms are %s\n", group, strings.Join(verbs, ", "))
	return exitUsageError
}

// writeCommandHelp prints one form's synopsis and summary.
func writeCommandHelp(w io.Writer, c command) {
	fmt.Fprintf(w, "usage: %s\n\n%s\n", c.synopsis, c.summary)
}

// newFlagSet is the flag set of the form named name: its errors and its -h go to stderr, and -h
// prints the form's synopsis and summary, then its flags when it defines any. A form with no
// flags parses its arguments with it too, so its -h answers as every other form's does and any
// flag is a usage error. A failure names the flag and never the value (flagSet.Parse). No flag
// takes a default from AGENT_SECRETS_KEY_DIR, which PrintDefaults would print: keygen and enroll
// read it after Parse where --out is not given.
func newFlagSet(name string, stderr io.Writer) *flagSet {
	c := lookupCommand(name)
	flags := &flagSet{flag.NewFlagSet(strings.TrimSpace("agent-secrets "+name), flag.ContinueOnError)}
	flags.SetOutput(stderr)
	flags.Usage = func() {
		writeCommandHelp(flags.Output(), c)
		hasFlags := false
		flags.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(flags.Output(), "\nflags:")
			flags.PrintDefaults()
		}
	}
	return flags
}

// flagSet is a form's flag set (newFlagSet): a flag.FlagSet whose Parse says a failure in words
// of its own. Go's flag package quotes the value that did not parse ("invalid value %q for flag
// -%s"), the argument whose syntax did not ("bad flag syntax: %s") and the argument it took for a
// flag it does not define ("flag provided but not defined: -%s"), in the line it prints and the
// error it answers: a URL flag's value can carry a credential, and an argument that starts with
// '-' can be a token pasted in the wrong place.
type flagSet struct{ *flag.FlagSet }

// Parse parses arguments as flag.FlagSet.Parse does, except that a failure prints and answers
// fixed text: "invalid value for flag -<name>" (or "invalid boolean value for flag -<name>") for
// a value the flag refused, "bad flag syntax: a flag that does not parse" for an argument whose
// syntax does not, "an argument that is not a flag of this form (not printed)" for one that names
// no flag the form defines, and Go's own words only for a flag given no value, which names a flag
// the form defines. The form's usage, which lists the flags it takes, follows each on the flag
// set's output, and -h prints the usage and answers flag.ErrHelp, as before. While Go's parse runs
// its output is discarded, and each flag's value records whether it refused its text
// (parsingValue).
func (f *flagSet) Parse(arguments []string) error {
	var refused *flag.Flag
	f.VisitAll(func(named *flag.Flag) { named.Value = parsingValue{named.Value, named, &refused} })
	out := f.Output()
	f.SetOutput(io.Discard)
	err := f.FlagSet.Parse(arguments)
	f.SetOutput(out)
	f.VisitAll(func(named *flag.Flag) { named.Value = named.Value.(parsingValue).Value })
	switch text := fmt.Sprint(err); {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		f.Usage()
		return err
	case refused != nil && isBoolFlag(refused.Value):
		err = fmt.Errorf("invalid boolean value for flag -%s", refused.Name)
	case refused != nil:
		err = fmt.Errorf("invalid value for flag -%s", refused.Name)
	case strings.HasPrefix(text, "flag provided but not defined: -"):
		err = errors.New("an argument that is not a flag of this form (not printed)")
	case !strings.HasPrefix(text, "flag needs an argument: -"):
		err = errors.New("bad flag syntax: a flag that does not parse")
	}
	fmt.Fprintln(out, err)
	f.Usage()
	return err
}

// parsingValue is a flag's Value while flagSet.Parse runs: it sets as the value it wraps does, and
// records named in refused where that value refuses its text, so Parse names the flag without it.
type parsingValue struct {
	flag.Value
	named   *flag.Flag
	refused **flag.Flag
}

func (v parsingValue) Set(text string) error {
	err := v.Value.Set(text)
	if err != nil {
		*v.refused = v.named
	}
	return err
}

// IsBoolFlag is the wrapped value's, so a boolean flag still takes no argument.
func (v parsingValue) IsBoolFlag() bool { return isBoolFlag(v.Value) }

// isBoolFlag reports whether value is a boolean flag's, which Go's flag package sets with no
// argument.
func isBoolFlag(value flag.Value) bool {
	b, ok := value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func exitUsage(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return exitUsageError
}

// flagGiven reports whether flags' command line gave the flag name, so a flag given an empty or
// zero value is told apart from one not given.
func flagGiven(flags *flagSet, name string) bool {
	given := false
	flags.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// reportError prints a command's failure as "<prefix>: <err>", except that a helper holding no
// launcher credential is reported with the one line identity prints for it, whatever the command:
// the helper's own answer, which says what cures it in the helper's mode.
func reportError(stderr io.Writer, prefix string, err error) {
	var none *noCredentialError
	if errors.As(err, &none) {
		fmt.Fprintf(stderr, "agent-secrets: %s\n", none.sessionLine())
		return
	}
	fmt.Fprintf(stderr, "%s: %v\n", prefix, err)
}

// unexpectedArgument is how a form refuses an argument it does not take (refuseUnexpectedArgument):
// without the argument, which is where a credential pasted in the wrong place lands.
const unexpectedArgument = "unexpected argument (not printed: an argument this form does not take may be a credential pasted in the wrong place)"

// refuse prints a command's refusal together with where this CLI looked for the secret
// (source.go): reportError's line with the source named, so a refusal of this CLI's can never be
// read as another secrets CLI's answer about the same name. The one exception is the helper's
// no-credential answer, which reportError prints as identity's own line: it already says this
// machine has no login to the secrets broker, and no broker was asked anything.
func refuse(stderr io.Writer, prefix string, src source, err error) {
	if errors.Is(err, errNoCredential) {
		reportError(stderr, prefix, err)
		return
	}
	fmt.Fprintf(stderr, "%s: %s\n", prefix, refusalText(src, err))
}

// refusalText is what a refusal says after its prefix: the failure, and where this CLI looked.
// A call that never reached the broker says so itself - which address it tried, that
// AGENT_SECRETS_URL named it, and whether the host does not resolve or nothing answered - and
// takes no source trailer: the broker refused nothing, so a trailer saying it was asked would
// read as an answer it gave, and the store behind it could not be read either. Likewise, a local
// helper failure names its socket: no broker request could begin without its signature. A
// refusal made before any call (refusedBeforeCall) takes no trailer either.
func refusalText(src source, err error) string {
	var unreached *unreachableError
	var helperFailure *helperError
	var notSent *refusedBeforeCall
	switch {
	case errors.As(err, &unreached):
		return unreached.Error()
	case errors.As(err, &helperFailure):
		return helperFailure.Error()
	case errors.As(err, &notSent):
		return notSent.Error()
	default:
		return err.Error() + src.asked()
	}
}

// splitArgs partitions args into recognized "--flag"/"--flag value" tokens (as declared by
// takesValue, keyed by flag name without its leading dashes) and every other bare token, so a
// subcommand can accept secret names or ids before, after, or between its optional flags — the
// stdlib flag package alone stops parsing at the first non-flag token, which would misparse
// "agent-secrets request NAME --reason TEXT". A token absent from takesValue is treated as a
// boolean flag consuming no value (e.g. --json).
func splitArgs(args []string, takesValue map[string]bool) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := strings.TrimLeft(a, "-")
		if !strings.HasPrefix(a, "-") || name == "" || name == a {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		if takesValue[name] && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positional
}

// encodeKeyPEM is a session key as its key.pem bytes: the PKCS8 PEM keygen writes and loadKey
// reads back, in the one place that marshals it, so every key this binary writes is
// byte-for-byte a key keygen wrote.
func encodeKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// loadKey is the session key the key.pem at path holds, which every refusal names as file
// (outKeyFile, keyDirFile(keyFile), machineCacheKeyFile) and never by its path: a file it cannot
// read prints the system's reason alone (redact.File), and content that is no key prints
// parseKeyPEM's fixed text.
func loadKey(path, file string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, redact.File(err))
	}
	return parseKeyPEM(data, file)
}

// parseKeyPEM is a session key as its PKCS#8 PEM bytes hold it, named for the refusal as name
// names it. Each refusal is fixed text: the PKCS#8 parser's words describe the key's bytes.
func parseKeyPEM(data []byte, name string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: not a PEM file", name)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: not a PKCS#8 private key", name)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA private key", name)
	}
	return key, nil
}

// buildSigner selects and builds this process's Signer: the first source of the chain that has
// one (chain.go) — a launcher's key dir, a person's sign-in, the helper for a caller inside a
// session it registered, the machine's own identity. The key dir and socket are
// AGENT_SECRETS_KEY_DIR and AGENT_SECRETS_HELPER_SOCK, or their defaults where those are unset
// (identity.go). While a box's launcher is still enrolling it (a fresh enrollment.pending in the
// key dir), this first waits for key.pem and enrollment, for AGENT_SECRETS_ENROLL_WAIT at most,
// saying so on stderr (awaitEnrollment). A refusal after a wait that ran out says how long the
// call waited and its bound, "(waited 20.004s, its bound 20s, while ...)": the time to the
// millisecond, or as measured where that rounding would put it under a bound finer than one.
// It is the refusal's own text, not a line before it, so a caller that takes the first line that
// is not a wait notice (the box launcher's token helper) still takes the refusal.
// The enrollment id it returns is the key dir's, the sign-in's or the machine's own; helper mode
// returns "" because nothing needs it as a literal except RenewEnrollment's URL path, and a
// machine session's lease is renewed by the helper itself — cmdRenew refuses outright once it
// sees an empty id from a successful build.
func buildSigner(stderr io.Writer) (enrollmentID string, signer Signer, err error) {
	return buildChainSigner(stderr, true)
}

// buildChainSigner is buildSigner with the chain's passOver (signerChain): false for renew alone.
func buildChainSigner(stderr io.Writer, passOver bool) (enrollmentID string, signer Signer, err error) {
	wait, err := enrollWait()
	if err != nil {
		return "", nil, &settingError{err}
	}
	dir := keyDir()
	waited, gaveUp := awaitEnrollment(stderr, dir, wait)
	enrollmentID, signer, err = selectSigner(dir, stderr, passOver)
	if err != nil && gaveUp {
		if shown := waited.Round(time.Millisecond); shown >= wait {
			waited = shown
		}
		err = fmt.Errorf("%w (waited %s, its bound %s, while %s held %s)", err, waited, wait, keyDirName(), pendingMarker)
	}
	return enrollmentID, signer, err
}

// settingError is a session form's failure for a setting it cannot run without or cannot read,
// which is a usage error (exit 2), where every other failure to find this process's identity is a
// plain failure (exit 1, sessionFailed).
type settingError struct{ error }

func (e *settingError) Unwrap() error { return e.error }

// sessionFailed reports why a session form has no signer, with reportError's line, and answers
// its exit code: exitUsageError for a settingError, 1 for the chain's refusal and for a source
// that was present but broken.
func sessionFailed(stderr io.Writer, prefix string, err error) int {
	reportError(stderr, prefix, err)
	var setting *settingError
	if errors.As(err, &setting) {
		return exitUsageError
	}
	return 1
}

// signInSigner is the signer of record, the sign-in of the person this session acts for, whose
// box key is key (signInSource chose both). A sign-in whose enrollment has not been made yet is
// registered now, under a fresh trade of that person's identity, and the record is rewritten
// with it, so a record that outlived its enrollment heals itself.
//
// So does one whose enrollment the broker no longer holds: nothing renews a sign-in's lease, so
// every enrollment it makes ends after BROKER_LEASE_SECONDS, and without this the box would
// answer nothing but a dead id until somebody ran `agent-secrets login` again — which would
// make a second box key and a second sign-in on that person's list (client.reenroller).
func signInSigner(record cipherclient.SignInRecord, key *ecdsa.PrivateKey) (*fileSigner, error) {
	// The box key is the sign-in's own, recorded at the broker with it, so it is the key a
	// registration made again signs with too.
	register := func(ctx context.Context, _ string) (*ecdsa.PrivateKey, string, error) {
		record.EnrollmentID = ""
		fresh, err := registerSignInSession(ctx, record, key)
		if err != nil {
			return nil, "", err
		}
		record = fresh
		return key, fresh.EnrollmentID, nil
	}
	if record.EnrollmentID == "" {
		if _, _, err := register(context.Background(), ""); err != nil {
			return nil, err
		}
	}
	return &fileSigner{key: key, enrollmentID: record.EnrollmentID, register: register}, nil
}

// sessionContext resolves the broker URL and this process's Signer every session-authenticated
// subcommand (renew, request, status, cancel, revoke, self, and the exec form) needs, plus the
// enrollment id buildSigner read (empty in helper mode). A missing broker URL is a settingError.
// stderr is the command's, which buildSigner's wait for a launcher still enrolling speaks on.
func sessionContext(stderr io.Writer) (base cipheraddr.Address, enrollmentID string, signer Signer, err error) {
	return chainSessionContext(stderr, true)
}

// renewContext is sessionContext for renew, whose chain never passes over a person's sign-in
// (signerChain): renew reports an enrollment the broker no longer holds rather than renew another.
func renewContext(stderr io.Writer) (base cipheraddr.Address, enrollmentID string, signer Signer, err error) {
	return chainSessionContext(stderr, false)
}

func chainSessionContext(stderr io.Writer, passOver bool) (base cipheraddr.Address, enrollmentID string, signer Signer, err error) {
	base = brokerBaseURL()
	if base.Empty() {
		return cipheraddr.Address{}, "", nil, &settingError{fmt.Errorf("%s is required: it names the secrets broker this command reads every secret from, and this command reads no local secrets file", brokerURLVar)}
	}
	enrollmentID, signer, err = buildChainSigner(stderr, passOver)
	if err != nil {
		return cipheraddr.Address{}, "", nil, err
	}
	return base, enrollmentID, signer, nil
}

// writeVerbatim writes exactly raw, nothing more and nothing less: --json's contract is "the
// response body verbatim ... one JSON object on stdout and nothing else", so this must never
// append a newline the broker's own response didn't already end with.
func writeVerbatim(w io.Writer, raw []byte) {
	w.Write(raw)
}

// jsonFlag is --json's usage line wherever a form prints the broker's answer: the body as the
// broker wrote it, every field, where the form's own lines print each value as the broker's
// address lets its answer print (cipheraddr.Address.Words). The flag asks for the body by name.
const jsonFlag = "print the broker's response body verbatim: every field as the broker wrote it, none withheld, even beside an AGENT_SECRETS_URL that is not a plain http(s) URL"

// nextBackoff advances the backoff the poll loops of this package keep: two seconds, doubling
// to a ten-second cap. `machine login` sleeps it between reads of its helper; the exec form's
// wait sleeps it only after a read the broker did not hold (a broker from before ?wait=, or a
// failed read). The renewer's own step is leasetime.NextBackoff, which the broker's lease bound
// walks it by; these polls are nobody's lease, and a change made to move that floor is not a
// change to how often either of them asks.
func nextBackoff(cur time.Duration) time.Duration {
	return min(2*cur, 10*time.Second)
}

// pollBackoff is the exec form's sleep before its next status read, after one that came back
// undecided before the wait it asked had run out (its backoff). Nothing in this program replaces
// it: exec_wait_test.go builds agent-secrets with one more file that does, recording each sleep
// the loop asks for, so a row can tell a loop that sleeps after a held answer by that sleep
// itself, whatever the machine's load.
var pollBackoff = time.Sleep

// ---------------------------------------------------------------------------
// keygen
// ---------------------------------------------------------------------------

func cmdKeygen(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"out": true})
	if len(positional) > 0 {
		return refuseUnexpectedArgument(stderr, "keygen")
	}
	flags := newFlagSet("keygen", stderr)
	// --out's default, AGENT_SECRETS_KEY_DIR, is read after Parse rather than given to the flag,
	// as enroll's is: PrintDefaults, which -h and every usage error print, would print it, and it
	// is the directory of key.pem, a file that holds a credential.
	out := flags.String("out", "", "directory to write key.pem into; AGENT_SECRETS_KEY_DIR when not given")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if !flagGiven(flags, "out") {
		*out = os.Getenv("AGENT_SECRETS_KEY_DIR")
	}
	if *out == "" {
		fmt.Fprintln(stderr, "agent-secrets keygen: --out (or AGENT_SECRETS_KEY_DIR) is required")
		return exitUsageError
	}
	key, err := proof.NewKey()
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: generate key: %v\n", err)
		return 1
	}
	pemBytes, err := encodeKeyPEM(key)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: %v\n", err)
		return 1
	}
	// The directory and the key are named by the flag and the variable, never by their path
	// (outDir, outKeyFile): key.pem holds the session key.
	if err := os.MkdirAll(*out, 0o700); err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: make %s: %v\n", outDir, redact.File(err))
		return 1
	}
	if err := writeFileAtomic(filepath.Join(*out, keyFile), pemBytes, 0o600); err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: write %s: %v\n", outKeyFile, redact.File(err))
		return 1
	}
	thumb, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets keygen: thumbprint: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, thumb)
	return 0
}

// ---------------------------------------------------------------------------
// renew
// ---------------------------------------------------------------------------

// cmdRenew renews this session's enrollment until interrupted. With --pod-token-file (a pod; the
// Legion worker shim passes the projected token file the runtime mounts) every renewal reads the
// file again and sends its contents as pod_token, since the kubelet rotates the token in place;
// the broker verifies it against the enrollment's pod UID and service account. Without the flag
// a renewal has no body, a box's renewal. A broker that predates the token reads no renew body,
// so a renewal carrying one still renews there. A refusal in terminalRenewal
// (POD_TOKEN_REQUIRED, ENROLLMENT_NOT_LIVE, or an older broker's PROOF_INVALID saying "not
// live") exits exitRenewRefused (76), so the worker shim stops rather than restarting a renewer
// that cannot succeed; every other refusal, POD_IDENTITY_MISMATCH and POD_TOKEN_EXPIRED among
// them, retries.
func cmdRenew(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, map[string]bool{"pod-token-file": true})
	flags := newFlagSet("renew", stderr)
	podTokenFile := flags.String("pod-token-file", "", "a pod's projected service-account token file, read again before every renewal")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		fmt.Fprintln(stderr, "agent-secrets renew: no arguments are accepted")
		return exitUsageError
	}
	base, enrollmentID, signer, err := renewContext(stderr)
	if err != nil {
		return sessionFailed(stderr, "agent-secrets renew", err)
	}
	if enrollmentID == "" {
		fmt.Fprintln(stderr, "agent-secrets renew: a machine session's lease is renewed by agent-secrets-helper; agent-secrets renew is for agent boxes and pods")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *podTokenFile != "" {
		// A pod given a token file that cannot be read now never starts: a bodyless renewal of
		// such an enrollment is refused anyway, and would hide the fault.
		if _, err := readPodToken(*podTokenFile); err != nil {
			fmt.Fprintf(stderr, "agent-secrets renew: %s\n", tokenUnreadable(enrollmentID, err))
			return exitUsageError
		}
	}
	return renewLoop(ctx, newClient(base), signer, enrollmentID, *podTokenFile, stderr, time.After)
}

// tokenUnreadable is the ERROR a pod's renewer prints when it cannot read its token: it sends
// nothing (a bodyless renewal of a pod enrollment is refused, and would hide the fault), so the
// broker never hears of it, and this line, which the worker shim relays into its log, is the
// only record naming the enrollment whose lease is lapsing.
func tokenUnreadable(enrollmentID string, err error) string {
	return fmt.Sprintf("ERROR: enrollment %s cannot read its pod token (%v); nothing was sent, so its lease lapses unless the file becomes readable", enrollmentID, err)
}

// renewLoop renews enrollmentID until ctx ends or the broker refuses it for good. podTokenFile
// is a pod's projected token file, read again before every renewal because the kubelet rotates
// it in place, and "" is every other session's renewal, which carries no body.
func renewLoop(ctx context.Context, c *client, signer Signer, enrollmentID, podTokenFile string, stderr io.Writer, after func(time.Duration) <-chan time.Time) int {
	backoff := leasetime.RenewBackoffInitial
	for {
		var token string
		var tokenErr error
		if podTokenFile != "" {
			token, tokenErr = readPodToken(podTokenFile)
		}
		var expires time.Time
		err := tokenErr
		if err == nil {
			expires, err = c.RenewEnrollment(ctx, signer, enrollmentID, token)
		}
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			var refused *apiError
			if errors.As(err, &refused) && refused.Permanent() {
				code := refused.Code
				if code == "" {
					code = "UNKNOWN"
				}
				fmt.Fprintf(stderr, "agent-secrets renew: enrollment %s permanently refused by %s: HTTP %d %s: %s\n", enrollmentID, brokerSource(c.baseURL), refused.Status, code, refused.Message)
				return exitRenewRefused
			}
			if tokenErr != nil {
				fmt.Fprintf(stderr, "agent-secrets renew: %s; retrying in %s\n", tokenUnreadable(enrollmentID, tokenErr), backoff)
			} else {
				fmt.Fprintf(stderr, "agent-secrets renew: %v; retrying in %s\n", err, backoff)
			}
			select {
			case <-ctx.Done():
				return 0
			case <-after(backoff):
			}
			backoff = leasetime.NextBackoff(backoff)
			continue
		}
		backoff = leasetime.RenewBackoffInitial
		lease := time.Until(expires)
		if lease <= 0 {
			lease = time.Second
		}
		select {
		case <-ctx.Done():
			return 0
		case <-after(leasetime.RenewAfter(lease)):
		}
	}
}

// ---------------------------------------------------------------------------
// request
// ---------------------------------------------------------------------------

func cmdRequest(args []string, stdout, stderr io.Writer) int {
	flagArgs, names := splitArgs(args, map[string]bool{"reason": true, "ttl": true})
	flags := newFlagSet("request", stderr)
	reason := flags.String("reason", "", "why these secrets are needed; necessary where a person approves")
	ttlFlagValue := flags.Duration("ttl", 0, ttlFlag)
	asJSON := flags.Bool("json", false, jsonFlag)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	ttl, err := ttlSeconds(flags, *ttlFlagValue)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets request: %v\n", err)
		return exitUsageError
	}
	if len(names) == 0 {
		fmt.Fprintln(stderr, "agent-secrets request: at least one secret NAME is required")
		return exitUsageError
	}
	base, _, signer, err := sessionContext(stderr)
	if err != nil {
		return sessionFailed(stderr, "agent-secrets request", err)
	}
	ctx := context.Background()
	c := newClient(base)
	// `request` takes no --target: it asks for each name as its profile stands.
	result, raw, err := c.CreateRequest(ctx, signer, names, *reason, ttl, requestSessionID(), nil)
	if err != nil {
		refuse(stderr, "agent-secrets request", c.source(ctx), err)
		return 1
	}
	if err := grantedLifetime(ctx, c, signer, result, ttl); err != nil {
		fmt.Fprintf(stderr, "agent-secrets request: %s\n", refusalText(c.source(ctx), err))
		return 1
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
	} else {
		// Every value on these lines is the broker's, so each prints as the broker's address
		// lets its answer print (cipheraddr.Address.Words): on one line beside a clean address,
		// and not at all beside one that carries a credential, which whoever answers there can
		// repeat in any value. It was redact.OneLine alone until the values beside a
		// credentialed address were found to print whole; --json still prints the body as it
		// came, since it is asked for by name.
		fmt.Fprintf(stdout, "request_id: %s\nstate: %s\nsource: %s\n", idWords(c.baseURL, result.RequestID), c.baseURL.Words(result.State), c.source(ctx))
		printDetail(stdout, c.baseURL, result.Detail)
		if result.State == "pending" {
			fmt.Fprintln(stdout, waitingOn(c.baseURL, result.Approver, result.ApproveURL, result.JudgeDeciding))
		}
	}
	switch result.State {
	case "pending":
		return exitPending
	case "denied":
		projectHint(stderr, result.Detail)
		return exitDenied
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	flags := newFlagSet("status", stderr)
	asJSON := flags.Bool("json", false, jsonFlag)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets status: exactly one request_id is required")
		return exitUsageError
	}
	base, _, signer, err := sessionContext(stderr)
	if err != nil {
		return sessionFailed(stderr, "agent-secrets status", err)
	}
	ctx := context.Background()
	c := newClient(base)
	result, raw, err := c.GetRequest(ctx, signer, positional[0], 0)
	if err != nil {
		refuse(stderr, "agent-secrets status", c.source(ctx), err)
		return 1
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
		return 0
	}
	fmt.Fprintf(stdout, "state: %s\nsource: %s\n", c.baseURL.Words(result.State), c.source(ctx))
	if result.State == "pending" {
		fmt.Fprintln(stdout, waitingOn(c.baseURL, result.Approver, result.ApproveURL, result.JudgeDeciding))
	}
	if result.GrantID != nil {
		fmt.Fprintf(stdout, "grant_id: %s\n", idWords(c.baseURL, *result.GrantID))
	}
	if result.Decision != nil {
		fmt.Fprintf(stdout, "decided_by: %s\n", c.baseURL.Words(result.Decision.By))
	}
	printDetail(stdout, c.baseURL, result.Detail)
	return 0
}

// ---------------------------------------------------------------------------
// cancel
// ---------------------------------------------------------------------------

func cmdCancel(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("cancel", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets cancel: exactly one request_id is required")
		return exitUsageError
	}
	base, _, signer, err := sessionContext(stderr)
	if err != nil {
		return sessionFailed(stderr, "agent-secrets cancel", err)
	}
	ctx := context.Background()
	c := newClient(base)
	if err := c.CancelRequest(ctx, signer, positional[0]); err != nil {
		refuse(stderr, "agent-secrets cancel", c.source(ctx), err)
		return 1
	}
	fmt.Fprintf(stdout, "cancelled at %s\n", brokerSource(base))
	return 0
}

// ---------------------------------------------------------------------------
// revoke
// ---------------------------------------------------------------------------

func cmdRevoke(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("revoke", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "agent-secrets revoke: exactly one grant_id is required")
		return exitUsageError
	}
	c, failed, err := revokeAsSession(positional[0], stderr)
	switch {
	case failed != nil:
		return sessionFailed(stderr, "agent-secrets revoke", failed)
	case err != nil:
		refuse(stderr, "agent-secrets revoke", c.source(context.Background()), err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked at %s\n", brokerSource(c.baseURL))
	return 0
}

// revokeAsSession is the session's own revoke of grantID (POST /v1/grants/{id}/revoke), which
// `revoke` and `grant revoke` inside a session both make, under the identity, the enrollment wait
// and the exit codes of every session command (sessionContext, sessionFailed). failed is why this
// process has no such identity, for the form to report through sessionFailed, so the exit code
// comes from a function broker-refgen follows; otherwise err is the broker's answer to c's call,
// unreported, for the form to word. stderr is the form's, for the enrollment wait.
func revokeAsSession(grantID string, stderr io.Writer) (c *client, failed, err error) {
	base, _, signer, failed := sessionContext(stderr)
	if failed != nil {
		return nil, failed, nil
	}
	c = newClient(base)
	return c, nil, c.RevokeGrant(context.Background(), signer, grantID)
}

// ---------------------------------------------------------------------------
// self
// ---------------------------------------------------------------------------

// cmdSelf is self, and whoami as self --json; name is which, for its help and its messages.
func cmdSelf(name string, args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if len(positional) > 0 {
		return refuseUnexpectedArgument(stderr, name)
	}
	flags := newFlagSet(name, stderr)
	asJSON := flags.Bool("json", false, jsonFlag)
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	base, _, signer, err := sessionContext(stderr)
	if err != nil {
		return sessionFailed(stderr, "agent-secrets "+name, err)
	}
	ctx := context.Background()
	c := newClient(base)
	result, raw, err := c.Self(ctx, signer)
	if err != nil {
		refuse(stderr, "agent-secrets "+name, c.source(ctx), err)
		return 1
	}
	if *asJSON {
		writeVerbatim(stdout, raw)
		return 0
	}
	operator := ""
	if result.Operator != nil {
		operator = *result.Operator
	}
	words := c.baseURL.Words
	fmt.Fprintf(stdout, "enrollment_id: %s\nkind: %s\noperator: %s\nprojects: %s\nlease_expires_at: %s\nsource: %s\n",
		idWords(c.baseURL, result.EnrollmentID), words(result.Kind), words(operator), words(orDash(oauthwire.JoinProjects(result.Projects))), result.LeaseExpiresAt.Format(time.RFC3339), c.source(ctx))
	for _, g := range result.Grants {
		if g.ExpiresAt == nil {
			fmt.Fprintf(stdout, "grant: %s (request %s, until this session ends)\n", idWords(c.baseURL, g.GrantID), idWords(c.baseURL, g.RequestID))
			continue
		}
		fmt.Fprintf(stdout, "grant: %s (request %s, expires %s)\n", idWords(c.baseURL, g.GrantID), idWords(c.baseURL, g.RequestID), g.ExpiresAt.Format(time.RFC3339))
	}
	return 0
}

// ---------------------------------------------------------------------------
// NAME... -- <command> [args...]
// ---------------------------------------------------------------------------

// cmdExec implements the default "agent-secrets NAME... [--reason TEXT]
// [--wait DURATION] -- <command> [args...]" form: it requests the named secrets, waits out a
// pending decision, and syscall.Execs the command with the granted values in its environment.
// Nothing runs when no NAME is given, and nothing runs while any requested secret is still
// pending or was denied — a denial of any single name denies the whole request (requests.Machine
// aggregates the decision), so this never partially execs with some secrets missing.
func cmdExec(args []string, stdout, stderr io.Writer) int {
	if message, removed := removedGitHubFlag(args); removed {
		fmt.Fprintf(stderr, "agent-secrets: %s\n", message)
		return exitUsageError
	}
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		fmt.Fprint(stderr, usage())
		return exitUsageError
	}
	before, command := args[:sep], args[sep+1:]
	if len(command) == 0 {
		fmt.Fprintln(stderr, "agent-secrets: a command is required after --")
		return exitUsageError
	}

	flagArgs, names := splitArgs(before, map[string]bool{"reason": true, "wait": true, "ttl": true, "target": true})
	flags := newFlagSet("", stderr)
	reason := flags.String("reason", "", "why these secrets are needed; necessary where a person approves")
	wait := flags.Duration("wait", 30*time.Minute, "how long to wait for a pending request")
	ttlFlagValue := flags.Duration("ttl", 0, ttlFlag)
	var targets []string
	flags.Func("target", "narrow the one NAME's GitHub profile to ACCOUNT, or to ACCOUNT/REPOSITORY (repeatable, all of one account)", func(target string) error {
		targets = append(targets, target)
		return nil
	})
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	ttl, err := ttlSeconds(flags, *ttlFlagValue)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return exitUsageError
	}
	if len(names) == 0 {
		fmt.Fprintln(stderr, "agent-secrets: at least one secret NAME is required")
		return exitUsageError
	}
	locations, err := record.TargetLocations(names, targets)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return exitUsageError
	}
	// The command is looked up before the broker is asked anything: a command that is not on
	// PATH could never run, and a request for it would wait on an approver all the same.
	path, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v\n", err)
		return 1
	}

	base, _, signer, err := sessionContext(stderr)
	if err != nil {
		return sessionFailed(stderr, "agent-secrets", err)
	}
	c := newClient(base)
	ctx := context.Background()
	// Every line below names where the values come from, and the broker's store is read once for
	// all of them: the injection line, and each refusal, whether the request is refused, waits
	// out its decision, or releases no value.
	src := sync.OnceValue(func() source { return c.source(ctx) })
	result, _, err := c.CreateRequest(ctx, signer, names, *reason, ttl, requestSessionID(), locations)
	if err != nil {
		refuse(stderr, "agent-secrets", src(), err)
		return 1
	}
	if err := grantedLifetime(ctx, c, signer, result, ttl); err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %s; nothing was run\n", refusalText(src(), err))
		return 1
	}

	state, grantID, requestID, detail := result.State, result.GrantID, result.RequestID, result.Detail
	// shownID is the request's id as the lines below print it: the broker's, so as its address
	// lets its answer print, and beside an address that does not, whole where it has the broker's
	// id shape (idWords). requestID itself is what the polls ask after.
	shownID := idWords(c.baseURL, requestID)
	// pollErr is the last status poll's failure while the polls fail in a way a retry can cure
	// (retryable), and nil once one reads the request again. Such a poll does not end the wait:
	// the approver may still decide before --wait runs out, and a grant this command has stopped
	// waiting for is one nothing collects.
	var pollErr error
	if state == "pending" {
		reportPending(stderr, c.baseURL, shownID, result.Approver, result.ApproveURL, result.JudgeDeciding, *wait, src())
		judging := result.JudgeDeciding
		deadline := time.Now().Add(*wait)
		// backoff is the sleep before the next read after one that came back undecided before
		// the wait it asked had run out. The broker holds a read until the request is decided or
		// that wait passes, so a pending answer that comes sooner is from a broker before ?wait=,
		// which answers every read at once; a failed read that comes back sooner gets the same
		// sleep. Without it the loop would ask such a broker as fast as it answers.
		backoff := 2 * time.Second
		for state == "pending" {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			// Each read asks the broker to hold it for what is left of --wait, in whole seconds
			// rounded up so the last read covers the deadline, at most
			// record.MaxRequestWaitSeconds: the broker answers within a step of a decision, so
			// the command learns of it at once rather than at its next poll.
			asked := min(record.MaxRequestWaitSeconds, int((remaining+time.Second-1)/time.Second))
			sent := time.Now()
			status, _, err := c.GetRequest(ctx, signer, requestID, asked)
			switch {
			case err == nil:
				pollErr = nil
				state, grantID, detail = status.State, status.GrantID, status.Detail
				// The judge held the request and has given it up: it is its person's now, and the
				// agent is told who that is and where they decide it, once.
				if judging && !status.JudgeDeciding && state == "pending" {
					fmt.Fprintf(stderr, "agent-secrets: the model judge could not decide it; %s\n", waitingOn(c.baseURL, status.Approver, status.ApproveURL, false))
					judging = false
				}
			case retryable(err):
				// Said once per run of failed polls, not once per poll.
				if pollErr == nil {
					fmt.Fprintf(stderr, "agent-secrets: %s; still waiting for request %s\n", refusalText(src(), err), shownID)
				}
				pollErr = err
			default:
				refuse(stderr, "agent-secrets", src(), err)
				return 1
			}
			if state == "pending" && time.Since(sent) < time.Duration(asked)*time.Second {
				pollBackoff(min(backoff, time.Until(deadline)))
				backoff = nextBackoff(backoff)
			}
		}
	}
	switch state {
	case "granted":
	case "pending":
		if pollErr != nil {
			fmt.Fprintf(stderr, "agent-secrets: the wait for request %s to %s ran out while its status could not be read: %v; nothing was run. %s\n", shownID, src(), pollErr, statusHint(c.baseURL, requestID))
			return exitPending
		}
		fmt.Fprintf(stderr, "agent-secrets: request %s to %s is still waiting for approval; nothing was run. %s\n", shownID, src(), statusHint(c.baseURL, requestID))
		return exitPending
	case "denied":
		fmt.Fprintf(stderr, "agent-secrets: request %s to %s was denied%s\n", shownID, src(), deniedBecause(c.baseURL, detail))
		projectHint(stderr, detail)
		return exitDenied
	case "cancelled", "expired":
		fmt.Fprintf(stderr, "agent-secrets: request %s to %s was %s\n", shownID, src(), state)
		return 1
	default:
		fmt.Fprintf(stderr, "agent-secrets: request %s to %s is in unexpected state %s\n", shownID, src(), c.baseURL.Words(strconv.Quote(state)))
		return 1
	}
	if grantID == nil {
		fmt.Fprintf(stderr, "agent-secrets: request %s was granted but its grant from %s is no longer live; request it again\n", shownID, src())
		return 1
	}

	values, err := c.GrantValues(ctx, signer, *grantID, releaseCall{Command: record.ClipCommand(command), Reason: *reason})
	if err != nil {
		refuse(stderr, "agent-secrets", src(), err)
		return 1
	}
	// A broker from before the target reads no locations and releases the whole profile, so a
	// targeted run takes only a release that answers the target its request signed; anything else
	// is dropped before any value is read.
	if len(locations) > 0 && !slices.Equal(values.Locations, locations) {
		fmt.Fprintf(stderr, "agent-secrets: %s released %s without its --target, as a broker from before --target does; nothing was run: the broker is too old for --target\n", src(), names[0])
		return exitUsageError
	}
	// A name behind the airlock is released too: the broker hands over a placeholder and the
	// proxy that stands behind it (values.Airlock.Names) instead of the value, so only a name
	// in neither is one nothing was released for. A CLI from before the airlock finds such a
	// name in neither "values" nor anything it knows and refuses here, which is what makes a
	// broker with rules safe to deploy ahead of its clients: no old CLI runs a command with a
	// placeholder it cannot reach the value behind.
	var missing []string
	for _, name := range names {
		_, released := values.Values[name]
		if !released && values.Airlock != nil {
			_, released = values.Airlock.Names[name]
		}
		if !released {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "agent-secrets: the grant from %s released no value for %s; nothing was run\n", src(), strings.Join(missing, ", "))
		return 1
	}
	// What each GitHub profile's token leaves out is said before anything runs, and never a token.
	reportLeftOut(stderr, values.GitHubLeftOut)

	// The airlock's two files are written before anything is said or run: without the trust
	// bundle the command cannot verify the proxy, and a command that cannot reach its secret is
	// better refused here than failed halfway through its own work.
	var files airlockFiles
	if values.Airlock != nil {
		if files, err = prepareAirlock(os.Environ(), values.Airlock, *grantID, values.ExpiresAt); err != nil {
			fmt.Fprintf(stderr, "agent-secrets: %v; nothing was run\n", err)
			return 1
		}
	}
	childEnv, err := buildChildEnv(os.Environ(), values.Values, values.Airlock, files)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets: %v; nothing was run\n", err)
		return 1
	}

	// The one line the injected command's own output is preceded by: which names this process
	// puts in its environment, and that this CLI's broker and a store of which kind gave them,
	// never a value. A reader of the command's log can tell these values from another secrets
	// CLI's for the same names.
	fmt.Fprintf(stderr, "agent-secrets: %s from %s\n", strings.Join(names, ", "), src())
	if err := syscall.Exec(path, command, childEnv); err != nil {
		fmt.Fprintf(stderr, "agent-secrets: exec %s: %v\n", command[0], err)
		return 1
	}
	return 0 // unreachable: syscall.Exec replaces this process on success
}

// projectNotNamedHint is the line a denial for an unnamed private project adds: what the session
// can do about it. It names no project, because the broker names none — the person asking may be
// one who may not see it — so it says where the project is to be found instead.
const projectNotNamedHint = "agent-secrets: this secret belongs to a private project this session does not name; start the session with " +
	cipherclient.ProjectsVar + " naming that project (its owner, or `agent-secrets secret show NAME` under a credential that reads the store, says which)"

// projectHint writes projectNotNamedHint after a denial whose detail carries
// policy.ReasonProjectNotNamed, the one reason a session is told for a private project's secret
// it does not name; any other denial adds nothing.
func projectHint(stderr io.Writer, detail *string) {
	if detail != nil && strings.Contains(*detail, policy.ReasonProjectNotNamed) {
		fmt.Fprintln(stderr, projectNotNamedHint)
	}
}

// reportLeftOut says, for each GitHub profile of a release and each account of it, in order, the
// entries of the profile's permissions its token leaves out since the installation on that account
// does not hold them (the values answer's github_left_out), one line each. Every word is the
// broker's, so each prints on one line (redact.OneLine); none of them is a token.
func reportLeftOut(stderr io.Writer, leftOut map[string]map[string][]string) {
	for _, name := range slices.Sorted(maps.Keys(leftOut)) {
		for _, account := range slices.Sorted(maps.Keys(leftOut[name])) {
			fmt.Fprintf(stderr, "agent-secrets: the profile %s left out %s for %s, since the installation there does not hold them\n",
				redact.OneLine(name), redact.OneLine(strings.Join(leftOut[name][account], ", ")), redact.OneLine(account))
		}
	}
}

// sessionIdentityVars are the AGENT_SECRETS_* variables the exec form's child keeps: the broker's
// URL and the helper socket or key dir that say which session this is. The child is the same
// session — the helper signs for every descendant of the registered root, and a box's key never
// leaves the box — so an `agent-secrets` call it makes (a skill that nests a request, gh's token
// helper under `agent-secrets NAME -- omp`) must reach the broker as that session instead of
// failing with "AGENT_SECRETS_URL is required". Every other AGENT_SECRETS_* variable (a wait
// bound, an approve URL) configures this one invocation and is dropped.
var sessionIdentityVars = map[string]bool{
	"AGENT_SECRETS_URL":         true,
	"AGENT_SECRETS_HELPER_SOCK": true,
	"AGENT_SECRETS_KEY_DIR":     true,
}

// buildChildEnv is the exec form's child environment: the inherited environment minus every
// AGENT_SECRETS_* variable but sessionIdentityVars and, for every variable this CLI sets, minus
// its inherited entry too, before the set ones are appended. Without that second strip, a
// duplicate key from the inherited environment could shadow the broker-released value under an
// execve implementation that keeps the first occurrence of a repeated key rather than the last —
// and, with the airlock, an inherited HTTPS_PROXY or SSL_CERT_FILE could shadow the proxy and the
// trust bundle the command must use.
//
// What this CLI sets is every released value, and, for a grant the airlock holds a name of
// (release, and files, the two files prepareAirlock has written for it), that name's placeholder,
// the AWS variables its slots fill, the proxy, and the trust bundle (airlockVars). release is nil
// for every grant the airlock holds no name of, which is every grant of a broker with no rules
// file, and then nothing of the airlock is in the child's environment at all.
//
// The set entries are appended in sorted order, so one grant's child environment is one string
// whatever Go's map iteration gave.
func buildChildEnv(environ []string, values map[string]string, release *AirlockRelease, files airlockFiles) ([]string, error) {
	set := make(map[string]string, len(values))
	maps.Copy(set, values)
	if release != nil {
		vars, err := airlockVars(environ, release, files)
		if err != nil {
			return nil, err
		}
		maps.Copy(set, vars)
	}
	envp := make([]string, 0, len(environ)+len(set))
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "AGENT_SECRETS_") && !sessionIdentityVars[key] {
			continue
		}
		if _, ours := set[key]; ours {
			continue
		}
		envp = append(envp, e)
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		envp = append(envp, name+"="+set[name])
	}
	return envp, nil
}
