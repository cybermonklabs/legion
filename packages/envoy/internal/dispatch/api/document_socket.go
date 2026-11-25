package api

// document_socket.go is how the api package admits GET /ws/doc/{room}, the document socket, before
// the document service serves it: which credential opens a socket, how every other credential is
// refused, and whether the connection may write. The document service takes the caller decided
// here as it is handed (docs.SocketCaller, docs.WithSocketCaller) and reads no credential of its
// own, since the DPoP proof of a broker-issued token is spent once it has been read.

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// documentSocketPattern is the path of the document socket, which register mounts beside the
// table.
const documentSocketPattern = "/ws/doc/{room}"

// codeDocSocketCredentialRefused refuses a document socket's request whose Authorization header
// holds no DPoP presentation and whose sign-in does not resolve: the shared bearer, a personal
// token, a service-account token, or any other scheme. It is the api package's own code, as the
// broker token's are (brokerTokenActor); no client in this repository reads it.
const codeDocSocketCredentialRefused = "DOC_SOCKET_CREDENTIAL_REFUSED"

// docSocketCredentialRefusal is that refusal's message. It names no credential, so no answer
// repeats what the caller sent.
const docSocketCredentialRefusal = "the document socket takes a sign-in, or an access token the secrets broker issued under DPoP: nothing ran"

// documentSocketHandler is the chain register mounts GET /ws/doc/{room} behind, around serve,
// which is the document service's ServeHTTP: one place for the order, as liveSocketHandler is for
// the live socket's. From the outside in:
//
//   - the access-log line, which names the caller the layers inside it decided;
//   - the process name (nameDocumentSocketProcess), set before any layer inside it answers, so
//     every answer the api package writes for the socket names the process that wrote it;
//   - the socket's write holder (holdDocumentSocketWrite), outside the heartbeat so the
//     heartbeat's copy of the request shares it;
//   - the heartbeat (whileCallerResolves), which asks on each StreamHeartbeat whether the
//     credential that admitted the socket still resolves, and judges the spec-writer rule again
//     for a socket armed to write a document the rule holds (documentSocketStillResolves);
//   - the broker token's decision (decideBrokerToken), made once for the request, since its
//     proof is single use;
//   - the store's transaction tracking (trackTransactions);
//   - the credential rule (refuseDocumentSocketCredential), which reads no room;
//   - the parameter check (refuseUnstorableParameters), so no room Postgres refuses as text
//     reaches a read;
//   - the write decision (decideDocumentSocketWrite), which reads the room's artifact and fills
//     the write holder.
//
// The process name is the document service's own (docs.Service.ProcessName), found by an
// interface of its own, so a document service that names no process mounts the same chain
// without the header.
func (s *server) documentSocketHandler(serve http.HandlerFunc) http.Handler {
	process := ""
	if named, ok := s.deps.Docs.(interface{ ProcessName() string }); ok {
		process = named.ProcessName()
	}
	return accesslog.HandlerOn(documentSocketPattern, nameDocumentSocketProcess(process, holdDocumentSocketWrite(
		s.whileCallerResolves(s.documentSocketStillResolves, s.decideBrokerToken(trackTransactions(
			s.refuseDocumentSocketCredential(s.refuseUnstorableParameters(documentSocketPattern,
				s.decideDocumentSocketWrite(serve)))))))), s.clock())
}

// documentSocketWrite is a document socket's write as its heartbeat judges it again: the
// spec-writer rule's writer and the document the rule holds, armed once the document service has
// admitted the socket writable (decideDocumentSocketWrite, docs.SocketCaller.Admitted). It is
// armed on the goroutine that serves the socket and read on the heartbeat's, so it is an atomic
// pointer. It stays empty for every socket the rule cannot refuse later - a sign-in's, a
// read-only one, one whose credential holds the person scope, one to a document the rule does not
// hold - and for one the document service admitted read-only (a closed issue, a page the fleet
// does not admit), and its heartbeat then reads nothing of the rule.
type documentSocketWrite struct {
	armed atomic.Pointer[documentSpecWrite]
}

// documentSpecWrite is a document socket's write that the spec-writer rule holds: who writes, as
// the rule judges the token's write (specWriter), and the document.
type documentSpecWrite struct {
	writer   specWriter
	artifact model.Artifact
}

// documentSocketWriteKey carries a document socket's documentSocketWrite on its request's context.
type documentSocketWriteKey struct{}

// holdDocumentSocketWrite puts one empty documentSocketWrite on the request's context for next:
// the holder decideDocumentSocketWrite fills and documentSocketStillResolves reads, which share it
// because the heartbeat's copy of the request is made inside this.
func holdDocumentSocketWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(context.WithValue(r.Context(), documentSocketWriteKey{}, &documentSocketWrite{})))
	}
}

// documentSocketStillResolves is the document socket's heartbeat (whileCallerResolves). It asks
// again of the credential that admitted the socket, and of nothing the request carries beside it:
//
//   - A broker-issued token (ciphertoken.Presented) is asked what the live socket's heartbeat asks
//     of one (brokerTokenHeartbeat), the one check both sockets call, so a check added to it holds
//     here too. Then, for a socket armed to write a document the spec-writer rule holds
//     (documentSocketWrite), the rule is judged again (specWriterRefusesDocument), since its
//     answer can change while the socket is open: the architect role moves, the label legion is
//     added, the project is marked. A refusal, or a read of the rule that fails, logs one line and
//     answers errDocumentWriteRevoked, which closes the socket with 4412; the provider
//     reconnects, and the new upgrade decides the write afresh. A socket the holder leaves
//     unarmed costs no read.
//   - Any other request was admitted by its sign-in (refuseDocumentSocketCredential), which is
//     asked again (signInStillResolves). A bearer sent beside the cookie is never read, so it
//     never keeps the socket open once the sign-in has ended.
func (s *server) documentSocketStillResolves(r *http.Request) error {
	if _, presented := ciphertoken.Presented(r); !presented {
		return s.signInStillResolves(r)
	}
	if err := s.brokerTokenHeartbeat(r); err != nil {
		return err
	}
	held, _ := r.Context().Value(documentSocketWriteKey{}).(*documentSocketWrite)
	if held == nil {
		return nil
	}
	write := held.armed.Load()
	if write == nil {
		return nil
	}
	refuses, err := specWriterRefusesDocument(r.Context(), s.deps.Store.Pool, write.writer, write.artifact)
	switch {
	case err != nil && r.Context().Err() != nil:
		// The socket's own end cut the read short: it is closing already, and closes nothing more.
		return err
	case err != nil:
		slog.Warn("dispatch: closed a writable document socket: the read of the spec-writer rule failed; the provider reconnects",
			"artifact", write.artifact.ID, "issue", *write.artifact.IssueKey, "session", write.writer.actor.ID, "error", err)
		return errDocumentWriteRevoked
	case refuses:
		slog.Info("dispatch: closed a writable document socket whose token the spec-writer rule no longer lets write; the provider reconnects read-only",
			"artifact", write.artifact.ID, "issue", *write.artifact.IssueKey, "session", write.writer.actor.ID)
		return errDocumentWriteRevoked
	}
	return nil
}

// nameDocumentSocketProcess sets docs.ProcessHeader to process on the answer's header before next
// runs, so every answer the api package writes for a document socket's request - a refusal of its
// credential, of its parameters, or of a read of its room - names the process that wrote it, as
// the document service's own answers do, and a caller records which process refused it. The
// upgrade's 101 is written on the hijacked connection, where the document service adds the header
// itself. An empty process is one the document service names on none of its answers, so nothing
// is set.
func nameDocumentSocketProcess(process string, next http.HandlerFunc) http.HandlerFunc {
	if process == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(docs.ProcessHeader, process)
		next(w, r)
	}
}

// refuseDocumentSocketCredential is the document socket's credential rule. It admits two
// credentials, a broker-issued token and a sign-in, and reads the Authorization header for one
// thing only, whether it presents the first:
//
//   - A DPoP presentation (ciphertoken.Presented) is decided by decideBrokerToken, outside this.
//     The read-only gate runs on this branch alone, so a read-only broker token is refused 403
//     READ_ONLY_TOKEN, as on GET /ws/live; mounted on every request, the gate would look a
//     personal bearer up (resolvePersonalToken), which the socket never does. The decided caller
//     is then read through optionalActor, and a refusal is written as the API writes it
//     (writeAuthenticationError), each code, status and Retry-After its own. A cookie beside the
//     token is not read: the token decides, as it does on the API.
//   - Any other Authorization header that is not blank - the shared bearer, a personal token, a
//     service-account token, another scheme - is never classified, looked up or verified
//     (credentialOf is not called). Only the sign-in is read (Identity.Login), and a person it
//     resolves is admitted whatever the header holds. Any failure, a lapsed confirmation the
//     sign-in issuer cannot renew included, is 401 DOC_SOCKET_CREDENTIAL_REFUSED.
//   - With no Authorization header, or a blank one (auth.BearerToken's present), the sign-in is
//     read as optionalActor reads it: 500 IDENTITY_ERROR with no identity configured, and a
//     failure written by writeAuthenticationError, 401 NO_IDENTITY or 503 SIGNIN_UNAVAILABLE.
//
// A person the sign-in resolves is noted on the access-log line as optionalActor notes one, and
// handed to the document service as that person, Proven, who may write wherever the document
// service's own conditions let the socket write. A token's caller is handed on by
// decideDocumentSocketWrite, which reads the room.
func (s *server) refuseDocumentSocketCredential(next http.HandlerFunc) http.HandlerFunc {
	token := s.refuseReadOnlyToken(http.MethodGet+" "+documentSocketPattern, readEntry{}, func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := s.optionalActor(r); err != nil {
			s.writeAuthenticationError(w, err)
			return
		}
		next(w, r)
	})
	return func(w http.ResponseWriter, r *http.Request) {
		if _, presented := ciphertoken.Presented(r); presented {
			token(w, r)
			return
		}
		_, bearer := auth.BearerToken(r)
		var login string
		var err error = errorf(http.StatusInternalServerError, "IDENTITY_ERROR", "identity service unavailable")
		if s.deps.Identity != nil {
			login, err = s.deps.Identity.Login(r)
		}
		if err != nil {
			if bearer {
				writeError(w, codeDocSocketCredentialRefused, http.StatusUnauthorized, docSocketCredentialRefusal)
				return
			}
			s.writeAuthenticationError(w, err)
			return
		}
		accesslog.Note(r.Context(), accesslog.KindUser, login)
		next(w, r.WithContext(docs.WithSocketCaller(r.Context(), docs.SocketCaller{
			Actor: model.Actor{Kind: "user", ID: login, Proven: true}, MayWrite: true,
		})))
	}
}

// decideDocumentSocketWrite hands the document service a broker-issued token's caller and
// whether its connection may write; a request that presents no token passes through with the
// sign-in's caller refuseDocumentSocketCredential handed on.
//
// The caller is the session the token names, recorded as an API write with the same token records
// it (bearerSessionActor): its owner, Proven, and the act naming the session the broker proved, so
// graphstore reads the token's person as the author of every edit the connection sends. For a
// token that names a person, bearerSessionActor's note stands, and the access-log line reads
// caller_kind=session caller=<session> credential=broker-token:<person>, as that API write's does.
// A token that names no person is recorded on a context the line does not read
// (accesslog.Detached), so the line keeps brokerTokenActor's note, caller_kind=broker-token
// caller=service:<service>, as an API read with the token logs.
//
// The connection may write only where the same token may write the document through POST
// /api/v1/artifacts/{id}/edits (documentWritable), and a token that names no person writes
// nothing here at all, though the edits route lets an account entry's such token write under its
// account (actsAsItsAccount): no caller of the socket needs that write. A read the decision makes
// that fails refuses the upgrade (writeHandlerError), so no connection is admitted writable on a
// read that did not answer. The document service's own conditions, an open issue and a live fleet
// that admits the page, still decide beside this one (docs.SocketCaller.MayWrite).
//
// A writable connection to a document the spec-writer rule holds is judged by the rule again at
// each heartbeat, since the rule's answer can change while the connection is open
// (documentSocketStillResolves). So the writer and the document go into the socket's write
// holder (documentSocketWrite), armed only once the document service reports that it admitted the
// socket writable (docs.SocketCaller.Admitted); a socket it opens read-only costs its heartbeat no
// read. The heartbeat reaches the rule only through the holder documentSocketHandler puts on the
// request, so a request that carries none may not write such a document at all.
func (s *server) decideDocumentSocketWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, presented := ciphertoken.Presented(r); !presented {
			next(w, r)
			return
		}
		caller, _, err := s.optionalActor(r)
		if err != nil {
			s.writeAuthenticationError(w, err)
			return
		}
		person := caller.Owner != nil && *caller.Owner != ""
		noted := r
		if !person {
			noted = r.WithContext(accesslog.Detached(r.Context()))
		}
		recorded, err := bearerSessionActor(noted, caller, nil)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		mayWrite := false
		var recheck *documentSpecWrite
		if person {
			if mayWrite, recheck, err = s.documentWritable(r.Context(), r.PathValue("room"), caller, recorded); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		handed := docs.SocketCaller{Actor: recorded, MayWrite: mayWrite}
		if recheck != nil {
			held, ok := r.Context().Value(documentSocketWriteKey{}).(*documentSocketWrite)
			handed.MayWrite = ok
			if ok {
				handed.Admitted = func(writable bool) {
					if writable {
						held.armed.Store(recheck)
					}
				}
			}
		}
		next(w, r.WithContext(docs.WithSocketCaller(r.Context(), handed)))
	}
}

// documentWritable is whether caller, a broker-issued token's caller that names a person and is
// recorded as recorded, may write room's document, as the edits route decides the same token's
// write, and, for a write the spec-writer rule holds and passes, that write, which the socket's
// heartbeat judges again (documentSpecWrite; nil for every other answer):
//
//   - A token that names one project writes only a document of that project, read as the
//     project check reads it (artifactProject). A document of no project, a room that is no
//     artifact and a room Postgres will not read as a uuid resolve to no project, so the answer
//     is no, as withinProjectScope fails closed.
//   - The spec-writer rule passes (specWriterRefusesDocument): on an issue Legion runs, the
//     issue's spec or Verification document is written only by a person's credential or by an
//     architect session of the issue's chain. Its refusal is a no and writes no answer. A
//     credential that holds the person scope passes it at once, so its document is not read, and
//     no later answer of the rule can refuse it.
//
// A room that is no artifact (ARTIFACT_NOT_FOUND, isArtifactNotFound) is a no as well, and the
// api package writes no 404 for it: the document service answers that room 404 itself. The reads
// go to the pool and hold no transaction (store.ErrNestedAcquire). Any other error is a read that
// failed, which the caller answers.
func (s *server) documentWritable(ctx context.Context, room string, caller, recorded model.Actor) (bool, *documentSpecWrite, error) {
	pool := s.deps.Store.Pool
	if caller.Project != nil && *caller.Project != "" {
		project, err := artifactProject(ctx, pool, room)
		if err != nil || project != *caller.Project {
			return false, nil, err
		}
	}
	writer := specWriter{actor: recorded, person: caller.Credential.Holds(model.ScopePerson)}
	if writer.person {
		return true, nil, nil
	}
	artifact, err := loadArtifactRowByID(ctx, pool, room)
	if isArtifactNotFound(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	refuses, err := specWriterRefusesDocument(ctx, pool, writer, artifact)
	if err != nil || refuses {
		return false, nil, err
	}
	if _, held := specWriterDocumentName(artifact); !held {
		return true, nil, nil
	}
	return true, &documentSpecWrite{writer: writer, artifact: artifact}, nil
}

// specWriterRefusesDocument is whether the spec-writer rule refuses writer a write of artifact, as
// refuseSpecWriterOfDocument judges the edits route's write (judgeSpecWriter), with no line
// logged and no answer made: a document socket the rule refuses opens read-only, and no write was
// refused. A document the rule does not hold is read no further.
func specWriterRefusesDocument(ctx context.Context, q queryer, writer specWriter, artifact model.Artifact) (bool, error) {
	document, held := specWriterDocumentName(artifact)
	if !held {
		return false, nil
	}
	refusal, _, err := judgeSpecWriter(ctx, q, writer, specWrite{issue: *artifact.IssueKey, document: document})
	return refusal != nil, err
}
