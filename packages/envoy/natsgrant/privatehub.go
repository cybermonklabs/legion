package natsgrant

// The names two programs must spell alike for a hub rendered with private-project support. One
// side writes each and the auth service (internal/cipher/natscallout) reads it, so both take it
// from here: PrivateProjectsServerTag, which a hub's renderer (packages/envoy/infra/core) writes
// into the hub's configuration; HubAdmissionAudience, for which gcp:Dispatch projects Dispatch's
// token and which Dispatch's proof client presents; and PrivacyRequiredSuffix, which a listener
// puts at the end of its connections' client names. A package this one imports, internal/bus and
// internal/contracts among them, and natscred, which internal/bus imports, cannot import it back,
// so such a package takes the name from its caller. Nothing reads them yet: the auth service's
// private-hub admission, the hub writer's tag, Dispatch's projection and proof client, and the
// listener's required mode are later work.
const (
	// PrivateProjectsServerTag is the server_tags entry a hub's nats-server configuration carries
	// when, and only when, the hub is rendered with private-project support. nats-server copies a
	// server's tags into every authorization request it sends the auth service
	// (server/auth_callout.go, claim.Server.Tags), lowercased and trimmed as it reads them (jwt's
	// TagList.Add), so this value is lowercase and has no space to trim. The hub writer renders it,
	// and the auth service decides its private-hub admission rules on it. A hub rendered private
	// whose tags, so normalized, do not hold it, the entry missing or misspelled, reads to the auth
	// service as one without private-project support: Dispatch's hub-admission proof fails there,
	// and the auth service refuses there the credentials that only a hub rendered private may
	// admit, Dispatch's own included once a project is private.
	PrivateProjectsServerTag = "envoy-private-projects"

	// HubAdmissionAudience is the audience of the projected Kubernetes ServiceAccount token
	// Dispatch presents on its hub-admission proof connection: gcp:Dispatch projects it into the
	// envoy-dispatch container alone. The auth service admits a token of this audience only for
	// Dispatch's own account (its dispatch entry of BROKER_NATS_SERVICES), and only on a request
	// carrying PrivateProjectsServerTag. It must differ from BROKER_NATS_K8S_AUDIENCE, at which the
	// auth service admits a service's Kubernetes token on every hub (PlatformTokens): a proof token
	// projected for that audience is admitted as an ordinary service connection whether or not the
	// hub carries PrivateProjectsServerTag, and one projected for an audience the auth service
	// takes no NATS token at is refused on every hub.
	HubAdmissionAudience = "envoy-hub-admission"

	// PrivacyRequiredSuffix is the suffix a listener built in required privacy mode puts at the end
	// of the NATS client name of every connection its credential opens; its leading space keeps it
	// a word of its own. That is each of the listener's bus clients (the stream client, the role
	// lane and the session consumers, all three of which internal/bus names today) and the renewal
	// probe natscred dials with a later token, whose name must keep the suffix last: today's probe
	// name, the connection's name then " probe", does not, and changes before the auth service
	// applies this rule. internal/bus and natscred cannot import this package, so the listener
	// passes the suffix in. nats-server copies a client name unchanged into the authorization
	// request (server/auth_callout.go, ConnectOptions.Name in fillConnectOpts and
	// ClientInformation.Name in fillClientInfo). On a request carrying PrivateProjectsServerTag the
	// auth service refuses a connection it would answer with a listener's grant, Dispatch's
	// listener's included, whose client name does not end with this exact suffix. The name is the
	// client's own claim: it keeps an older or all-public listener build off a private hub, and
	// proves nothing about who holds a listener's credential.
	PrivacyRequiredSuffix = " privacy-required"
)
