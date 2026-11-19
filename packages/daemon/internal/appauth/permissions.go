package appauth

// The permission cut: what a role's token asks GitHub for, held against what the owner's account
// has approved for the App's installation. GitHub answers a request naming a permission the
// installation is not granted HTTP 422 and mints nothing, so a mint reads the grant first
// (grantedPermissions), asks for what the role's set and the grant have in common
// (narrowToGranted) and says once what the token therefore cannot do (sayShortfalls).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// writePermissions is the permission set a role's write token is asked for, one per App role:
// what that App's roles do to the repositories they work in, and nothing else. It is what
// the mint asks for of an installation granted all of it; an installation granted less is asked
// for what it is granted (exchangeGranted), since GitHub answers a request naming a permission
// the installation is not granted HTTP 422, minting nothing rather than leaving that one out.
//
// Every entry has its user. workflows:write is a push that touches `.github/workflows`, which
// GitHub refuses from a token without it; contents:write is that push and every other, each role
// pushing its own commits; pull_requests:write is the pull request, its comments and its reviews;
// actions:write is `gh run rerun`, a re-run being a write even though the role only reads what it
// produces; metadata:read is every read of the repository itself, which GitHub requires of any
// token. checks and statuses are the check runs and commit statuses of a head, and differ by App:
// the review App's set asks for checks:write, its reviewer creating the check runs, and
// statuses:read, because its roles read the commit statuses of a head, where the implement App's
// asks for checks:read, what its roles read of those check runs, and statuses:write, which is what
// the implement App declares. No role of either App posts a commit status: a tester's verdict is a
// check run and a comment (docs/solutions/legion/one-role-keyed-table-decides-which-github-app-acts.md).
//
// attestations:read is in both sets, because a role reads the attestations of an artifact, such
// as to check that an image was built from the default branch, and no role makes one: a release
// workflow does. Neither set asks for more than read of it (readPermissions says where a token
// without it was refused).
//
// The set says what the App's roles use, never what an installation holds: what an account has
// approved is the account's, and the cut takes each mint's request down to it (exchangeGranted),
// so an installation granted less than a set names is asked for less rather than refused.
//
// issues:write is in both sets, though the daemon itself reads and writes no GitHub issue: the
// roles do, through their tokens. The implementer runs the work's own code, which may read an
// issue of the repository it works in and post or edit a comment there, such as a release script
// marking that an issue's run should stop or be tried again. The tester runs that same code under
// the review App, so that App's set needs what the code needs. The write lease already holds
// contents:write and pull_requests:write wherever it reaches, so issues:write adds no new kind of
// harm; giving the review App's other roles issues read alone would mean keying the set by
// workflow role rather than by App, a larger change for no measured risk. A token whose request
// names no issues permission is refused an issue of a private repository with 403 "Resource not
// accessible by integration" (readPermissions says where that was measured).
//
// Repository projects and packages are in neither set, though both Apps' installations hold
// them: Legion writes no project and no package.
//
// security_events is the code-scanning alerts of the repository, and it differs by App as checks
// does: the review App's set asks for write and the implement App's for read, its roles reading
// which alerts stand on a head. The write is what a dismissal of an alert whose code is right
// needs, and the set is keyed by App rather than by workflow role, so the token serves every role
// the review App mints for — architect, planner, tester and reviewer (AppRoleFor, identity.go) —
// and each of them can dismiss an alert, upload SARIF or delete an analysis. A dismissal names
// the App in its dismissed_by, whichever of those roles made it, so which role acted is readable
// only where the comment the dismissal carries says so; an upload or a deletion leaves neither.
//
// It is here because, in key mode, a token minted without it is refused those reads whatever the
// installation holds: measured on 2026-11-10, every code-scanning read of both role Apps answered
// 403 with `X-Accepted-Github-Permissions: security_events=read` while the accounts' grants of
// 2026-11-08 stood, because this file named the permission in neither set and GitHub mints a
// token carrying what the request names. A deployment in token mode asks its command for no
// permissions at all (tokencommand.go), so this set decides nothing there (dispatch://INFRA-1639).
var writePermissions = map[AppRole]map[string]string{
	Implement: {"actions": "write", "attestations": "read", "checks": "read", "contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write", "security_events": "read", "statuses": "write", "workflows": "write"},
	Review:    {"actions": "write", "attestations": "read", "checks": "write", "contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write", "security_events": "write", "statuses": "read"},
}

// readPermissions is the read set: what a read lease asks GitHub for, each permission at read. It
// is what a controller needs of every repository its App is installed on, which it works none of
// (ControllerCredential), and what the provisioning token takes of the issue's own repository,
// which a clone, a fetch and a repository cache only read (implementTokens,
// internal/daemon). A mint asks for what the installation is granted of this set, at read
// (exchangeGranted), and GitHub mints a token with the permissions the request names, so an
// installation granted less gets a lease that holds less. This package alone declares the set.
// The secrets broker's read-only release, which a daemon on token_command gets in its place
// (tokencommand.go), holds the broker's own set, contents, metadata, pull requests and Actions,
// which differs from this one.
//
// The set is the most a read lease holds, and the only thing that holds it: the lease reaches
// every repository its installation holds, as every token this package mints does, so what
// keeps a reading lease from writing is that each permission of the set is a read, and that the
// set names nothing beside them. A permission the installation is granted none of is left out
// of the request rather than minted. Each permission is here for a reason of its own:
//
// contents:read is a clone, a fetch and a read of a file: a pod's repository is cloned and
// fetched with it, and a controller reads the code of each repository of its project through it.
// It stays a read: contents:read cannot push a commit, a branch or a tag.
//
// metadata:read is every read of the repository itself, such as its name, its default branch
// and its collaborators, and GitHub requires it of every token. It stays a read: metadata:read
// changes nothing of the repository.
//
// pull_requests:read is a pull request, its reviews and its comments: a controller reads the work
// of each repository of its project there. It stays a read: pull_requests:read cannot open,
// review, comment on or merge a pull request.
//
// issues:read is here because an issue is where a work's context is often written, and a
// controller reads the issues of every repository its project serves. Measured on 2026-11-10,
// and again on 2026-11-15, from roles of both Apps on a private repository: with no issues
// permission in the request, reading an issue answered 403 "Resource not accessible by
// integration" while the same token read that repository's contents and pull requests. It stays
// a read: issues:read cannot open, edit, close or comment on an issue.
//
// actions:read is here because reading a repository includes reading what its merges did: a
// deployment check of a merge reads the push run that carries it, that run's jobs and a job's
// log, and a controller that cannot read them cannot tell a deploy that applied from one that was
// skipped. Measured on 2026-11-04 against a private repository whose installation holds
// actions:write: unnarrowed, actions/runs and actions/runs/{id}/jobs answered 200 and
// actions/jobs/{id}/logs answered 302 to a signed URL that answered 200; narrowed to contents,
// pull requests and metadata, all three answered 403 "Resource not accessible by integration".
// The narrowing was the whole refusal, and no App or installation setting was missing. It stays a
// read: actions:read cannot cancel, re-run or dispatch a workflow, and reaches no Actions secret
// or variable, which are permissions of their own.
//
// checks:read is here because a controller reads what CI said of a head: the check runs
// of a commit, and the check runs in a pull request's check rollup. Measured on 2026-11-16 on a
// private repository: through a read lease, commits/<sha>/check-runs answered 403 and the pull
// request's statusCheckRollup answered FORBIDDEN, while the implement App's token without the
// narrowing read the same commit. It stays a read: checks:read cannot create or change a check
// run.
//
// statuses:read is here because a commit status is the other half of what CI says of a head: the
// commit statuses of a commit, and the statuses in a pull request's check rollup. A base branch
// can require a commit status before a merge. Measured on 2026-11-16: through a read lease on a
// private repository, commits/<sha>/status answered 403. It stays a read: statuses:read cannot
// create a commit status.
//
// attestations:read is here because a controller reads the attestations of an artifact, to check
// that an image was built from the default branch. Measured on 2026-11-17: through a read lease on
// a private repository, a read of its attestations answered 403 "Resource not accessible by
// integration". It stays a read: attestations:read cannot create or delete an attestation.
var readPermissions = map[string]string{
	"contents":      "read",
	"metadata":      "read",
	"pull_requests": "read",
	"issues":        "read",
	"actions":       "read",
	"checks":        "read",
	"statuses":      "read",
	"attestations":  "read",
}

// accessOrder ranks GitHub's three accesses, each holding every one below it: a permission
// granted at write is granted its read, and one granted at read is not granted its write. A
// value outside these three is no access this daemon knows how to ask for, and ranks below all
// of them, so narrowToGranted leaves that permission out rather than ask for a word of GitHub's
// it cannot order.
var accessOrder = map[string]int{"read": 1, "write": 2, "admin": 3}

// grantShortfall is one permission a role's set asks for that the installation is not granted at
// the access asked: the permission, the access the set asks for, and the access the installation
// is granted, empty where it is granted none. It carries no token, and neither does the record it
// is said in (sayShortfalls).
type grantShortfall struct {
	permission string
	asked      string
	granted    string
}

// granting is what the installation is granted of the permission, as the message says it.
func (s grantShortfall) granting() string {
	if s.granted == "" {
		return "no " + s.permission
	}
	return s.permission + ": " + s.granted
}

// minting is what the mint's request carries of the permission: the access granted where that is
// one GitHub mints at, and nothing where the installation is granted none of it.
func (s grantShortfall) minting() string {
	if accessOrder[s.granted] == 0 {
		return "without it"
	}
	return "with " + s.permission + ": " + s.granted
}

// narrowToGranted is asked cut down to granted, and every shortfall between the two in one order
// whatever order a map ranges in: a permission granted at the access asked or above is asked for
// as asked names it, one granted below is asked for at the access granted, and one granted none
// is left out. It copies, since the sets it cuts are shared by every mint.
func narrowToGranted(asked, granted map[string]string) (map[string]string, []grantShortfall) {
	narrowed := make(map[string]string, len(asked))
	var short []grantShortfall
	for _, permission := range slices.Sorted(maps.Keys(asked)) {
		access, held := asked[permission], granted[permission]
		switch {
		case accessOrder[held] == 0:
			short = append(short, grantShortfall{permission: permission, asked: access, granted: held})
		case accessOrder[held] < accessOrder[access]:
			narrowed[permission] = held
			short = append(short, grantShortfall{permission: permission, asked: access, granted: held})
		default:
			narrowed[permission] = access
		}
	}
	return narrowed, short
}

// shortfallMessage is the message of the warn record a Manager logs once per App role, owner and
// shortfall, when a mint asks for less than the role's set names because the installation is
// granted less: the lease is served, and what the token cannot do is said once rather than at
// every mint. It takes the role, the owner, what the installation is granted (granting), the
// permission and the access asked for, what the request carries of it (minting), and the
// permission and access to grant. It carries no token.
const shortfallMessage = "appauth: the %s App's installation on %s is granted %s, where its token asks for %s: %s, so the token is minted %s; grant the App %s: %s and approve it on that account"

// saidShortfall is the key one record of a shortfall is logged under: the App role, the owner
// lowercased as a scope's is, and what the installation is granted of the permission. A second
// mint of any scope of that role and owner logs nothing again, since the grant is the
// installation's and the record is about the installation.
type saidShortfall struct {
	role  AppRole
	owner string
	grantShortfall
}

// exchangeGranted is exchange over what the installation is granted: the request's permissions
// cut down to the access owner's account approved for the App (grantedPermissions,
// narrowToGranted), with a warn record for each one left out or lowered (sayShortfalls). A
// permission the installation is not granted is left out, where asking for it would make GitHub
// refuse the mint whole (a 422) and cost the role every other permission of its set, and the gh
// bundle that mint belongs to (GitHubCredential).
//
// A cut that leaves no permission at all refuses the mint instead: GitHub reads a request with
// no permissions as every permission the installation holds, so a body without them would mint
// wider than the role's set asks for rather than narrower. The refusal names the role, the
// owner and the set asked for (askedFor), and carries no token. A gh bundle leaves that owner's
// directory out over it, unless it is the bundle's own default (mintBundle).
//
// An installation its account suspended is refused by the read, before any token is asked of it
// (grantedPermissions, errInstallationSuspended), and a bundle leaves that owner's directory out
// over it in the same way.
//
// A 422 from the exchange after this cut is a grant that changed between the two calls
// (ErrNarrowingRefused): it stays the hard refusal it is.
func (m *Manager) exchangeGranted(ctx context.Context, jwt, installation string, request mintRequest) (Lease, error) {
	granted, err := m.grantedPermissions(ctx, jwt, installation)
	if err != nil {
		return Lease{}, err
	}
	permissions, short := narrowToGranted(request.permissions, granted)
	m.sayShortfalls(request.role, request.owner, short)
	if len(permissions) == 0 {
		return Lease{}, fmt.Errorf("the %s App's installation on %s is granted none of the permissions its token asks for (%s), and a token request naming no permission holds every permission the installation has: %w",
			request.role, request.owner, askedFor(request.permissions), errGrantedNone)
	}
	return m.exchange(ctx, jwt, installation, permissions)
}

// errGrantedNone is the refusal exchangeGranted makes of an installation granted none of what the
// request asks for: an answer about the installation, read from what its account approved, which
// a bundle leaves that owner's directory out over (credential.go's leaveOutReason).
var errGrantedNone = errors.New("the installation is granted none of the permissions asked for")

// errInstallationSuspended is the refusal grantedPermissions makes of an installation its account
// suspended: an answer about the installation, read from the installation's own suspended_at,
// which a bundle leaves that owner's directory out over (credential.go's leaveOutReason), where
// a rate limit or GitHub's own trouble is not one.
var errInstallationSuspended = errors.New("the installation is suspended")

// grantedPermissions is what owner's account approved for the App's installation: GET
// /app/installations/{id} under the App's JWT answers the installation, whose permissions object
// maps each permission it is granted to the access it is granted at ("read", "write", "admin"),
// and names none of the ones it is granted nothing of. GitHub's trouble is transient, as every
// other read here is, and a 404 is the installation gone (errInstallationGone), which mint
// answers by looking for the installation once more.
//
// The same answer says whether the account suspended the installation: its suspended_at is a
// time for one that is and null for every other, the field discovery leaves a suspended
// installation out of the owners by (discoverInstallations). A suspended installation is refused
// here (errInstallationSuspended) and no token is asked of it. GitHub answers that token request
// 403, the status of its secondary rate limit too, so the exchange could tell the two apart by
// the words of GitHub's message alone, and a message GitHub rewords would turn a suspended
// owner into a failure of every role's bundle. A field has no words to change. This read is
// what finds an installation suspended after discovery listed it, and one the configuration
// names, which discovery never lists.
//
// It is read before every exchange and never cached. A mint happens about three times an hour
// per scope, so the read costs little, and an account that approves a grant has it asked for at
// the next mint rather than at the next daemon restart, which a cache no approval invalidates
// would make it wait for.
func (m *Manager) grantedPermissions(ctx context.Context, jwt, installation string) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/app/installations/"+url.PathEscape(installation), nil)
	if err != nil {
		return nil, err
	}
	m.appHeaders(request, jwt)
	response, err := m.client.Do(request)
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("GitHub App installation permissions read: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := refusalBody(response)
		failed := fmt.Errorf("GitHub App installation permissions read failed (%d): %s", response.StatusCode, githubrest.BodyText(body, jwt))
		if response.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %w", errInstallationGone, failed)
		}
		return nil, transient(response, body, failed)
	}
	body, err := readBody(response, "GitHub App installation permissions read", responseBound)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Permissions map[string]string `json:"permissions"`
		SuspendedAt *string           `json:"suspended_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("GitHub App installation permissions read returned an invalid response")
	}
	if payload.SuspendedAt != nil {
		return nil, fmt.Errorf("%w: GitHub answers installation %s with suspended_at %s", errInstallationSuspended, installation, *payload.SuspendedAt)
	}
	return payload.Permissions, nil
}

// sayShortfalls logs shortfallMessage at warn for each of short the first time a mint of role for
// owner meets it, and nothing on any mint after it: the grant is the installation's and the
// record is about the installation, which the owner names. Beside the message the record carries
// what it says as fields, so a reader filtering the daemon's log finds it by them: the App, the
// owner, the permission, the access the set asks for and the access the installation is granted,
// empty where it is granted none.
func (m *Manager) sayShortfalls(role AppRole, owner string, short []grantShortfall) {
	for _, shortfall := range short {
		key := saidShortfall{role: role, owner: strings.ToLower(strings.TrimSpace(owner)), grantShortfall: shortfall}
		m.mu.Lock()
		said := m.saidShortfalls[key]
		m.saidShortfalls[key] = true
		m.mu.Unlock()
		if said {
			continue
		}
		m.log.Warn(fmt.Sprintf(shortfallMessage, role, owner, shortfall.granting(),
			shortfall.permission, shortfall.asked, shortfall.minting(), shortfall.permission, shortfall.asked),
			"app", string(role), "owner", owner, "permission", shortfall.permission,
			"asked", shortfall.asked, "granted", shortfall.granted)
	}
}
