package appauth

// Token mode: an App configured with github_apps.<role>.token_command, for a deployment whose App
// keys only a secrets broker holds. The Manager holds no key and signs no JWT for such an App: it
// runs the command, with `sh -c`, whenever it needs a lease (Token's refreshWindow governs when, as
// in key mode), and reads from its stdout the release the secrets broker's `agent-secrets
// <PROFILE> -- <cmd>` puts in <cmd>'s environment for a profile whose value is installations:
//
//	{"installations":{"<account login>":{"installation_id":<int>,"token":"ghs_…","expires_at":"<RFC3339>"}}}
//
// one entry per installation of the App the release covers. The command's environment names what
// the daemon wants: TokenCommandOwner, the account whose installation token it needs ("" for every
// installation the command may release), and TokenCommandReadOnly, "1" for a read-only token
// (ReadToken) and "" otherwise; no other variable of their namespace reaches the command, whatever
// the daemon inherited (tokenCommandNamespace). An operator's command maps the first onto the
// --target that narrows the profile to that one account, and the second onto the role's read
// profile, the write profile's name with _READ after it (docs/deployment.md):
//
//	P=<PROFILE>${LEGION_GITHUB_READ_ONLY:+_READ}; agent-secrets "$P" ${LEGION_GITHUB_OWNER:+--target "$LEGION_GITHUB_OWNER"} -- printenv "$P"
//
// The release reaches every repository the account's installation holds, here as in key mode: the
// account that installs the App chooses them, and the daemon asks for no narrowing of them. A
// token holds what its profile names, cut by the broker to what the installation holds, and the
// deployment's profiles file decides that: the profiles docs/deployment.md gives name this
// package's own sets, writePermissions and readPermissions, so a lease holds the same in either
// mode. What a release holds is the command's own business: the daemon reads what an
// installation is granted only where it mints with a key of its own (exchangeGranted), and asks
// a command for no permission at all.
//
// No token reaches a log or an error: the command's stdout is never quoted, and its stderr, which
// `agent-secrets` uses for the names and the request it waits on, goes to the daemon's stderr and
// into a failure's message.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

const (
	// TokenCommandOwner names, in a token_command's environment, the account whose installation
	// token the daemon wants, empty for every installation the command may release.
	TokenCommandOwner = "LEGION_GITHUB_OWNER"
	// TokenCommandReadOnly is "1" in a token_command's environment when the daemon wants a
	// read-only token (ReadToken), and empty otherwise.
	TokenCommandReadOnly = "LEGION_GITHUB_READ_ONLY"
)

// tokenCommandNamespace is the namespace of the variables the daemon sets in a token_command's
// environment, TokenCommandOwner and TokenCommandReadOnly. A variable of it the daemon inherited
// never reaches the command: execTokenCommand drops every one before it sets its own, so the
// command reads no value the daemon did not mean, nor any name of it the daemon does not set.
const tokenCommandNamespace = "LEGION_GITHUB_"

// releaseLimit bounds what a token_command may print: a release of a few installations is a few
// kilobytes.
const releaseLimit = 1 << 20

// tokenCommandLimit bounds one run of a token_command. A run is not its caller's: a caller whose
// own deadline ends first returns ErrCommandPending while the run goes on, and the next caller
// asking the same thing joins it, so a grant that waits on a person's approval is one request,
// never killed and asked again. `agent-secrets` waits 30 minutes for a decision by default, then
// exits 75; the limit leaves it that.
var tokenCommandLimit = 35 * time.Minute

// exitPending is `agent-secrets`' exit status for a request still waiting on a person's approval
// when its wait ended: no answer about the App, so a later run may pass.
const exitPending = 75

// brokerRequestPending is the code the secrets broker refuses a request with while another request
// of the session for the same name waits (packages/envoy/internal/cipher/api,
// handlers_requests.go), which `agent-secrets` prints on stderr and exits exitRefused with. A
// profile's grant is reused, and its waiting request joined, only for the same target, so while a
// human-tier profile's request for one owner waits on a person, the request for another owner is
// refused this way. Like exit 75 it is no answer about the App: a later run may pass. Only a run
// that exited exitRefused is read so; the text in a run that exited otherwise is not
// `agent-secrets`' refusal.
const brokerRequestPending = "(REQUEST_PENDING)"

// exitRefused is `agent-secrets`' exit status for a refusal its message names.
const exitRefused = 1

// ErrCommandPending is a token_command still running when its caller's deadline ended, wrapped in
// a TransientError: most often a grant that waits on a person's approval. The run goes on, and the
// caller's next attempt joins it.
var ErrCommandPending = errors.New("token_command is still running")

// ErrApprovalPending is a token_command that ended on the secrets broker's request waiting on a
// person: `agent-secrets` exited 75 (exitPending), or the broker refused it REQUEST_PENDING
// (brokerRequestPending). Its TransientError carries the command's own failure, which reads as
// this error too (approvalPending).
var ErrApprovalPending = errors.New("the secrets broker's request waits on a person's approval")

// approvalPending is a token_command's failure that ErrApprovalPending names, with the failure's
// own text.
type approvalPending struct{ err error }

func (e approvalPending) Error() string        { return e.err.Error() }
func (e approvalPending) Unwrap() error        { return e.err }
func (e approvalPending) Is(target error) bool { return target == ErrApprovalPending }

// Pending reports a token_command's grant that waits rather than an answer of GitHub's: the
// command still running past its caller's deadline (ErrCommandPending), or the broker's request
// waiting on a person (ErrApprovalPending). Each is a TransientError, since a later run may
// pass, but it is about one owner's grant at the broker, never GitHub's rate limit or its own
// trouble, so a caller that holds every owner's GitHub calls on GitHub's trouble holds none on it
// (internal/daemon's reviewAppClient).
func Pending(err error) bool {
	return errors.Is(err, ErrCommandPending) || errors.Is(err, ErrApprovalPending)
}

// releasedInstallation is one installation of a token_command's release.
type releasedInstallation struct {
	owner     string
	token     string
	expiresAt time.Time
}

// commandRun is one run of a token_command, which every caller asking for the same owner and
// read-only flag while it runs shares.
type commandRun struct {
	done          chan struct{}
	installations map[string]releasedInstallation
	err           error
}

// tokenCommandField is the configuration key of role's token_command, the name its failures carry.
func tokenCommandField(role AppRole) string {
	return "github_apps." + string(role) + ".token_command"
}

// runTokenCommand returns the release of app's token_command for the request's owner ("" for
// every installation) and read, keyed by lowercased account login. A release of every
// installation records its accounts as the role's installations (recordOwners), the set a
// narrowed run that fails is read against. Every read of a token_command goes through it
// (mintFromCommand, anyInstallation, readInstalledOwners), and these are the rules of its run.
//
// The run belongs to no caller: it runs on the Manager's own context, bounded by
// tokenCommandLimit, and every caller of the same scope joins it while it runs, so a caller that
// leaves ends neither the run nor the wait of a caller that joined it. The run is keyed by the
// request's own scope (leaseScope), the key its lease is cached under: a read-only run must never
// answer a caller that asked for a write token, and one encoding of a scope serves both.
//
// A caller whose ctx ends before the run does is answered ErrCommandPending in a TransientError,
// which wraps ctx's error too, while the command goes on, so a caller that tells its own
// context's end by that error reads it as such. The error names the owner, or every installation
// where the owner is "". The boot's key proof reads that answer as pending and spends no attempt
// on it (internal/daemon's appAttempt), so a grant that waits on a person's approval holds the
// boot without ending it.
//
// A run that finishes with no caller waiting is kept for finishedRunKeep, so a caller's next
// attempt gets the release of the run its earlier attempt started, and a grant a person approved
// is not asked for again. A caller that gets a finished run's result forgets the run (forgetRun),
// so the next caller of the scope joins another run in flight or runs the command again.
func (m *Manager) runTokenCommand(ctx context.Context, app config.GitHubApp, request mintRequest) (map[string]releasedInstallation, error) {
	role, owner := request.role, request.owner
	key := request.scope()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: the token manager is closed", tokenCommandField(role))
	}
	run, ok := m.commands[key]
	if !ok {
		run = &commandRun{done: make(chan struct{})}
		m.commands[key] = run
		m.runs.Add(1)
		go func() {
			defer m.runs.Done()
			bounded, cancel := context.WithTimeout(m.root, tokenCommandLimit)
			defer cancel()
			run.installations, run.err = m.execTokenCommand(bounded, app, request)
			if run.err == nil && owner == "" {
				m.recordOwners(role, run.installations)
			}
			m.mu.Lock()
			close(run.done)
			m.mu.Unlock()
			// A caller whose deadline ended may come back after the run finished: keep the result
			// for finishedRunKeep, or until a caller reads it, so that caller gets it and the
			// command is not run again.
			time.AfterFunc(finishedRunKeep, func() { m.forgetRun(key, run) })
		}()
	}
	m.mu.Unlock()
	select {
	case <-run.done:
		m.forgetRun(key, run)
		return run.installations, run.err
	case <-ctx.Done():
		release := "every installation"
		if owner != "" {
			release = strconv.Quote(owner)
		}
		return nil, &TransientError{Err: fmt.Errorf("%s for %s is still running after %w (a grant may wait on a person's approval): %w", tokenCommandField(role), release, ctx.Err(), ErrCommandPending)}
	}
}

// finishedRunKeep is how long a finished token_command run's result waits for a caller whose own
// deadline ended before the run did: longer than the boot's longest wait between attempts
// (the production App-call retry's Max, 30 s; internal/daemon's productionAppCalls), far shorter than a released token's life.
var finishedRunKeep = 2 * time.Minute

// forgetRun drops run from the shared runs if it is still key's run.
func (m *Manager) forgetRun(key scope, run *commandRun) {
	m.mu.Lock()
	if m.commands[key] == run {
		delete(m.commands, key)
	}
	m.mu.Unlock()
}

// Close ends every token_command still running and waits for each to exit; a later call needing
// one is refused. The closed mark is set under the lock that accepts a run, so every run accepted
// before it is counted before Close waits. The daemon calls it as the workflow stops.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.stop()
	m.runs.Wait()
}

// recordOwners records the accounts of a release of every installation as role's installations.
func (m *Manager) recordOwners(role AppRole, installations map[string]releasedInstallation) {
	owners := make(map[string]string, len(installations))
	for owner := range installations {
		owners[owner] = owner
	}
	m.mu.Lock()
	m.installations[role] = owners
	m.mu.Unlock()
}

// execTokenCommand runs app's token_command once, under ctx, for the request's owner and read.
// A run ctx ended, one that exited 75 (`agent-secrets`' request still waiting for approval), and
// one the broker refused REQUEST_PENDING (another target's request of the same profile still
// waiting, brokerRequestPending) are transient, the last two read as ErrApprovalPending too; one
// that exited otherwise, printed too much, or printed no release is the configuration's answer.
// Its stderr reaches the daemon's and the error with every GitHub token redacted.
func (m *Manager) execTokenCommand(ctx context.Context, app config.GitHubApp, request mintRequest) (map[string]releasedInstallation, error) {
	field := tokenCommandField(request.role)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(runCtx, "sh", "-c", app.TokenCommand)
	procgroup.Configure(command)
	readOnlyValue := ""
	if request.read {
		readOnlyValue = "1"
	}
	// Every variable of the daemon's namespace it inherited is dropped (tokenCommandNamespace),
	// and both of its own are set, empty where the daemon asks nothing by them, so a value the
	// daemon inherited never reads as one it meant.
	command.Env = append(slices.DeleteFunc(runtime.WithoutNATSSeeds(os.Environ()), func(entry string) bool {
		return strings.HasPrefix(entry, tokenCommandNamespace)
	}), TokenCommandOwner+"="+request.owner, TokenCommandReadOnly+"="+readOnlyValue)
	stdout := &boundedBuffer{limit: releaseLimit, overflow: cancel}
	var stderr bytes.Buffer
	relay := &redactingLines{out: m.stderr, keep: &stderr, limit: releaseLimit, overflow: cancel}
	command.Stdout = stdout
	command.Stderr = relay
	err := command.Run()
	relay.flush()
	if stdout.overflowed {
		return nil, fmt.Errorf("%s printed more than %d bytes, which no release is", field, releaseLimit)
	}
	if relay.overflowed {
		return nil, fmt.Errorf("%s wrote more than %d bytes to stderr", field, releaseLimit)
	}
	if ctx.Err() != nil {
		return nil, &TransientError{Err: fmt.Errorf("%s did not finish within %s: %w", field, tokenCommandLimit, ctx.Err())}
	}
	if err != nil {
		end := procgroup.Ended(err)
		if !end.Ran() {
			return nil, fmt.Errorf("%s could not run: %w", field, err)
		}
		failed := fmt.Errorf("%s failed (%s)", field, end)
		if message := strings.TrimSpace(stderr.String()); message != "" {
			failed = fmt.Errorf("%s failed (%s): %s", field, end, message)
		}
		if end.ExitedWith(exitPending) || (end.ExitedWith(exitRefused) && strings.Contains(stderr.String(), brokerRequestPending)) {
			return nil, &TransientError{Err: approvalPending{err: failed}}
		}
		return nil, failed
	}
	return parseRelease(stdout.buf.Bytes(), field, m.now())
}

// boundedBuffer keeps at most limit bytes of a command's stdout; the first write past it marks the
// output overflowed and calls overflow, which ends the command, so no broken command fills the
// daemon's memory.
type boundedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflow   func()
	overflowed bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.overflowed || b.buf.Len()+len(p) > b.limit {
		if !b.overflowed {
			b.overflowed = true
			b.overflow()
		}
		return 0, errors.New("token_command output exceeds its limit")
	}
	return b.buf.Write(p)
}

// tokenPattern matches the GitHub token forms (installation, OAuth, user-to-server, refresh,
// personal, fine-grained personal), which a command's stderr may carry and no log or error may.
var tokenPattern = regexp.MustCompile(`(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})`)

// redactingLines writes each whole line of a command's stderr to out, and to keep for the failure's
// message, with every GitHub token in it replaced, so `agent-secrets`' line naming a pending request
// reaches the daemon's log as it is written while a token never does; flush writes a last line with
// no newline. Past limit bytes in all it marks the stream overflowed, calls overflow, which ends the
// command, and relays nothing more.
type redactingLines struct {
	out        io.Writer
	keep       *bytes.Buffer
	limit      int
	overflow   func()
	overflowed bool
	seen       int
	pending    []byte
}

func (r *redactingLines) Write(p []byte) (int, error) {
	if r.overflowed {
		return 0, errors.New("token_command stderr exceeds its limit")
	}
	r.seen += len(p)
	if r.seen > r.limit {
		r.overflowed, r.pending = true, nil
		r.overflow()
		return 0, errors.New("token_command stderr exceeds its limit")
	}
	r.pending = append(r.pending, p...)
	for {
		end := bytes.IndexByte(r.pending, '\n')
		if end < 0 {
			break
		}
		r.emit(r.pending[:end+1])
		r.pending = r.pending[end+1:]
	}
	return len(p), nil
}

func (r *redactingLines) emit(line []byte) {
	clean := tokenPattern.ReplaceAll(line, []byte("[redacted]"))
	_, _ = r.out.Write(clean)
	r.keep.Write(clean)
}

func (r *redactingLines) flush() {
	if len(r.pending) > 0 && !r.overflowed {
		r.emit(r.pending)
	}
	r.pending = nil
}

// loginPattern is a GitHub account login, ghrepo's ownerName: what a release may name an
// installation by, and the only owner text a refusal of a release quotes. It is a letter or digit
// and then up to 38 letters, digits, hyphens and underscores. An underscore is admitted because a
// managed user's login ends in `_<shortcode>` (`mona-cat_octo`), and the broker releases such an
// account's installation like any other: a rule here narrower than ghrepo's refused the whole
// release, so no owner of it was minted for. The 39 characters are GitHub's own bound, which no
// login exceeds.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)

// parseRelease reads a token_command's stdout as the broker's release. Every refusal names the
// field and the account and never quotes the output, which holds tokens.
func parseRelease(output []byte, field string, now time.Time) (map[string]releasedInstallation, error) {
	var release struct {
		Installations map[string]struct {
			InstallationID json.RawMessage `json:"installation_id"`
			Token          string          `json:"token"`
			ExpiresAt      string          `json:"expires_at"`
		} `json:"installations"`
	}
	// encoding/json's syntax errors quote the offending input, which holds tokens: the error is
	// dropped and named.
	if json.Unmarshal(bytes.TrimSpace(output), &release) != nil || release.Installations == nil {
		return nil, fmt.Errorf(`%s printed no release (want {"installations":{"<account>":{"installation_id":…,"token":…,"expires_at":…}}})`, field)
	}
	installations := make(map[string]releasedInstallation, len(release.Installations))
	for owner, entry := range release.Installations {
		// Every refusal after this one quotes the key, and a token's text is what no error may
		// carry, so a key holding a token form tokenPattern names is refused here, unquoted. A
		// real token is 40 characters or more and already fails loginPattern's 39; this closes
		// the shorter text spelled as one, which the underscore a login holds would otherwise let
		// through. A login that merely opens like a token (`ghs_acme`) holds no token form and is
		// read.
		if !loginPattern.MatchString(owner) || tokenPattern.MatchString(owner) {
			return nil, fmt.Errorf("%s released an installation whose account is not a GitHub login", field)
		}
		if _, err := installationID(entry.InstallationID); err != nil {
			return nil, fmt.Errorf("%s released %s's installation with no installation id", field, owner)
		}
		expiresAt, err := time.Parse(time.RFC3339, entry.ExpiresAt)
		if err != nil || entry.Token == "" {
			return nil, fmt.Errorf("%s released %s's installation without a token and an RFC 3339 expires_at", field, owner)
		}
		if !expiresAt.After(now) {
			return nil, fmt.Errorf("%s released %s's installation with a token that expired at %s", field, owner, expiresAt.Format(time.RFC3339))
		}
		installations[strings.ToLower(owner)] = releasedInstallation{owner: owner, token: entry.Token, expiresAt: expiresAt}
	}
	return installations, nil
}

// brokerNoInstallation is the secrets broker's refusal of a release for an account the App has no
// installation on (packages/envoy/internal/cipher/secrets/githubapp.go, GitHubApp.Mint), which
// `agent-secrets` prints on stderr and runTokenCommand's error carries.
const brokerNoInstallation = "has no installation on"

// brokerNarrowingRefused is the secrets broker's refusal of a profile it cannot serve for the
// owner (packages/envoy/internal/cipher/secrets/minterr.go, ErrMintNarrowingRefused), carried the
// same way: the profile's permissions, cut to what the installation holds, leave none but
// metadata (ErrMintEmptyCut), or GitHub answered its token request 422. Either is the key mode's
// ErrNarrowingRefused by another road.
// The command is told no repository, so none is among what the broker refuses. The broker's text
// never quotes GitHub's own and never a token.
const brokerNarrowingRefused = "the narrowing cannot be served"

// mintFromCommand is mint in token mode: the command's release for the request's owner, of the
// read profile where it is the read token, and the token checked to be the configured App's on
// every release (tokenIdentity, since a later release could carry another App's token). A
// release without owner's installation is ErrNotInstalled, and so is a run that failed with the
// broker's refusal for an account with no installation, or failed for an owner the last release
// of every installation did not name, such as an account outside the profile: no second, wider
// release is asked for to decide it, so a read-only request never mints a write token.
//
// A run the broker refused for the profile's scope itself (brokerNarrowingRefused) is
// ErrNarrowingRefused, the refusal a key-mode exchange answers a 422 with, and a caller that can
// act on that reads it the same way in either mode (ErrNarrowingRefused).
func (m *Manager) mintFromCommand(ctx context.Context, app config.GitHubApp, request mintRequest) (Lease, error) {
	role, owner := request.role, request.owner
	notInstalled := fmt.Errorf("%w: the %s App's %s released no installation on %s", ErrNotInstalled, role, tokenCommandField(role), owner)
	installations, err := m.runTokenCommand(ctx, app, request)
	if err != nil {
		var transient *TransientError
		switch {
		case errors.As(err, &transient):
			return Lease{}, err
		case strings.Contains(err.Error(), brokerNoInstallation):
			return Lease{}, fmt.Errorf("%w: %w", notInstalled, err)
		case strings.Contains(err.Error(), brokerNarrowingRefused):
			return Lease{}, fmt.Errorf("%w: the %s App's %s released no token for %s: %w",
				ErrNarrowingRefused, role, tokenCommandField(role), owner, err)
		}
		m.mu.Lock()
		known, recorded := m.installations[role]
		_, listed := known[strings.ToLower(owner)]
		m.mu.Unlock()
		if recorded && !listed {
			return Lease{}, fmt.Errorf("%w: %w", notInstalled, err)
		}
		return Lease{}, err
	}
	installation, ok := installations[strings.ToLower(owner)]
	if !ok {
		return Lease{}, notInstalled
	}
	identity, err := m.tokenIdentity(ctx, role, app.AppID, installation.token)
	if err != nil {
		return Lease{}, err
	}
	return Lease{Token: installation.token, ExpiresAt: installation.expiresAt, Minted: m.now(), Identity: identity}, nil
}

// anyInstallation is one installation of the release of every installation app's token_command
// gives, the first by account login: AppLogin needs a token of any installation, and no
// owner's.
//
// It is the one release the daemon asks for that names no owner, and it is the write profile's:
// the run is told no owner and no read-only flag. The release is what lists the installations
// themselves, and the login it is read for is read before any owner of the project is known. What
// it is used for is read-only: the GraphQL viewer read that names the App's bot login and the GET
// /apps/<slug> that checks the App's id (tokenIdentity), and the token is never cached as a lease
// nor handed to a role.
//
// It asks for the scope InstalledOwners reads in token mode (mintRequest{role: role}), so the two
// share what runTokenCommand shares and keeps of a run: a login read joins a run in flight that
// an installation read started, or takes a finished run kept for finishedRunKeep and forgets it,
// as every caller that takes a run does, so the next installation read of the role does not get
// that release but joins another run in flight or runs the command again. At the boot the logins
// read runs before the key proof (internal/daemon's openWorkflow) and passes only once it has
// taken a run's release for each role, so the key proof never takes a release the logins read
// was answered with: its first read of each role joins a run in flight or runs the command again.
// The installation AppLogin took stays its own (loginRelease), never the shared run's.
func (m *Manager) anyInstallation(ctx context.Context, role AppRole, app config.GitHubApp) (releasedInstallation, error) {
	installations, err := m.runTokenCommand(ctx, app, mintRequest{role: role})
	if err != nil {
		return releasedInstallation{}, err
	}
	if len(installations) == 0 {
		return releasedInstallation{}, fmt.Errorf("%s released no installation of the %s App", tokenCommandField(role), role)
	}
	owners := make([]string, 0, len(installations))
	for owner := range installations {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	return installations[owners[0]], nil
}

// loginRelease is the installation role's AppLogin reads the App's login under in token mode:
// the one an earlier AppLogin of the role took and kept (loginReleases), while more than
// refreshWindow of it is left, as a lease is served; else one of a release the command gives now
// (anyInstallation), which it keeps. Each run of the command asks the secrets broker for a
// release, which may wait on a person's approval of a grant, so a login read that failed after
// it took its release (GitHub's trouble on the viewer read, its attempt's bound) reads again
// under the same token rather than run the command a second time. A kept installation that has
// come within refreshWindow of its expiry, as one can across the start's waits for GitHub's rate
// limit, is replaced by a new release: the command runs again.
func (m *Manager) loginRelease(ctx context.Context, role AppRole, app config.GitHubApp) (releasedInstallation, error) {
	m.mu.Lock()
	kept, ok := m.loginReleases[role]
	m.mu.Unlock()
	if ok && kept.expiresAt.Sub(m.now()) > refreshWindow {
		return kept, nil
	}
	installation, err := m.anyInstallation(ctx, role, app)
	if err != nil {
		return releasedInstallation{}, err
	}
	m.mu.Lock()
	m.loginReleases[role] = installation
	m.mu.Unlock()
	return installation, nil
}

// forgetLoginRelease drops the installation role's AppLogin kept, once the App's login is read
// under it.
func (m *Manager) forgetLoginRelease(role AppRole) {
	m.mu.Lock()
	delete(m.loginReleases, role)
	m.mu.Unlock()
}

// tokenIdentity is the bot identity of the App whose installation token token is, with no JWT:
// verifyTokenApp checks the token is appID's App's and names its bot login, run for every token,
// and GET /users/<login> gives the bot user's id for its noreply email, read once per App.
func (m *Manager) tokenIdentity(ctx context.Context, role AppRole, appID, token string) (GitIdentity, error) {
	login, err := m.verifyTokenApp(ctx, role, appID, token)
	if err != nil {
		return GitIdentity{}, err
	}
	identity, err := m.identity(ctx, appID, func(ctx context.Context) (GitIdentity, error) {
		return m.botIdentity(ctx, login, token)
	})
	if err != nil {
		return GitIdentity{}, err
	}
	if identity.Name != login {
		return GitIdentity{}, fmt.Errorf("the %s App's %s released a token that acts as %s, where the App acted as %s", role, tokenCommandField(role), login, identity.Name)
	}
	return identity, nil
}

// verifyTokenApp returns the bot login token acts as, refusing a token that is not the App appID's:
// GraphQL's viewer names the bot login, `<slug>[bot]`, and GET /apps/<slug> must answer appID, so a
// command that releases another App's token is refused rather than acted as.
func (m *Manager) verifyTokenApp(ctx context.Context, role AppRole, appID, token string) (string, error) {
	login, err := m.viewerLogin(ctx, role, token)
	if err != nil {
		return "", err
	}
	slug, ok := strings.CutSuffix(login, "[bot]")
	if !ok || slug == "" {
		return "", fmt.Errorf("the %s App's %s released a token that acts as %s, not an App's bot", role, tokenCommandField(role), login)
	}
	if err := m.checkAppID(ctx, role, slug, appID, token); err != nil {
		return "", err
	}
	return login, nil
}

// viewerLogin is the login GitHub's GraphQL viewer answers for token.
func (m *Manager) viewerLogin(ctx context.Context, role AppRole, token string) (string, error) {
	what := fmt.Sprintf("the %s App's bot login (GraphQL viewer)", role)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/graphql", strings.NewReader(`{"query":"{viewer{login}}"}`))
	if err != nil {
		return "", err
	}
	tokenHeaders(request, token)
	request.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return "", &TransientError{Err: fmt.Errorf("read %s: %w", what, err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", m.transient(response, refusalBody(response), fmt.Errorf("read %s failed (%d)", what, response.StatusCode))
	}
	body, err := readBody(response, "read "+what, responseBound)
	if err != nil {
		return "", err
	}
	var answer struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Data.Viewer.Login == "" {
		return "", fmt.Errorf("read %s: GitHub answered no login", what)
	}
	return answer.Data.Viewer.Login, nil
}

// checkAppID reads GET /apps/<slug> and refuses an App whose id is not appID.
func (m *Manager) checkAppID(ctx context.Context, role AppRole, slug, appID, token string) error {
	what := fmt.Sprintf("the %s App %s (GET /apps/%s)", role, slug, slug)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/apps/"+url.PathEscape(slug), nil)
	if err != nil {
		return err
	}
	tokenHeaders(request, token)
	response, err := m.client.Do(request)
	if err != nil {
		return &TransientError{Err: fmt.Errorf("read %s: %w", what, err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return m.transient(response, refusalBody(response), fmt.Errorf("read %s failed (%d)", what, response.StatusCode))
	}
	body, err := readBody(response, "read "+what, responseBound)
	if err != nil {
		return err
	}
	var app struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &app) != nil {
		return fmt.Errorf("read %s: GitHub answered no App id", what)
	}
	id, err := installationID(app.ID)
	if err != nil {
		return fmt.Errorf("read %s: GitHub answered no App id", what)
	}
	if id != appID {
		return fmt.Errorf("the %s App's %s released a token of App %s (%s), not of the configured app_id %s", role, tokenCommandField(role), id, slug, appID)
	}
	return nil
}

// installedFromCommand is InstalledOn in token mode: the App is installed on repo when the release
// for repo's owner names an installation whose repositories (GET /installation/repositories, every
// page) include repo. GET /repos/{owner}/{repo} would not do: an installation token reads any
// public repository, installed or not. A cached lease GitHub answers 401 (an App uninstalled while
// its token lived) is dropped and minted once again, so an uninstall reads as not installed rather
// than an error until the lease expires.
func (m *Manager) installedFromCommand(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error) {
	for attempt := 0; ; attempt++ {
		installed, refused, err := m.listInstallation(ctx, role, repo)
		if !refused || attempt > 0 {
			return installed, err
		}
		m.Forget(role, repo.Owner())
	}
}

// listInstallation reads whether repo is among the repositories of its owner's installation,
// under that owner's read lease; refused reports a 401, a token GitHub no longer honours.
//
// The listing is the whole answer. No token request names a repository any longer
// (INFRA-2543), so neither GitHub nor the broker refuses one, and the one thing that says
// whether the App reaches this repository is the installation's own list of them, read page by
// page under the owner's read release. An owner the App has no installation on mints nothing
// and is not installed (ErrNotInstalled); every other failure is a failure, a refused narrowing
// among them.
//
// The read is that answer's one source, so it drops whatever the Manager remembers of that
// scope's refusal first (forgetNarrowingRefusal) and mints: a repository's preparation makes the
// check precisely when an operator may have changed something — the App's installation above all
// — and a refusal served from the five minutes one is remembered for would fail the check for an
// account the App was just installed on (lease, narrowingRefusalWindow, INFRA-513). A refusal
// the mint itself meets is remembered as any mint's is: it is GitHub's answer of this moment,
// which the bundles refreshing behind the check would otherwise ask for again at once, and the
// next preparation's check drops it again as this one did.
func (m *Manager) listInstallation(ctx context.Context, role AppRole, repo ghrepo.Repository) (installed, refused bool, err error) {
	m.forgetNarrowingRefusal(role, repo.Owner())
	lease, err := m.ReadToken(ctx, role, repo.Owner())
	if errors.Is(err, ErrNotInstalled) {
		return false, false, nil
	}
	what := fmt.Sprintf("read whether the %s App is installed on %s", role, repo)
	if err != nil {
		return false, false, fmt.Errorf("%s: %w", what, err)
	}
	for page := 1; ; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			m.baseURL+"/installation/repositories?per_page=100&page="+strconv.Itoa(page), nil)
		if err != nil {
			return false, false, err
		}
		tokenHeaders(request, lease.Token)
		response, err := m.client.Do(request)
		if err != nil {
			return false, false, &TransientError{Err: fmt.Errorf("%s: %w", what, err)}
		}
		if response.StatusCode == http.StatusUnauthorized {
			return false, true, m.answered(response, refusalBody(response), fmt.Errorf("%s failed (401)", what))
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return false, false, m.transient(response, refusalBody(response), fmt.Errorf("%s failed (%d)", what, response.StatusCode))
		}
		body, err := readBody(response, what, responseBound)
		if err != nil {
			return false, false, err
		}
		var listing struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if json.Unmarshal(body, &listing) != nil {
			return false, false, fmt.Errorf("%s: GitHub answered an invalid repository list", what)
		}
		for _, listed := range listing.Repositories {
			if strings.EqualFold(listed.FullName, repo.String()) {
				return true, false, nil
			}
		}
		if len(listing.Repositories) < 100 {
			return false, false, nil
		}
	}
}

// tokenHeaders sets an installation token's headers on request.
func tokenHeaders(request *http.Request, token string) {
	request.Header.Set("Authorization", "token "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
}
