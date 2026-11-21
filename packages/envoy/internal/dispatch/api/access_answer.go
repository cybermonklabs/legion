package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// The membership answer (K3) is what the secrets broker reads to decide a project's secrets and a
// token's list: one project's owner, visibility and Slack channels, and whether one person sees
// it, one person's member kind and whether Dispatch holds them (personStanding), which the
// broker's hold gate reads, and every private project's Slack channels. It names no
// repositories. It is plain data read outside any transaction. Only a service the deployment names
// in DISPATCH_ACCESS_ACCOUNTS reads it (accessAccountOf): the secrets broker, with a read token it
// issued to itself from its own read entry, bound to its DPoP key, whose service is its Kubernetes
// account, and, where the deployment turns on Dispatch's own check of projected tokens
// (DISPATCH_OIDC_*), a pod presenting such a token of a listed account. Before anything is read, a
// caller with no credential, or one whose credential does not verify, is refused as every route
// refuses it (401 NO_IDENTITY for none: optionalActor's error, writeAuthenticationError); another
// service's read token is refused 403 READ_ONLY_TOKEN by the read-only gate further out
// (refuseReadOnlyToken); and every other caller, a person included, is 403 ACCESS_ACCOUNT_REQUIRED
// (the read class accessAccount, which withinReadScope holds the three routes to). A deployment
// that names no such account answers every caller so.
//
// A question that names a person may also say which Google account the credential being checked
// records, and the answer then says whether that credential's account holds the person's address
// (holder, accountHolder): ?account_issuer=&account_subject=, the broker's introspection's own two,
// or ?account=none, each with ?credential_created_at=, when the credential was made at the broker
// (Unix seconds, the introspection's own field), where it has one. A credential of an account
// Dispatch no longer records for the address acts for nobody, from the moment another account's
// sign-in to Dispatch records itself; one recording no account is held to when it was made,
// against when Dispatch recorded the address's account (people.sub_recorded_at), with
// brokerCredentialMargin between the two clocks; with nothing revoked and nothing ended for anyone
// else. A question that says nothing of the credential gets the answer it always got: no holder,
// and visible as the project's rule reads it. Every broker before this one asks that way, and a
// broker asks it for an operator entry's token, which no account made (brokerPersonAdmitted).

// accessAccountOf answers the service a request's verified credential names as one that may read
// the membership answer, and false for every other credential. Two credentials name one. The
// secrets broker's read token (model.Credential.ReadOnly), which the broker-token check marks only
// for the scope read beside the project scope none, a service and no person and no project
// (brokertoken.Caller.ReadOnly), and whose service is the account its read entry names; it is
// checked here again for no person and no project, so a token naming a person never reads the
// answer it exists to gate. And a pod's projected token, which Dispatch verifies itself where
// DISPATCH_OIDC_* turns that check on: a service and nothing else. Every other broker-issued token
// names a fleet as its service and proved a session, not a pod's account, so it is not one.
func accessAccountOf(actor model.Actor) (string, bool) {
	switch {
	case actor.Service == nil || actor.Credential == nil:
		return "", false
	case actor.Credential.ReadOnly:
		if actor.Owner != nil || len(actor.Credential.Projects) > 0 {
			return "", false
		}
		return *actor.Service, true
	case actor.Kind != "" || actor.Credential.Session != "" || actor.Credential.TokenID != "":
		return "", false
	}
	return *actor.Service, true
}

// isAccessAccount reports whether actor's credential names a service DISPATCH_ACCESS_ACCOUNTS
// lists (accessAccountOf): the one rule both requireAccessAccount and the read-only gate
// (refuseReadOnlyToken) read.
func (s *server) isAccessAccount(actor model.Actor) bool {
	subject, ok := accessAccountOf(actor)
	return ok && slices.Contains(s.deps.AccessAccounts, subject)
}

// requireAccessAccount admits a request only from a service DISPATCH_ACCESS_ACCOUNTS names:
// withinReadScope's check of every row of the read class accessAccount.
func (s *server) requireAccessAccount(w http.ResponseWriter, r *http.Request) bool {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return false
	}
	if s.isAccessAccount(actor) {
		return true
	}
	writeError(w, "ACCESS_ACCOUNT_REQUIRED", http.StatusForbidden,
		"the membership answer is read only by a service account this deployment names in DISPATCH_ACCESS_ACCOUNTS")
	return false
}

// accessQuestion is a membership question's person and the account the credential being checked
// records: issuer and subject, empty for none, and when the credential was made (zero for a time
// the question did not give). describes is whether the question says which account that is, an
// account or account=none; one that does not asks about the person alone.
type accessQuestion struct {
	person          string
	issuer, subject string
	createdAt       time.Time
	describes       bool
}

// readAccessQuestion reads a membership question's query: the person, which the person route
// requires, and the credential's account, both of account_issuer and account_subject, or
// account=none, or neither (the person alone), and credential_created_at, Unix seconds, only
// beside one of those two; each only with a person, since it asks whether that credential's
// account holds the person's address. A query breaking any rule is 400 INVALID_QUERY.
func readAccessQuestion(w http.ResponseWriter, r *http.Request, personRequired bool) (accessQuestion, bool) {
	query := r.URL.Query()
	question := accessQuestion{
		person:  strings.TrimSpace(query.Get("person")),
		issuer:  strings.TrimSpace(query.Get("account_issuer")),
		subject: strings.TrimSpace(query.Get("account_subject")),
	}
	account := strings.TrimSpace(query.Get("account"))
	created := strings.TrimSpace(query.Get("credential_created_at"))
	seconds, createdErr := strconv.ParseInt(created, 10, 64)
	if created != "" && createdErr == nil && seconds > 0 {
		question.createdAt = time.Unix(seconds, 0).UTC()
	}
	question.describes = question.issuer != "" || account == "none"
	switch {
	case personRequired && question.person == "":
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "person is required: the email whose member kind to read")
	case (question.issuer == "") != (question.subject == ""):
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "account_issuer and account_subject name one account together: give both or neither")
	case account != "" && (account != "none" || question.issuer != ""):
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "account is none alone, for a credential that records no account; one that records an account gives account_issuer and account_subject")
	case created != "" && question.createdAt.IsZero():
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "credential_created_at is when the credential was made, in whole Unix seconds")
	case question.person == "" && (question.issuer != "" || account != "" || created != ""):
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "a credential is asked about as the holder of a person's address, so person is required with account_issuer, account_subject, account and credential_created_at")
	case created != "" && !question.describes:
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "credential_created_at describes the credential the question names: give it with account_issuer and account_subject, or with account=none")
	default:
		return question, true
	}
	return accessQuestion{}, false
}

// accountHolder answers, for a question naming a person and describing the credential being
// checked, whether that credential holds the person's address. On a deployment whose sign-in
// records Google accounts (Deps.SignInIssuer set) and for a person Dispatch records an account for
// (people.sub, written by the sign-in alone, store.PgPeopleStore.SignInSubject), it is false for a
// credential recording another account, or one of another issuer (compared as oidc.CanonicalIssuer
// spells it), whenever made, as the admission refuses one (brokerAccountReplaced), and for a
// credential recording none that was made before Dispatch recorded the address's account
// (people.sub_recorded_at), less than brokerCredentialMargin after it, or at a time the question
// did not give: the two times come from two databases' clocks, so a skew refuses. It is true
// otherwise: no account recorded for the person (no row included), no recording time to compare a
// credential recording none with (a row from before the column), or a deployment whose sign-in
// records no account. Whether an account is recorded is read as the admission reads it, `sub is
// not null`; the database refuses an empty sub (migration 20261118041916), so no row reads as
// recorded to one and as unrecorded to the other. It is nil for a question naming no person, and
// for one that does not say which account the credential records, which the answer then leaves
// out, as it always did.
func (s *server) accountHolder(ctx context.Context, q access.Querier, question accessQuestion) (*bool, error) {
	if question.person == "" || !question.describes {
		return nil, nil
	}
	holder := true
	if s.deps.SignInIssuer == "" {
		return &holder, nil
	}
	var recorded, recordedAt, madeAfter bool
	var sub string
	err := q.QueryRow(ctx, `select sub is not null, coalesce(sub, ''), sub_recorded_at is not null,
			coalesce($2::timestamptz >= sub_recorded_at + `+brokerCredentialMarginSQL+`, false)
		from people where email = $1`, model.CanonicalLogin(question.person), timeOrNull(question.createdAt)).Scan(&recorded, &sub, &recordedAt, &madeAfter)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the Google account recorded for %s: %w", model.CanonicalLogin(question.person), err)
	}
	switch {
	case !recorded:
	case question.subject != "":
		holder = oidc.CanonicalIssuer(question.issuer) == s.deps.SignInIssuer && question.subject == sub
	default:
		holder = !recordedAt || madeAfter
	}
	return &holder, nil
}

// getAccessProject answers project {key}'s membership answer for ?person=, which may be left out
// to read the project alone (member_kind none, and visible whether the project is public). A
// question whose account does not hold the person's address answers visible false, with holder
// false.
func (s *server) getAccessProject(w http.ResponseWriter, r *http.Request) {
	question, ok := readAccessQuestion(w, r, false)
	if !ok {
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := access.Answer(r.Context(), pool, r.PathValue("key"), question.person)
	if err != nil {
		s.writeAccessError(w, err)
		return
	}
	if answer.Holder, err = s.accountHolder(r.Context(), pool, question); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if answer.Holder != nil && !*answer.Holder {
		answer.Visible = false
	}
	WriteJSON(w, http.StatusOK, answer)
}

// getAccessPerson answers ?person='s member kind, whether Dispatch holds them (personStanding), and
// holder for a question naming an account.
func (s *server) getAccessPerson(w http.ResponseWriter, r *http.Request) {
	question, ok := readAccessQuestion(w, r, true)
	if !ok {
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := access.PersonAnswer(r.Context(), pool, question.person)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.personStanding(r.Context(), &answer)
	if answer.Holder, err = s.accountHolder(r.Context(), pool, question); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// privateProjectsAnswer is GET /api/v1/access/private-projects' body. Projects is always an
// array, [] when no project is private (access.PrivateProjects), and never left out.
type privateProjectsAnswer struct {
	Projects []model.PrivateProjectChannels `json:"projects" nullable:"false"`
}

// getAccessPrivateProjects answers every private project's Slack channels and when each list last
// changed, by key, read live on each request. Its read class, accessAccount, has withinReadScope
// check the caller before it runs (requireAccessAccount), once, as for the two other answers.
func (s *server) getAccessPrivateProjects(w http.ResponseWriter, r *http.Request) {
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	projects, err := access.PrivateProjects(r.Context(), pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, privateProjectsAnswer{Projects: projects})
}

// personStanding fills in whether Dispatch holds answer's person (Deps.Standing) and the time the
// answer rests on. Where no Google rule signs people in (Standing nil) nobody is held, and as_of
// stays the database's now() the member kind was read with. A Standing that fails is no answer: the
// answer leaves held out, so the broker reads it as Dispatch not asked for the hold and still reads
// its member_kind, keeps that now() as as_of, and logs the failure; the route still answers 200.
// Otherwise it carries Standing's held and the time of the read it rests on, and, under IAP
// identity, the deadline of a not-held answer as not_held_until: as_of itself for an unavailable one
// (identity.Standing.Unavailable), which the broker refuses as it refuses a deadline passed.
func (s *server) personStanding(ctx context.Context, answer *model.PersonAnswer) {
	if s.deps.Standing == nil {
		answer.Held = new(bool)
		return
	}
	standing, err := s.deps.Standing.Standing(ctx, answer.Person)
	if err != nil {
		slog.Warn("dispatch: the person answer leaves held out: whether Dispatch holds the person could not be read", "error", err)
		return
	}
	answer.Held, answer.AsOf = &standing.Held, standing.AsOf
	if !standing.Held && !standing.NotHeldUntil.IsZero() {
		answer.NotHeldUntil = &standing.NotHeldUntil
	}
}
