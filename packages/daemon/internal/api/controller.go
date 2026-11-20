package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ControllerStore is the project's controller record: the capability `legion controller start`
// was last issued, and the session that registered with it (`store.Store`).
type ControllerStore interface {
	MintControllerThen(ctx context.Context, project string, capabilityHash []byte, onCommit func(generation uint64)) (uint64, error)
	Controller(ctx context.Context, project string) (controller.Record, bool, error)
	RegisterController(ctx context.Context, project string, generation uint64, session string, secretHash []byte, at time.Time, pluginContract int) (bool, error)
}

// ControllerSecretRequest is `POST /legion/v1/controller/secret`'s body: the daemon API contract
// `legion controller start` held the controller's plugin to before it asked (its own
// DaemonAPIVersion, which its probe found the plugin's manifest declares). The daemon compares it
// before it mints, since the mint revokes the incumbent controller.
type ControllerSecretRequest struct {
	PluginContract int `json:"pluginContract"`
}

// ControllerSecretResponse is `POST /legion/v1/controller/secret`'s answer: the capability the
// controller's Oh My Pi registers with, and the design-gate steps the project gates
// (`gates.design`), in the order it gates them, which `legion controller start` tells the
// controller so its take comment promises a design approval only where the deployment gates a
// step. The list is empty for a deployment that gates none and is never absent: `legion
// controller start` refuses an answer that omits it, which is a daemon from before contract 55.
// `legion controller start` reads it; the plugin never does.
type ControllerSecretResponse struct {
	Secret string `json:"secret"`
	// DesignGateSteps is the deployment's list, never nil, so a deployment that gates no step
	// answers `[]` and only a daemon that does not know the field answers nothing at all.
	DesignGateSteps []record.DesignStep `json:"designGateSteps"`
}

// ControllerCredentialRequest is `POST /legion/v1/controller/github-credential`'s body: the
// current controller capability, the same secret `legion controller start` holds from
// `ControllerSecretResponse`, and Default, the owner whose directory the controller's
// GH_CONFIG_DIR names once a first answer named it; empty on that first fetch.
type ControllerCredentialRequest struct {
	Secret  string `json:"secret"`
	Default string `json:"default,omitempty"`
}

// ControllerCredentialResponse is `POST /legion/v1/controller/github-credential`'s answer: the
// review App's read-only token for every owner that App is installed on, and for an owner a
// configured rename moved a repository away from (appauth.ControllerCredential), each one
// rendered as the two
// files gh reads (`ghconfig.Rendered`: Hosts for hosts.yml, Config for config.yml) and keyed by
// lowercased owner; Default, the owner whose directory the controller's GH_CONFIG_DIR names (the
// request's, or the first owner, sorted); the App name; the soonest of the tokens' expiries;
// Renames, the configured pairs the bundle routes a moved repository by, each `<owner>/<name>`
// as GitHub spells it, which a project configuring none answers without; and Installed, the
// owner accounts the review App is installed on.
// `legion controller start` writes them under `<state_dir>/gh`, the owners' two files each in a
// directory of its own and the pairs in the root's one file (ghconfig.WriteBundle), and
// refreshes them every minute; the plugin never reads this route or that directory.
type ControllerCredentialResponse struct {
	Owners    map[string]ControllerOwnerCredential `json:"owners"`
	Default   string                               `json:"default"`
	App       string                               `json:"app"`
	ExpiresAt string                               `json:"expiresAt"`
	Renames   []ControllerRename                   `json:"renames,omitempty"`
	// Installed is the owner accounts the review App is installed on, lowercased and sorted,
	// as the one read of GitHub that opened this bundle's build answered them
	// (appauth.ControllerCredential), so the directories and the installations of one answer
	// are one moment's. `legion controller start` tells its controller which owners its App
	// is installed on from it, which Owners cannot say: a bundle also holds a directory for
	// an owner a configured rename moved a repository away from, where the App may not be
	// installed, and holds none for an installed owner whose lease GitHub refused. Never
	// null.
	Installed []string `json:"installed"`
}

// ControllerOwnerCredential is one owner's two gh files in a ControllerCredentialResponse.
type ControllerOwnerCredential struct {
	Hosts  string `json:"hosts"`
	Config string `json:"config"`
}

// ControllerRename is one configured pair of a ControllerCredentialResponse: the repository as
// it was recorded and the one GitHub holds it under now, each `<owner>/<name>`. A request for
// the first is answered from the gh directory of the second's owner (ghconfig.Bundle.Pairs).
type ControllerRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ControllerRegisterResponse is `POST /legion/v1/claims/register`'s answer to a session that
// registered as the project's controller: the project's controller role token
// (claim.ControllerToken), the role claim.RoleController, a generation, and the secret its
// controller grants authenticate with. It has no tree and no issue; a claim's registration is
// claim.RegisterResponse.
type ControllerRegisterResponse struct {
	ClaimToken claim.Token `json:"claimToken"`
	Role       claim.Role  `json:"role"`
	// Generation is the generation of the capability the operator's controller registered with,
	// and, for the daemon's own controller (`controller: daemon`), the generation of the launch
	// that registered, which its agent names on `claims/ready`.
	Generation uint64 `json:"generation"`
	Secret     string `json:"secret"`
}

// controllerSecret is `legion controller start`'s one daemon call: the operator's bearer, compared
// in constant time, buys a fresh controller capability. The mint replaces the previous capability
// and its registration, and ends every controller grant, so the controller it replaces stops being
// able to act the moment this answers — last start wins. A request whose plugin contract is not
// this daemon's is refused before the mint, naming both: the controller it would start is refused
// at registration, so minting for it would only cut the running controller off (a `legion` binary
// replaced before its daemon restarted, or the other way round). A daemon that launches its own
// controller (`controller: daemon`) refuses every request, since a mint would cut that controller
// off for a second one. Every refusal is one log line and mints nothing.
func (s *server) controllerSecret(w http.ResponseWriter, r *http.Request) {
	if !s.operatorAuthorized(r) {
		s.log.Warn("api: refused a controller secret: no operator bearer, or the wrong one")
		writeJSON(w, http.StatusForbidden, errorBody(invalidOperatorToken))
		return
	}
	if s.controllerLaunched {
		s.log.Warn("api: refused a controller secret: this daemon launches the project's controller itself")
		writeJSON(w, http.StatusConflict, errorBody(
			"this daemon launches the project's controller itself (controller: daemon), so legion controller start has none to start"))
		return
	}
	var req ControllerSecretRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if req.PluginContract != DaemonAPIVersion {
		s.log.Warn("api: refused a controller secret: legion controller start holds the plugin to another daemon API contract",
			"pluginContract", req.PluginContract, "daemonApiVersion", DaemonAPIVersion)
		named := "names no daemon API contract"
		if req.PluginContract != 0 {
			named = fmt.Sprintf("holds the controller's pi-legion to daemon API contract %d", req.PluginContract)
		}
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
			"legion controller start %s; this daemon requires %d: run the legion built with this daemon",
			named, DaemonAPIVersion)))
		return
	}
	secret := rand.Text()
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	// The controller store runs the revocation as the mint's commit hook, fenced by the generation
	// it confirms: a lost reply whose commit landed still revokes replaced grants once the store
	// says so, and an older confirmation leaves a newer mint's grants standing.
	if _, err := s.controller.MintControllerThen(context.WithoutCancel(r.Context()), s.project, capabilityHash(secret), s.revokeReplacedControllers); err != nil {
		s.log.Error("api: mint a controller capability", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("controller secret failed: the daemon could not record it"))
		return
	}
	writeJSON(w, http.StatusOK, ControllerSecretResponse{Secret: secret, DesignGateSteps: s.steps})
}

// revokeReplacedControllers is a controller mint's commit hook: it ends every controller grant of
// a generation below the one the mint committed, and says so.
func (s *server) revokeReplacedControllers(generation uint64) {
	s.grants.RevokeControllersBelow(generation)
	s.log.Info("api: minted a controller capability; the previous controller's registration and grants are revoked", "generation", generation)
}

// controllerGitHubCredential is `legion controller start`'s refresh call: proving the current
// controller capability buys the review App's fresh read-only tokens for every owner that App
// is installed on, each reaching every repository its own installation holds
// (appauth.ControllerCredential), each rendered as the two files `gh` reads. The capability
// compare runs under controllerMu, the same lock the mint and the controller-session grant take,
// so a capability a concurrent mint just replaced is never matched; the token mints run after the
// lock is released, since GitHub is the slow step and holding the lock across it would block every
// other controller-record read or mint for as long as GitHub takes to answer. A caller whose
// capability a later `legion controller start` has since replaced is refused with a code the CLI
// can tell apart from a missing or malformed secret, so its refresh loop stops rather than
// retrying forever; a daemon that launches its own controller (`controller: daemon`) refuses every
// call, as controllerSecret does, since its controller's tokens live in its pod's Secret and no
// `legion controller start` runs against it.
//
// A default owner the mint could not fill — GitHub refusing the read-only permissions the lease
// asks for on that installation (appauth.ErrNarrowingRefused) — is DEFAULT_OWNER_UNREACHABLE,
// naming the owner. The caller sees a refusal rather than a 200 whose bundle names no App and no
// owner, which it could only read as gh files it cannot write; its refresh loop keeps the files
// it has and tries again at its next tick, since the owner is reached again the moment the
// account grants the App those permissions. A caller that names no owner is answered from the
// owners the mint did reach, so this is the recorded default's case alone.
func (s *server) controllerGitHubCredential(w http.ResponseWriter, r *http.Request) {
	if s.controllerLaunched {
		s.log.Warn("api: refused a controller GitHub credential: this daemon launches the project's controller itself")
		writeFailure(w, http.StatusConflict, "CONTROLLER_LAUNCHED",
			"this daemon launches the project's controller itself (controller: daemon), so legion controller start has none to start")
		return
	}
	var req ControllerCredentialRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if !requireFailureFields(w, field{"secret", req.Secret}) {
		return
	}
	matched, err := s.controllerCapabilityCurrent(r, req.Secret)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the controller record for a GitHub credential")
		writeFailure(w, http.StatusInternalServerError, "CONTROLLER_RECORD_UNREADABLE", "could not read the controller record")
		return
	}
	if !matched {
		s.log.Warn("api: refused a controller GitHub credential: INVALID_CONTROLLER_CAPABILITY")
		writeFailure(w, http.StatusForbidden, "INVALID_CONTROLLER_CAPABILITY",
			"the controller capability is not the current one: a later legion controller start replaced it")
		return
	}
	if req.Default != "" && !ghconfig.IsOwner(strings.ToLower(req.Default)) {
		writeFailure(w, http.StatusBadRequest, "INVALID_DEFAULT_OWNER", "default is not a GitHub login")
		return
	}
	if s.tokens == nil {
		writeFailure(w, http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GitHub token source is unavailable")
		return
	}
	// The owners this build reads the review App installed on are kept for the answer
	// (ControllerCredentialResponse.Installed) in a record of this request's own, so what the
	// answer names is the read this bundle was built from and no other caller's. What a mint
	// found refused is recorded nowhere (appauth.LeftOut): this bundle is built per request
	// and reaches a `legion controller start` that rewrites its own gh directory every minute,
	// so no pod's fixed projection is ever compared against it.
	read := appauth.NewInstalled()
	bundle, err := appauth.ControllerCredential(s.tokens, s.githubRepositories, s.renames, read, nil, s.log)(r.Context(), req.Default)
	switch {
	case errors.Is(err, runtime.ErrNoGitHubOwner):
		writeFailure(w, http.StatusInternalServerError, "GITHUB_OWNER_UNCONFIGURED", "no repository owner is configured for GitHub App tokens")
		return
	case errors.Is(err, appauth.ErrDefaultOwnerNotInstalled):
		s.log.Warn("api: refused a controller GitHub credential: DEFAULT_OWNER_NOT_INSTALLED", "default", strings.ToLower(req.Default))
		writeFailure(w, http.StatusBadRequest, "DEFAULT_OWNER_NOT_INSTALLED", err.Error())
		return
	case appauth.NarrowingRefused(err):
		s.log.Warn("api: refused a controller GitHub credential: DEFAULT_OWNER_UNREACHABLE", "default", strings.ToLower(req.Default), "error", err)
		writeFailure(w, http.StatusInternalServerError, "DEFAULT_OWNER_UNREACHABLE", err.Error())
		return
	case err != nil:
		s.logRequestFailure(r, err, "api: mint the controller's GitHub App tokens")
		writeFailure(w, http.StatusBadGateway, "GITHUB_TOKEN_FAILED", "GitHub token exchange failed")
		return
	}
	installed, _ := read.Owners(appauth.AppRoleFor(claim.RoleController))
	if installed == nil {
		installed = []string{}
	}
	answer := ControllerCredentialResponse{
		Owners:    make(map[string]ControllerOwnerCredential, len(bundle.Dirs)),
		Default:   bundle.Default,
		App:       bundle.Dirs[bundle.Default].App,
		Installed: installed,
	}
	for _, pair := range bundle.Pairs {
		answer.Renames = append(answer.Renames, ControllerRename{From: pair.From.String(), To: pair.To.String()})
	}
	var soonest time.Time
	for owner, rendered := range bundle.Dirs {
		answer.Owners[owner] = ControllerOwnerCredential{Hosts: rendered.Hosts, Config: rendered.Config}
		if soonest.IsZero() || rendered.ExpiresAt.Before(soonest) {
			soonest = rendered.ExpiresAt
		}
	}
	answer.ExpiresAt = soonest.UTC().Format(timeFormat)
	s.log.Info("api: controller github credential served", "app", answer.App, "expiresAt", soonest, "owners", bundle.DirNames(), "installed", installed)
	writeJSON(w, http.StatusOK, answer)
}

// controllerCapabilityCurrent compares secret against the project's current controller
// capability under controllerMu, so the compare never races the mint that would replace it.
func (s *server) controllerCapabilityCurrent(r *http.Request, secret string) (bool, error) {
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	stored, found, err := s.controller.Controller(r.Context(), s.project)
	if err != nil {
		return false, err
	}
	return found && subtle.ConstantTimeCompare(stored.CapabilityHash, capabilityHash(secret)) == 1, nil
}

// registerController is a registration whose token is no launch's boot token, on a daemon that
// leaves the controller to its operator (`controller: operator`; register routes none here
// otherwise): it registers req's session as the project's controller when the token is the current
// controller capability, and answers the one refusal an unknown token gets otherwise. No boot gate
// checks the operator's Oh My Pi — the daemon gates only the panes it launches — so a controller
// whose plugin speaks another daemon API contract is refused here, naming both, and nothing is
// recorded. The registration is issued a secret of its own, persisted by its hash before this
// answers, which the session's controller grants authenticate with.
func (s *server) registerController(w http.ResponseWriter, r *http.Request, req claim.RegisterRequest) {
	ctx := context.WithoutCancel(r.Context())
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	stored, found, err := s.controller.Controller(ctx, s.project)
	if err != nil {
		s.log.Error("api: read the controller record for a registration", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not read its controller record"))
		return
	}
	if !found || subtle.ConstantTimeCompare(stored.CapabilityHash, capabilityHash(req.BootToken)) != 1 {
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	if req.PluginContract != DaemonAPIVersion {
		s.log.Warn("api: refused a controller registration: its plugin speaks another daemon API contract",
			"session", req.SessionID, "pluginContract", req.PluginContract, "daemonApiVersion", DaemonAPIVersion)
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
			"pi-legion speaks daemon API contract %d; this daemon requires %d", req.PluginContract, DaemonAPIVersion)))
		return
	}
	secret := rand.Text()
	registered, err := s.controller.RegisterController(ctx, s.project, stored.Generation, req.SessionID, capabilityHash(secret), time.Now().UTC(), req.PluginContract)
	if err != nil {
		s.log.Error("api: record a controller registration", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not record it"))
		return
	}
	if !registered {
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	token := claim.ControllerToken(s.project)
	s.log.Info("api: controller registered", "claim", token, "generation", stored.Generation,
		"session", req.SessionID, "agent", req.AgentID)
	writeJSON(w, http.StatusOK, ControllerRegisterResponse{
		ClaimToken: token,
		Role:       claim.RoleController,
		Generation: stored.Generation,
		Secret:     secret,
	})
}

// registerLaunchedController is the registration of a launch of the daemon's own controller, on a
// daemon under `controller: daemon` (register refuses one otherwise): its agent registers with the
// launch's boot token, which the daemon minted for that launch and wrote into the pod's Secret,
// where the operator's controller presents the capability its bearer bought. The claim's machine
// takes the registration first, behind the generation and same-agent fences every claim's has, and
// persists the session, its transcript and the issued secret's hash; then the session is recorded
// as the project's controller — the record admission's wakes, the controller grant route and the
// state read — under controller.UnheldCapability, which revokes every earlier controller's grants
// and keeps the boot token out of the record, whose invariant that function's doc states. The
// answer is the operator's controller's registration, its generation the launch's, which the
// agent's ready names.
func (s *server) registerLaunchedController(w http.ResponseWriter, r *http.Request, req claim.RegisterRequest, launch BootToken, m *supervise.Machine) {
	ctx, decided := s.decision(r, launch.Claim)
	defer decided()
	secret := rand.Text()
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	if err := m.Handle(ctx, supervise.RequestRegister{
		Claim: launch.Claim, Generation: launch.Generation, Session: req.SessionID, SessionFile: req.OmpSessionFile,
		CapabilityHash: capabilityHash(secret), PluginContract: req.PluginContract,
	}); err != nil {
		s.claimFailure(w, "register", launch.Claim, err)
		return
	}
	generation, err := s.controller.MintControllerThen(ctx, s.project, controller.UnheldCapability(), s.revokeReplacedControllers)
	if err != nil {
		s.logFailure("api: record the launched controller's capability", "claim", launch.Claim, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not record its controller"))
		return
	}
	registered, err := s.controller.RegisterController(ctx, s.project, generation, req.SessionID, capabilityHash(secret), time.Now().UTC(), req.PluginContract)
	if err != nil || !registered {
		s.logFailure("api: record the launched controller's registration", "claim", launch.Claim, "registered", registered, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not record its controller"))
		return
	}
	s.log.Info("api: launched controller registered", "claim", launch.Claim, "generation", launch.Generation,
		"session", req.SessionID, "agent", req.AgentID, "pluginContract", req.PluginContract)
	writeJSON(w, http.StatusOK, ControllerRegisterResponse{
		ClaimToken: launch.Claim,
		Role:       claim.RoleController,
		Generation: launch.Generation,
		Secret:     secret,
	})
}

// controllerSessionGrant is the grants route's controller-session form: the session registered
// with the current capability, proving the secret its registration was issued, mints a controller
// grant that names that session (credential.Grant.Session). Anything else answers the claim form's
// refusal.
func (s *server) controllerSessionGrant(w http.ResponseWriter, r *http.Request, req GrantRequest) {
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	stored, found, err := s.controller.Controller(r.Context(), s.project)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the controller record for a grant")
		writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
		return
	}
	if !found || !controllerSessionRegistered(stored, req.SessionID, req.Secret) {
		writeFailure(w, http.StatusForbidden, "INVALID_SESSION_SECRET", claim.InvalidSecret.Message)
		return
	}
	if s.refuseIfStaleController(w, r, stored) {
		return
	}
	grant, err := s.grants.MintController(req.SessionID, stored.Generation)
	if errors.Is(err, credential.ErrReplaced) {
		// A mint confirmed since the read above replaced the session's capability: the session
		// is no longer registered, as the next read of the record says.
		writeFailure(w, http.StatusForbidden, "INVALID_SESSION_SECRET", claim.InvalidSecret.Message)
		return
	}
	s.writeControllerGrant(w, grant, err)
}

// controllerSessionRegistered is the one authentication of a controller session against its
// record: the current capability's registration, naming this session, whose secret hashes to the
// recorded one. controllerSessionGrant and refuseIfStaleControllerSession both authenticate by it.
// The record holds the session as the store keeps it, so the caller's is compared as the store
// would keep it (storetext.Text).
func controllerSessionRegistered(stored controller.Record, session, secret string) bool {
	return stored.Registered() && stored.Session == storetext.Text(session) &&
		subtle.ConstantTimeCompare(stored.SecretHash, capabilityHash(secret)) == 1
}

// refuseIfStaleController is the stale-caller refusal for the operator's controller record
// (LEGION-164). When the deployment launches its own controller (`controller: daemon`), the caller named
// by this record is a claim: its machine (claim.ControllerToken) carries the contract its own
// registration recorded, so it is refused by the same rule and sentence (claim.StalePluginContract,
// staleClaim) any other claim is — no machine recorded means nothing is stale. Otherwise (the
// operator's own `legion controller start`) the daemon never relaunches the operator's controller,
// so there is no relaunch to wait for: it is stale, with claim.StaleControllerContract's sentence,
// whenever its recorded contract differs at all.
func (s *server) refuseIfStaleController(w http.ResponseWriter, r *http.Request, stored controller.Record) bool {
	if !s.controllerLaunched {
		if stored.PluginContract == DaemonAPIVersion {
			return false
		}
		s.logStaleRefusal(r, "session", stored.Session, stored.PluginContract)
		refusal := claim.StaleControllerContract(stored.PluginContract, DaemonAPIVersion)
		writeFailure(w, http.StatusConflict, "STALE_PLUGIN_CONTRACT", refusal.Message)
		return true
	}
	m, found := s.supervisor.Machine(claim.ControllerToken(s.project))
	if !found {
		return false
	}
	c := m.Claim()
	got, stale := s.staleClaim(c)
	if !stale {
		return false
	}
	s.logStaleRefusal(r, "session", stored.Session, got)
	refusal := claim.StalePluginContract(got, DaemonAPIVersion)
	writeFailure(w, http.StatusConflict, "STALE_PLUGIN_CONTRACT", refusal.Message)
	return true
}

// refuseIfStaleControllerSession is readBody's identity check for the grants route's controller
// form ({sessionId, secret}): the session registered with the current controller capability,
// authenticated exactly as controllerSessionGrant does.
func (s *server) refuseIfStaleControllerSession(w http.ResponseWriter, r *http.Request, sessionID, secret string) bool {
	s.controllerMu.Lock()
	record, found, err := s.controller.Controller(r.Context(), s.project)
	s.controllerMu.Unlock()
	if err != nil || !found || !controllerSessionRegistered(record, sessionID, secret) {
		return false
	}
	return s.refuseIfStaleController(w, r, record)
}

// writeControllerGrant answers the controller grant a route minted for a caller it has authorised:
// the operator's bearer (grant's empty form) or the registered controller session
// (controllerSessionGrant), or the failure of that mint.
func (s *server) writeControllerGrant(w http.ResponseWriter, grant credential.Grant, err error) {
	if err != nil {
		s.log.Error("api: mint controller grant", "error", err)
		writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
		return
	}
	writeJSON(w, http.StatusOK, GrantResponse{GrantID: grant.ID, ExpiresAt: grant.ExpiresAt.UTC().Format(timeFormat)})
}
