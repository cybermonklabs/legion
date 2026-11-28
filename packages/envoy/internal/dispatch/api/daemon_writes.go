package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// daemonWrite is a route only the Legion daemon writes, as its declaration states it
// (routeDecl.DaemonWrites): the check that tells the daemon apart from every other caller, and
// what the route writes, which a record route's refusal names. refuseNonDaemon makes the check
// ahead of the handler, so the declaration is what refuses, as ResultsScope is for a results
// token (refuseLimitedToken).
//
// The daemon is told apart by the session a write names, and not by its credential: the daemon
// presents the credential the deployment configured (the shared token, or a personal token where
// a deployment's dispatch_token_file holds one) or, under dispatch_token_broker, its own broker
// token, and every write of it names the session legion-daemon:<PROJECT> (packages/daemon's
// dispatch.DaemonSession, dispatch.WritesAs). So a person's sign-in, whose actor is the person,
// and a person's token that names its own session or proves one are refused; a bearer that proves
// no session and names the daemon's in its body is taken for the daemon, as each route's
// description says, and a project's proven-sessions setting is what refuses that one on the
// role-holder publication.
type daemonWrite struct {
	// Check is the check that tells the daemon apart. The routes differ in it, so each
	// declaration names its own.
	Check daemonCheck
	// Record is what a daemonRecordWriter route writes, as its refusal names it: "a step record",
	// "a plan". A daemonRoleHolderPublisher route's refusal names the issue's roles and takes none.
	Record string
}

// daemonCheck is how a route only the Legion daemon writes tells the daemon apart (daemonWrite).
type daemonCheck int

const (
	// daemonRecordWriter is a write of an issue's records - a step record, its rating, its
	// experiment, a plan - whose actor must be the session of a daemon that writes the issue's
	// records (isIssueDaemon): the issue's project's daemon, or another project's daemon whose
	// broker token proves its session and names its project. Every other writer is 403
	// STEP_WRITER (requireIssueDaemon).
	daemonRecordWriter daemonCheck = iota + 1
	// daemonRoleHolderPublisher is the publication of an issue's role holders. With
	// Deps.LegionStatusScope on, only a daemon's own broker token publishes: it holds
	// legion-status and proves the session of the issue's project's daemon, or another project's
	// daemon's session and names its project (roleHolderDaemonProven). With it off, the actor must
	// be such a daemon's session (isIssueDaemon). Every other caller is 403 ROLE_HOLDER_FORBIDDEN
	// (requireRoleHolderPublisher).
	daemonRoleHolderPublisher
)

// codeRoleHolderForbidden refuses a role-holder publication whose writer is no daemon's
// (daemonRoleHolderPublisher).
const codeRoleHolderForbidden = "ROLE_HOLDER_FORBIDDEN"

// code is the code of the 403 the route's gate answers every caller that is not the daemon: the
// code the route's handler answered such a caller with before the gate took the check over from
// it, so a caller reads the refusal it read before.
func (write daemonWrite) code() string {
	if write.Check == daemonRoleHolderPublisher {
		return codeRoleHolderForbidden
	}
	return codeStepWriter
}

// refuseNonDaemon is the gate register mounts every route that states DaemonWrites behind
// (routeHandler): it answers every caller that is not the Legion daemon 403 with the route's code
// (daemonWrite.code), naming the session that writes the route, and next does not run. It reads
// the writer as the route's handler reads it: the body's actor, read as the handler decodes its
// body (bodyActor), and the caller optionalActor resolves (actorFrom, requestWriter). A body the
// handler's decode refuses runs next, which refuses it as it always did, and a writer that does
// not resolve is answered as requireActor answers it, so every caller is answered in the order,
// with the status and the code, it was answered before the gate. The caller it resolves rides on
// the request (withResolvedCaller), so the handler resolves no credential a second time.
func (s *server) refuseNonDaemon(write daemonWrite, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = s.withResolvedCaller(r)
		supplied, decoded := bodyActor(r)
		if !decoded {
			next(w, r)
			return
		}
		switch write.Check {
		case daemonRecordWriter:
			if !s.requireIssueDaemon(w, r, supplied, write.Record) {
				return
			}
		case daemonRoleHolderPublisher:
			if !s.requireRoleHolderPublisher(w, r, supplied) {
				return
			}
		}
		next(w, r)
	}
}

// requireIssueDaemon is the check the daemon's gate makes of a record write (daemonRecordWriter):
// it reports whether the caller's actor (requireActor) writes the records of the issue its path
// names (isIssueDaemon), the session of the issue's project's Legion daemon or the session of
// another project's daemon that the request's broker token proves. It refuses any other writer 403
// STEP_WRITER, naming what record and whose, and an actor that does not resolve as requireActor
// refuses it.
func (s *server) requireIssueDaemon(w http.ResponseWriter, r *http.Request, supplied *model.Actor, record string) bool {
	actor, ok := s.requireActor(w, r, supplied)
	if !ok {
		return false
	}
	key := r.PathValue("key")
	if s.isIssueDaemon(r, actor, key) {
		return true
	}
	writeError(w, codeStepWriter, http.StatusForbidden, fmt.Sprintf(
		"only a Legion daemon writes %s of %s: the session %s, or the session legion-daemon:<P> of another project's daemon whose broker token names P and proves that session",
		record, key, legionDaemonSessionOf(key)))
	return false
}

// requireRoleHolderPublisher is the check the daemon's gate makes of a role-holder publication
// (daemonRoleHolderPublisher): r's writer (requestWriter) must publish the role holders of the
// issue its path names. With Deps.LegionStatusScope on, that is a daemon's own broker token
// (roleHolderDaemonProven); with it off, a daemon's session (isIssueDaemon). It answers every other
// caller 403 ROLE_HOLDER_FORBIDDEN, and a writer that does not resolve as requestWriter refuses it.
func (s *server) requireRoleHolderPublisher(w http.ResponseWriter, r *http.Request, supplied *model.Actor) bool {
	writer, err := s.requestWriter(r, supplied)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return false
	}
	key := r.PathValue("key")
	daemon := legionDaemonSessionOf(key)
	if s.deps.LegionStatusScope {
		if s.roleHolderDaemonProven(r, writer, key) {
			return true
		}
		writeError(w, codeRoleHolderForbidden, http.StatusForbidden, fmt.Sprintf(
			"only a Legion daemon's own broker token publishes the roles of %s. It holds the %s scope, and it proves session %s, or it proves session legion-daemon:<P> and names project P.",
			key, model.ScopeLegionStatus, daemon))
		return false
	}
	if s.isIssueDaemon(r, writer.actor, key) {
		return true
	}
	writeError(w, codeRoleHolderForbidden, http.StatusForbidden, fmt.Sprintf(
		"only a Legion daemon names the sessions that hold the roles of %s: the daemon of project %s, as session %s, or another project's daemon, as session legion-daemon:<P> with a broker token that names P and proves that session.",
		key, legionProjectOf(key), daemon))
	return false
}

// bodyActor is the actor r's body names, read as decodeJSON reads a body (readJSONBody): a JSON
// content type, one value of at most maxJSONRequestBytes, no U+0000 in any of its strings. It
// puts r's body back whole, the bytes it read and then the rest, so the handler reads and decodes
// every byte of it again. It answers false where that read refuses the body; the handler's own
// decode then refuses it as it always did. The members beside actor are the handler's to judge,
// so none of them is refused here (judgeUndeclaredMembers).
func bodyActor(r *http.Request) (*model.Actor, bool) {
	if r.Body == nil {
		return nil, false
	}
	// One byte past the limit, so a body over it is still over it when the handler reads it.
	read, err := io.ReadAll(io.LimitReader(r.Body, maxJSONRequestBytes+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(read), r.Body))
	if err != nil {
		return nil, false
	}
	probe := r.WithContext(r.Context())
	probe.Body = io.NopCloser(bytes.NewReader(read))
	var input actorInput
	if _, err := readJSONBody(probe, &input, maxJSONRequestBytes, errJSONTooLarge); err != nil {
		return nil, false
	}
	return input.Actor, true
}
