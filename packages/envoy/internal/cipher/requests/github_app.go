// packages/envoy/internal/cipher/requests/github_app.go
// The github_app field an agent-secrets of a release before --target sends with --github-owner,
// --github-repo, --github-read-only and --github-token: the narrowing of a release of a stored
// github-app secret, which legion-envoy-v33.7.0 applied and the profiles release refused. Until
// every Legion project runs a client with --target, the broker answers it again, on both routes,
// from the profiles file's own rules: the request is served as the profile of that secret the file
// grants the caller at once whose scope holds the narrowing (gitHubAppProfile), narrowed to the
// account and repositories the body names, and its release is minted in the installations form
// that client reads and handed over outside the airlock, as v33.7.0 handed it over
// (narrowedServed). BROKER_GITHUB_APP_FIELD=off (Machine.GitHubAppFieldOff) refuses the field as
// before.
package requests

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// ErrGitHubAppFieldRefused is the refusal of a github_app field the broker does not answer: every
// one with BROKER_GITHUB_APP_FIELD=off, and one it cannot serve, whose refusal names why after
// this text (gitHubAppFieldRefused). The API answers it 400 GITHUB_APP_FIELD_REMOVED.
var ErrGitHubAppFieldRefused = errors.New("github_app was removed: name a profile and narrow it with --target ACCOUNT[/REPOSITORY]")

// gitHubAppFieldRefused is ErrGitHubAppFieldRefused naming why this field was not answered.
func gitHubAppFieldRefused(why string) error {
	return fmt.Errorf("%w (%s)", ErrGitHubAppFieldRefused, why)
}

// GitHubAppFieldAnsweredMessage is logged, at INFO, for each request and each release that
// answered a github_app field, followed by at=<request or release>, name=<the stored secret the
// client asked for>, profile=<the profile it was served as>, request= or grant=, state= for a
// request, enrollment=, kind=, person=, service=, owner=, repositories= and read_only=: the count
// of the clients that still send the field, read before BROKER_GITHUB_APP_FIELD is set to off.
const GitHubAppFieldAnsweredMessage = "broker: answered the github_app field of an earlier agent-secrets"

// GitHubAppNarrowing is a github_app field as a released client sends it: the one account whose
// installation it asks for, lowercased, the repositories of that account it names, sorted and each
// once (nil for every repository the installation reaches), and whether it asks to read alone.
type GitHubAppNarrowing struct {
	Owner        string
	Repositories []string
	ReadOnly     bool
}

// target is the narrowing as a request's target, the form a profile's scope is cut by.
func (n GitHubAppNarrowing) target() record.Target {
	return record.Target{Account: n.Owner, Repositories: n.Repositories}
}

// readOnlyPermissions is the read set legion-envoy-v33.7.0 asked GitHub for (its secrets'
// readOnlyPermissions), so a command that reads a repository's contents, its pull requests, the
// Actions runs its merges produced and what CI said of a head works as it did. A read-only
// narrowing asks for each of them at read where the profile it is served as holds it, and leaves
// out every one the profile does not, as v33.7.0 left out what the installation did not hold.
var readOnlyPermissions = []string{"actions", "checks", "contents", "metadata", "pull_requests", "statuses"}

// GitHubAppNarrowing reads raw, a body's github_app field, as the narrowing a released client
// sends: {"owner", "repositories", "read_only"}, a GitHub login, from 1 to
// githubname.MaxNarrowedRepositories repository names where it names any, and nothing else. Every
// refusal is ErrGitHubAppFieldRefused, which with BROKER_GITHUB_APP_FIELD=off is the whole answer.
func (m *Machine) GitHubAppNarrowing(raw json.RawMessage) (GitHubAppNarrowing, error) {
	if m.GitHubAppFieldOff {
		return GitHubAppNarrowing{}, ErrGitHubAppFieldRefused
	}
	var sent struct {
		Owner        string   `json:"owner"`
		Repositories []string `json:"repositories"`
		ReadOnly     bool     `json:"read_only"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sent); err != nil {
		return GitHubAppNarrowing{}, gitHubAppFieldRefused("github_app does not read as a narrowing: " + err.Error())
	}
	if !githubname.ValidLogin(sent.Owner) {
		return GitHubAppNarrowing{}, gitHubAppFieldRefused("github_app.owner names no GitHub login")
	}
	if sent.Repositories != nil && (len(sent.Repositories) == 0 || len(sent.Repositories) > githubname.MaxNarrowedRepositories) {
		return GitHubAppNarrowing{}, gitHubAppFieldRefused(fmt.Sprintf("github_app.repositories names from 1 to %d repositories", githubname.MaxNarrowedRepositories))
	}
	for _, repository := range sent.Repositories {
		if !githubname.ValidRepositoryName(repository) {
			return GitHubAppNarrowing{}, gitHubAppFieldRefused("github_app.repositories names a repository GitHub cannot hold")
		}
	}
	repositories := slices.Compact(slices.Sorted(slices.Values(sent.Repositories)))
	if sent.Repositories == nil {
		repositories = nil
	}
	return GitHubAppNarrowing{Owner: strings.ToLower(sent.Owner), Repositories: repositories, ReadOnly: sent.ReadOnly}, nil
}

// CreateNarrowed is Create for a body that carries a github_app field, narrowing: the request is
// for the one stored github-app secret its signed object names, with no locations, and it is
// served as the profile of that secret gitHubAppProfile chooses, with narrowing's account and
// repositories as its target. A request that names more than one secret, signs locations, or
// names a profile of another name than its source is refused ErrGitHubAppFieldRefused before
// anything is written. A request the profile would put to a person or the judge is refused the
// same way, since the record a person decides holds the signed object, which names neither the
// profile nor the target.
func (m *Machine) CreateNarrowed(ctx context.Context, enrollmentID, compactRequest, sessionID string, narrowing GitHubAppNarrowing) (Request, error) {
	if m.GitHubAppFieldOff {
		return Request{}, ErrGitHubAppFieldRefused
	}
	return m.create(ctx, enrollmentID, compactRequest, sessionID, &narrowing)
}

// checkNarrowedObject refuses a request object a github_app field cannot narrow: one naming other
// than one secret, or signing locations of its own.
func checkNarrowedObject(obj record.RequestObject, names []string) error {
	if len(names) != 1 || len(obj.Details) != 1 {
		return gitHubAppFieldRefused(fmt.Sprintf("the field narrows a request of one secret, and this one names %d", len(obj.Details)))
	}
	if len(obj.Details[0].Locations) > 0 {
		return gitHubAppFieldRefused("the request signs a target of its own")
	}
	return nil
}

// gitHubAppFieldServes reports whether the old body is answered for this caller and this stored
// secret at all, before any profile is chosen. The profiles file alone would be wider than the
// broker the field comes from: a profile with no requesters list is granted to every agent-tier
// caller, and that caller would be handed a real token outside the airlock, where its own
// --target request gets a placeholder behind the airlock or nothing. So the field is answered
// only for the callers the deployment names in BROKER_GITHUB_APP_FIELD_REQUESTERS
// (Machine.GitHubAppFieldRequesters: a registered service's name or a person's login), and for a
// person asking for their own App, which owner, the stored secret's owner tag as its profiles
// carry it, names. Unset or empty, the list answers no service and no person but that one.
func (m *Machine) gitHubAppFieldServes(requester policy.Requester, owner string) bool {
	if owner != "" && requester.Operator != "" && owner == record.CanonicalLogin(requester.Operator) {
		return true
	}
	for entry := range strings.FieldsSeq(strings.ReplaceAll(m.GitHubAppFieldRequesters, ",", " ")) {
		if requester.Service != "" && entry == requester.Service ||
			requester.Operator != "" && record.CanonicalLogin(entry) == record.CanonicalLogin(requester.Operator) {
			return true
		}
	}
	return false
}

// gitHubAppFieldSecret reports whether name, a stored github-app secret, is one the deployment
// lets the field hand over outside the airlock (BROKER_GITHUB_APP_FIELD_SECRETS,
// Machine.GitHubAppFieldSecrets), unset naming none. legion-envoy-v33.7.0 refused a narrowed
// release of a name its airlock held (its ErrNarrowingBehindAirlock), and this answer hands the
// token past the proxy, so the Apps that may leave it are named one by one rather than taken
// from the requester list, which bounds who asks and not which App.
func (m *Machine) gitHubAppFieldSecret(name string) bool {
	for entry := range strings.FieldsSeq(strings.ReplaceAll(m.GitHubAppFieldSecrets, ",", " ")) {
		if entry == name {
			return true
		}
	}
	return false
}

// gitHubAppServable reports whether served, a name of the policy snapshot set, is one the old
// body may be served as: a github-app profile of source, outside the airlock's reach of a
// private project's channels (policy.ReasonProjectChannelsNeedAirlock holds such a name to the
// proxy, which this answer hands the token past). gitHubAppCandidates is its one caller, and
// Create's choice and every release read it through checkGitHubAppField, so no route serves
// what another refuses.
func gitHubAppServable(served policy.Secret, source string) bool {
	return served.Profile != nil && served.Profile.GitHub != nil && served.Profile.Source == source &&
		served.Type() == policy.TypeGitHubApp && served.ProjectChannels == ""
}

// gitHubAppCandidates is every profile of the stored secret name the old body may be served as,
// in the profiles file's order with name's own profile first, and the owner of that secret as its
// profiles carry it (policy.Secret.Owner, the source's owner tag). A secret the profiles file
// serves only through profiles of other names holds no entry of its own name
// (policy.Profiles.apply), so the owner is read from the first candidate rather than from that
// entry.
func gitHubAppCandidates(set *policy.Set, name string) (candidates []string, owner string) {
	for _, candidate := range append([]string{name}, set.ProfilesOf(name)...) {
		served, ok := set.Secrets[candidate]
		if !ok || !gitHubAppServable(served, name) {
			continue
		}
		if owner == "" {
			owner = served.Owner
		}
		candidates = append(candidates, candidate)
	}
	return candidates, owner
}

// checkGitHubAppField is the one gate the request's choice (gitHubAppProfile) and every narrowed
// release (narrowedRelease) ask of a stored secret name and a caller: the deployment lets that
// App leave the airlock (gitHubAppFieldSecret), the profiles file serves it as a github-app
// profile (gitHubAppCandidates, over gitHubAppServable), and the caller is
// one the field is answered for. It answers the candidates the choice reads, and the refusal
// (ErrGitHubAppFieldRefused) both routes answer, so neither admits what the other refuses.
func (m *Machine) checkGitHubAppField(set *policy.Set, name string, requester policy.Requester) ([]string, error) {
	if !m.gitHubAppFieldSecret(name) {
		return nil, gitHubAppFieldRefused("BROKER_GITHUB_APP_FIELD_SECRETS does not name " + name + ", so no release of it is handed over outside the airlock")
	}
	candidates, owner := gitHubAppCandidates(set, name)
	if len(candidates) == 0 {
		return nil, gitHubAppFieldRefused(name + " is no github-app profile the field may narrow")
	}
	if !m.gitHubAppFieldServes(requester, owner) {
		return nil, gitHubAppFieldRefused("this requester is served no github_app field: BROKER_GITHUB_APP_FIELD_REQUESTERS does not name it and it does not own " + name)
	}
	return candidates, nil
}

// gitHubAppProfile is the name a request for the stored secret name carrying narrowing is served
// as: of the profiles the one gate answers (checkGitHubAppField), in the profiles file's order
// with name's own entry first, one the policy grants requester at once and whose scope holds
// narrowing's target. Where none is, and where the gate refuses, the request is refused
// (ErrGitHubAppFieldRefused): there is no wider path. A name served as a profile of another
// source is refused too: a released client names the stored secret.
func (m *Machine) gitHubAppProfile(ctx context.Context, enr enrollmentRow, set *policy.Set, facts projectFacts, name string, narrowing GitHubAppNarrowing, reason string) (string, error) {
	if served, ok := set.Secrets[name]; ok && served.Profile != nil && served.Profile.Source != name {
		return "", gitHubAppFieldRefused(name + " is a profile of " + served.Profile.Source + ", and the field narrows a stored github-app secret")
	}
	requester, err := m.requester(ctx, m.Store.Pool, enr, facts)
	if err != nil {
		return "", err
	}
	candidates, err := m.checkGitHubAppField(set, name, requester)
	if err != nil {
		return "", err
	}
	best, bestWrites, bestAll := "", -1, -1
	for _, candidate := range candidates {
		served := set.Secrets[candidate]
		if e, err := m.evaluate(set, []string{candidate}, requester, reason); err != nil || e.state != "granted" {
			continue
		}
		scope, err := gitHubScope(candidate, narrowedServed(served, narrowing), narrowing.target())
		if err != nil {
			continue
		}
		writes, all := 0, 0
		for _, level := range scope.Permissions {
			if githubname.LevelRank(level) > githubname.LevelRank("read") {
				writes++
			}
			all += githubname.LevelRank(level)
		}
		// The score is the count of permissions above read, then the sum of every permission's
		// level (githubname.LevelRank). Only a strictly higher score replaces the best so far,
		// and the stored secret's own name is read first, so it wins a tie before every profile
		// of it and each profile before the ones after it in the file.
		if writes > bestWrites || writes == bestWrites && all > bestAll {
			best, bestWrites, bestAll = candidate, writes, all
		}
	}
	if best == "" {
		return "", gitHubAppFieldRefused("no profile of " + name + " is granted to this requester at once for the account and repositories the field names")
	}
	return best, nil
}

// narrowedServed is served as a release narrowed to narrowing hands it over: its GitHub profile
// minted in the installations form a released client reads, cut to readOnlyPermissions for a
// read-only narrowing, and with no proxy block, so the token is handed over as v33.7.0 handed it.
// The target is the request's, which gitHubScope cuts the scope to. served is not changed.
func narrowedServed(served policy.Secret, narrowing GitHubAppNarrowing) policy.Secret {
	if served.Profile == nil || served.Profile.GitHub == nil {
		return served
	}
	profile := *served.Profile
	block := *profile.GitHub
	block.Value = githubname.ValueInstallations
	if narrowing.ReadOnly {
		block.Permissions = map[string]string{}
		for _, permission := range readOnlyPermissions {
			if githubname.LevelRank(served.Profile.GitHub.Permissions[permission]) > 0 {
				block.Permissions[permission] = "read"
			}
		}
	} else {
		block.Permissions = maps.Clone(block.Permissions)
	}
	profile.GitHub, profile.Proxy = &block, nil
	served.Profile = &profile
	return served
}

// ValuesNarrowed is Values for a release body that carries a github_app field, narrowing: the
// grant's one name, a github-app profile whose recorded target is narrowing's, is minted as
// narrowedServed hands it over and answered under the name of its source, the stored secret the
// client asked for. A grant of another shape, or of another target, is refused
// ErrGitHubAppFieldRefused and releases nothing.
func (m *Machine) ValuesNarrowed(ctx context.Context, grantID, enrollmentID string, call ReleaseCall, narrowing GitHubAppNarrowing) (Release, error) {
	if m.GitHubAppFieldOff {
		return Release{}, ErrGitHubAppFieldRefused
	}
	return m.values(ctx, grantID, enrollmentID, call, &narrowing)
}

// narrowedRelease is every check a release carrying narrowing makes before anything is minted,
// and the name its value is handed over as: the stored secret its profile is of. The grant must
// hold one name; that name must be a github-app profile the field may be served as
// (gitHubAppServable, the check Create's choice makes); the grant must have been made from a
// github_app field of the same read_only (requestedSecret.gitHubAppField), so no --target grant,
// and no grant a person approved for another form, is released past the airlock by adding the
// field to a release body; the one gate (checkGitHubAppField, which reads the deployment's list
// of Apps, the profiles of the stored secret and the requester list) must answer it today;
// and today's policy must still grant that profile to this requester at once, so a release checks
// what Create checked and never less.
func (m *Machine) narrowedRelease(ctx context.Context, checked grantRelease, narrowing GitHubAppNarrowing) (string, error) {
	if len(checked.granted) != 1 {
		return "", gitHubAppFieldRefused("the field narrows a grant of one secret")
	}
	g := checked.granted[0]
	served := checked.set.Secrets[g.name]
	source := g.name
	if served.Profile != nil {
		source = served.Profile.Source
	}
	if !g.gitHubAppField || g.gitHubAppReadOnly == nil {
		return "", gitHubAppFieldRefused("this grant was not made from a github_app field, so its release is the profile's own: ask again with the field, or release it without one")
	}
	if *g.gitHubAppReadOnly != narrowing.ReadOnly {
		return "", gitHubAppFieldRefused("this grant was made read_only=" + strconv.FormatBool(*g.gitHubAppReadOnly) + ", and the release asks for read_only=" + strconv.FormatBool(narrowing.ReadOnly))
	}
	if !g.target.Equal(narrowing.target()) {
		return "", gitHubAppFieldRefused("the grant is narrowed to " + cmp.Or(g.target.String(), "no target") + ", not to " + narrowing.target().String())
	}
	requester, err := m.requester(ctx, m.Store.Pool, checked.enr, checked.facts)
	if err != nil {
		return "", err
	}
	candidates, err := m.checkGitHubAppField(checked.set, source, requester)
	if err != nil {
		return "", err
	}
	if !slices.Contains(candidates, g.name) {
		return "", gitHubAppFieldRefused(g.name + " is no github-app profile the field may narrow")
	}
	if e, err := m.evaluate(checked.set, []string{g.name}, requester, checked.reason); err != nil {
		return "", err
	} else if e.state != "granted" {
		return "", gitHubAppFieldRefused(g.name + " is no longer granted to this requester at once")
	}
	return source, nil
}

// logGitHubAppAnswer writes GitHubAppFieldAnsweredMessage for one answer at at, "request" or
// "release", of name served as profile to enr, with ids, the request's or the grant's fields.
func logGitHubAppAnswer(ctx context.Context, at, name, profile string, enr enrollmentRow, narrowing GitHubAppNarrowing, ids ...any) {
	attrs := append([]any{"at", at, "name", name, "profile", profile}, ids...)
	attrs = append(attrs, "enrollment", enr.ID, "kind", enr.Kind, "person", deref(enr.Operator), "service", deref(enr.Service),
		"owner", narrowing.Owner, "repositories", narrowing.Repositories, "read_only", narrowing.ReadOnly)
	answer.FromContext(ctx).NoteSecrets(name)
	slog.Info(GitHubAppFieldAnsweredMessage, attrs...)
}
