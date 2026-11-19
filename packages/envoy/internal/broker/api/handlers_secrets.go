package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// settingsResponse is GET /v1/settings's answer: the store the agent secrets live in and, per
// store, where.
type settingsResponse struct {
	// Which secret store holds the agent secrets: aws-secrets-manager or gcp-secret-manager. A
	// broker from before this field answered none, and held them in AWS Secrets Manager.
	Store string `json:"store"`
	// The name prefix every agent secret sits under: with aws-secrets-manager a Secrets Manager
	// name prefix ending in "/", with gcp-secret-manager projects/<project number>/secrets/<secret-id
	// prefix>-. The secret a session asks for as DEEL_API_KEY is <prefix>deel-api-key.
	SecretsPrefix string `json:"secrets_prefix"`
	// With aws-secrets-manager, the ARN of the agent-secrets KMS key, which every agent secret
	// must be encrypted with; absent otherwise.
	KMSKeyARN string `json:"kms_key_arn,omitempty"`
	// With aws-secrets-manager, the AWS account the agent-secrets key, and so every agent secret,
	// is in; absent otherwise.
	AWSAccountID string `json:"aws_account_id,omitempty"`
	// With aws-secrets-manager, the AWS region the agent-secrets key, and so every agent secret,
	// is in; absent otherwise.
	AWSRegion string `json:"aws_region,omitempty"`
	// With gcp-secret-manager, the resource name of the agent-secrets Cloud KMS key, which every
	// agent secret must be encrypted with; absent otherwise.
	KMSKeyName string `json:"kms_key_name,omitempty"`
	// With gcp-secret-manager, the number of the project every agent secret is in, the one the
	// prefix names; absent otherwise.
	GCPProject string `json:"gcp_project,omitempty"`
	// JudgeKeys is each key the airlock rules give the model judge, by name, with its judge
	// seconds and the guidance the judge decides it on — the rules file's own words, never a
	// value — and absent where the rules give the judge nothing. It is what `agent-secrets secret
	// show` reads to say who decides a request for a key and what the judge will look for, so an
	// agent can meet the guidance on its first request. It is the rules' answer and the same on
	// every broker with the same rules; one that runs no judge sends such a request to its
	// person. An administrator's key is named here when the rules give it seconds, and its
	// person decides it all the same; `secret show` reads its tag.
	JudgeKeys map[string]judgeKey `json:"judge_keys,omitempty"`
	// The audience a machine's Google identity token must carry at the token exchange
	// (BROKER_GOOGLE_OIDC_AUDIENCE), on either store; absent where no machine can trade one, which
	// is a broker that lists no VM service (cmd/broker's googleExchangeAudience). A machine's
	// `agent-secrets` mints its identity token for this audience with no setting. No Kubernetes
	// exchange audience is named beside it: a pod's trade mints no launcher credential, so no
	// machine could register a session under it.
	GoogleExchangeAudience string `json:"google_exchange_audience,omitempty"`
	// Always true: this broker reads a secret's type from the type tag (policy.TagType), and the
	// old kind tag only where type is absent. A broker from before the type tag sends no such
	// field and reads no type tag, so it serves a secret that carries type alone as a stored
	// secret, and `agent-secrets` writes no typed secret through a broker that does not say this.
	SecretTypeTag bool `json:"secret_type_tag"`
}

// judgeKey is one key in settingsResponse.JudgeKeys.
type judgeKey struct {
	// Seconds is the longest grant the judge makes of the key, its default lifetime too.
	Seconds int64 `json:"seconds"`
	// Guidance is the key owner's instruction to the judge, "" for an entry that gives none.
	Guidance string `json:"guidance,omitempty"`
}

// readSettings is GET /v1/settings: public, since it answers only the broker's own configuration —
// where the secrets live, and which names the judge decides and on what guidance — and no secret's
// value, and since two of its callers hold no credential of the broker's when they read it: a
// machine's `agent-secrets` reads google_exchange_audience to mint the identity token it trades
// for its first credential (machineSource), and the secret forms read the store, the prefix and
// the key under the cloud credential alone, with no identity at the broker at all.
func (s *server) readSettings(w http.ResponseWriter, r *http.Request) {
	resp := settingsResponse{Store: s.deps.SecretsStore, SecretsPrefix: s.deps.SecretsPrefix,
		GoogleExchangeAudience: s.deps.GoogleExchangeAudience, SecretTypeTag: true}
	switch s.deps.SecretsStore {
	case secrets.StoreGCP:
		resp.KMSKeyName = s.deps.SecretsKMSKey
		resp.GCPProject = secrets.GCPProject(s.deps.SecretsPrefix)
	default:
		resp.KMSKeyARN = s.deps.SecretsKMSKey
		resp.AWSRegion, resp.AWSAccountID = secrets.AWSKeyARNParts(s.deps.SecretsKMSKey)
	}
	if m := s.deps.Machine; m != nil {
		for _, rule := range m.Airlock.Secrets {
			if rule.Judge != nil {
				if resp.JudgeKeys == nil {
					resp.JudgeKeys = map[string]judgeKey{}
				}
				resp.JudgeKeys[rule.Name] = judgeKey{Seconds: int64(*rule.Judge), Guidance: rule.Guidance}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// rereadResponse is POST /v1/secrets/{name}/reread's answer to a session (rereadSecret).
type rereadResponse struct {
	// The name reread, as the path gave it.
	Name string `json:"name"`
	// True when the broker now serves the secret.
	Served bool `json:"served"`
	// Why the broker does not serve it, absent when it does: "absent" (no secret under the prefix
	// carries the name, or the one that does is scheduled for deletion) or the reason the broker
	// refused the secret, the one its policy-refused log line names (such as owner-tag-missing or
	// no-current-value).
	Reason string `json:"reason,omitempty"`
	// The type the broker serves the secret as, while it serves it: github-app or google-login, or
	// absent for a stored secret, whose grants release its stored value. A person who retags a
	// secret to a type reads it here before storing the key the type mints from: a stale store read
	// can still serve the secret as stored.
	Type string `json:"type,omitempty"`
}

// rereadAccepted is POST /v1/secrets/{name}/reread's answer to a caller that sends no session
// proof: one body, the same for every name, written before the reread reads the store
// (rereadSecret), so neither what it says nor how long it takes says whether the store holds the
// name.
type rereadAccepted struct {
	// Always true: the broker took the reread and runs it after this answer. It says nothing of
	// what the broker then serves; only a session's call is answered that.
	Accepted bool `json:"accepted"`
}

// rereadSecret is POST /v1/secrets/{name}/reread: open to any caller and rate-limited per source
// address and broker-wide (refuseReread) before any work, it reads the one secret from the store
// (policy.Current.RefreshOne) and tells every other pod serving the same database to reread it
// too (notifyPeers), so one call reaches the whole fleet rather than the pod the load balancer
// happened to pick. A name no secret can carry is 400 SECRET_NAME_INVALID, a check of the name's
// shape alone, the same for every caller.
//
// What it answers depends on the caller, never on the name. A caller that sends no Proof header is
// answered 202 and rereadAccepted at once, and the reread runs after that answer
// (rereadAfterAnswer): the answer is the same for a name the store holds as for one it does not,
// and so is when it comes, which a reread the caller waited on would not give, since the store
// takes one call to say it holds no such secret and more to describe one it holds. Such callers
// still need the reread itself — the secret forms of an `agent-secrets` with no broker identity,
// a person's own curl after a write — so it is run, not dropped, and costs what it always did:
// the same two buckets meter it.
//
// A caller that sends a Proof header must be a session the broker authenticates, as on every
// session route (sessionCaller: 401 PROOF_INVALID, or 503 when the proof cannot be checked), and
// is answered as before, once the reread is done: whether the broker serves the name, why not, and
// as which type. A session learns no more here than it could learn by asking for the secret. A
// session's reread whose request ended first - its caller went away, or its deadline passed - is
// the request's failure, not the broker's, so it logs nothing, as the miss path logs nothing for
// one: anyone can end a request, and the "broker: reread secret failed" line is the one an
// operator's alert counts. The peers are told on the 200, and on the 503 of a reread whose request
// ended during its value read alone (policy.ErrValueUnchecked), which has already put the tags it
// read in force here.
func (s *server) rereadSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	recorder := answer.FromContext(r.Context())
	recorder.NoteSecrets(name)
	if s.refuseReread(w, r) {
		return
	}
	if err := s.deps.Policy.CheckName(name); err != nil {
		writeError(w, http.StatusBadRequest, "SECRET_NAME_INVALID", err.Error())
		return
	}
	if r.Header.Get("Proof") == "" {
		writeJSON(w, http.StatusAccepted, rereadAccepted{Accepted: true})
		s.rereadAfterAnswer(r.Context(), name)
		return
	}
	// Until the proof verifies, the record names the caller unauthenticated, as a session
	// route's refusal raised in authentication does.
	recorder.NoteCaller(answer.CallerUnauthenticated, "")
	who, ok := s.sessionCaller(w, r, "PROOF_INVALID", "proof invalid: not live")
	if !ok {
		return
	}
	recorder.NoteCaller(answer.CallerSession, who.enrollment)
	lk, err := s.deps.Policy.RefreshOne(r.Context(), name)
	switch {
	case err != nil && r.Context().Err() != nil:
		// A reread that merged the tags it read before its value read was cut serves them here,
		// so the peers hear of it as of a 200 (notifyPeers).
		if errors.Is(err, policy.ErrValueUnchecked) {
			s.notifyPeers(r.Context(), name)
		}
		writeError(w, http.StatusServiceUnavailable, "REQUEST_ENDED", "the request ended before the reread finished; send it again")
		return
	case err != nil:
		writeInternal(w, "reread secret", err)
		return
	}
	s.notifyPeers(r.Context(), name)
	resp := rereadResponse{Name: name, Served: lk.Served, Reason: lk.Reason}
	if lk.Served {
		resp.Type = lk.Secret.Type()
	}
	writeJSON(w, http.StatusOK, resp)
}

// rereadAfterAnswer runs the reread rereadSecret has already answered a caller with no session
// proof for, and then tells the peers, on a goroutine of its own: the caller's answer is written
// and its handler returns without waiting on the store, so the connection the caller holds closes
// or serves its next request at the same moment whatever the store holds. It keeps every value on
// the request's context and drops its cancellation, as notifyPeers does, since the answer is
// written as soon as this starts, and it bounds the reread by rereadAfterAnswerTimeout instead.
// What it bounds is no more than the buckets admit: refuseReread spent a token before this was
// started, so the rereads in flight are at most what those buckets let through in one timeout.
//
// No caller waits on it, so its failure is the broker's, a store or a writer lock that did not
// answer in time among them, and it logs the "broker: reread secret failed" line the reread a
// session waits on logs. A reread whose value read alone that bound cut has merged the tags it
// read, as a session's cut there has, so the peers are told of it all the same.
func (s *server) rereadAfterAnswer(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rereadAfterAnswerTimeout)
	go func() {
		defer cancel()
		_, err := s.deps.Policy.RefreshOne(ctx, name)
		if err != nil {
			slog.Error("broker: reread secret failed", "name", name, "error", err)
		}
		if err == nil || errors.Is(err, policy.ErrValueUnchecked) {
			s.notifyPeers(ctx, name)
		}
	}()
}

// rereadAfterAnswerTimeout bounds a reread no caller waits on (rereadAfterAnswer): the 45 seconds
// cmd/broker gives every request (its requestDeadline), which bounded this reread too while its
// caller waited on it.
const rereadAfterAnswerTimeout = 45 * time.Second

// notifyPeers tells the other pods to reread name (Deps.NotifyReread) once this pod's own reread
// of it has merged what it read into the live policy: on a session's 200, and on the 503
// REQUEST_ENDED of a reread whose request ended during its value read alone
// (policy.ErrValueUnchecked), which serves the tags it read here as a 200 would; and once a
// reread run after its answer (rereadAfterAnswer) has done the same. A reread that merged nothing
// - a name no secret can carry, a request that ended before or during the metadata read, a store
// that failed, or the 429 refuseReread wrote before any work - is nothing for a peer to copy, and
// neither calls it for any of them. ctx is the context the reread ran on, whose values it keeps.
//
// It notifies on every reread that merged, whether or not the broker now serves the name.
// Notifying only when the live answer changed would need RefreshOne to report a change it does
// not report, and would still leave a hole: a pod whose own five-minute reload had already caught
// the retag would answer "unchanged" and tell nobody, leaving its peers stale for up to a reload.
// Notifying always is also the arithmetic RereadOverallFor divides a store's fleet budget by, and
// a reread a notification drives takes no token from any of this package's buckets - the pod that
// served the original call already paid for it - so one accepted reread costs the fleet one store
// read per pod, and the fleet's whole store traffic is one pod's derived rate times the replica
// count squared, which is that budget again.
//
// The fan-out deliberately drops ctx's cancellation, which for a session's call fires as soon as
// the answer is written and which a caller that hangs up mid-reread fires earlier still: the local
// reread has already merged what it read, and this pod serves it, so that caller must not be able
// to stop the other pods from learning of it. It keeps every value on the context, as this
// repository's other detached work does (context.WithoutCancel): only the cancellation is in the
// way. The handler answers as it would whatever the fan-out does, since nothing about it changes
// what this pod now serves.
//
// Only this public route notifies; the call is deliberately not inside policy.Current.RefreshOne.
// RefreshOne's other caller is the miss path in requests/machine_state.go, which is bounded per
// enrollment and per process and already heals a new secret on first use; fanning that out would
// spend every peer a store read for a name that peer may never be asked for.
func (s *server) notifyPeers(ctx context.Context, name string) {
	if s.deps.NotifyReread == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyRereadTimeout)
	defer cancel()
	s.deps.NotifyReread(ctx, name)
}

// notifyRereadTimeout bounds the fan-out one reread sends: a single Postgres NOTIFY on the shared
// pool, which either goes out at once or is waiting for a connection the requests behind it need
// back. It is short because the pod's own reread has already succeeded by the time it runs, and
// every peer's five-minute reload is the backstop for a notification that never goes out.
const notifyRereadTimeout = 5 * time.Second
