// packages/envoy/internal/cipher/config/config.go
// Package config reads the broker's BROKER_* environment. A missing required variable, an
// unreadable or empty startup credential file, a value out of range or of the wrong form, or a
// removed variable refuses to start naming the variable.
//
// Each Config field's doc comment opens with the variables it reads and a colon; the broker's
// generated configuration reference (cmd/cipher-refgen) is built from those comments
// and refuses a variable Load reads that no field documents.
package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/approval"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/netaddr"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/rulehost"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/redact"
)

// DefaultGoogleOIDCIssuer is the issuer of every Google identity token, the one a machine's
// metadata server has Google sign, and what BROKER_GOOGLE_OIDC_ISSUER is when the environment
// names none. The variable exists because a dev stack has no Google: cmd/dev-oidc-issuer stands
// in for it on a loopback port, so the broker and its machines can be driven end to end on one
// machine (scripts/dev-stack.sh). A deployment leaves it unset and gets Google.
const DefaultGoogleOIDCIssuer = "https://accounts.google.com"

type Config struct {
	// BROKER_LISTEN_ADDR: the host:port the broker listens on.
	ListenAddr string
	// BROKER_METRICS_LISTEN_ADDR: the address the broker's metrics port binds, default
	// 127.0.0.1:13382, which answers GET /metrics in the Prometheus text format and the NATS auth
	// service's liveness at GET /livez (natscallout.Livez), and nothing else.
	// It is a port of its own rather than a route of the API's, so that who may scrape a pod is
	// the deployment's to decide apart from who may call the broker: the API's address is the one
	// the load balancer sends every caller to. It is a third port of the same process, bound and
	// drained with the API's, and a pod that cannot bind it refuses to start.
	MetricsListenAddr string
	// BROKER_DATABASE_URL: the Postgres connection URL, the one variable that says which database
	// the broker uses and how it signs in. Required. The broker applies its own migrations at
	// startup. A URL that names a user and no password (neither in the URL nor from PGPASSWORD or a
	// passfile) and whose host is an Amazon RDS endpoint (one ending in .rds.amazonaws.com) signs in
	// by RDS IAM authentication: each new connection signs in with an auth token the broker mints
	// for that user and host from the AWS SDK's default credentials, in the region AWS_REGION,
	// AWS_DEFAULT_REGION or the shared AWS config names, which need rds-db:connect on the database
	// user. Such a URL must name that one host, with sslmode=verify-full and an sslrootcert file (the
	// broker image ships the RDS CA bundle at /etc/ssl/rds/global-bundle.pem), or the broker refuses
	// to start, since a token is a password to the database for 15 minutes; sslrootcert=system is
	// refused too, since the system trust store holds no RDS CA. Any other URL, one carrying its own
	// password or a passwordless one to a local Postgres that trusts its clients, connects as given.
	// Nothing is substituted into it: a URL naming the removed ${BROKER_DATABASE_PASSWORD}
	// placeholder is refused, as BROKER_DATABASE_PASSWORD itself is.
	DatabaseURL string
	// DatabaseIAM is whether the broker signs in to DatabaseURL with RDS IAM auth tokens: the URL
	// names a user and an RDS endpoint host, pgx finds no password for it, and it verifies that
	// host.
	DatabaseIAM bool
	// BROKER_PUBLIC_URL: the broker's own address as its callers reach it, an absolute http or
	// https URL with no path. Required. Every signed proof and request object names it, so a client's
	// AGENT_SECRETS_URL must be exactly this.
	PublicURL string
	// BROKER_MACHINE_LOGIN_URL: the absolute http(s) URL of the page where a person types a
	// machine login's code (Dispatch's /credentials/machine). Optional. The device grant's
	// verification_uri names it, so a machine shows its person where to go, and with no page to
	// show there is no device grant: the broker composes none of its handlers, POST
	// /v1/oauth/device_authorization is not mounted at all (404), and the authorization server
	// metadata leaves the device code out of grant_types_supported. Everything else the OAuth
	// provider serves — the token endpoint, the RFC 8693 exchange, introspection and revocation
	// — is served either way. Unset is the deployment where machines hold a cloud identity and
	// no person ever types a code.
	MachineLoginURL string
	// BROKER_EDGE_LISTEN_ADDR: the host:port the broker's edge binds, a fourth port of the same
	// process, bound and drained with the API's. Its port is a decimal number, and it is refused
	// when that number is the port of BROKER_LISTEN_ADDR, BROKER_METRICS_LISTEN_ADDR or
	// BROKER_AIRLOCK_LISTEN_ADDR, each read as the bind reads it, so 013382 is 13382. The edge is
	// what a caller outside the deployment's private network reaches through a public load
	// balancer, and it serves three routes and no other: POST /v1/oauth/device_authorization,
	// POST /v1/oauth/token and GET /healthz; any other method on the two login paths is 405. On
	// the two login routes a caller's address is the second-to-last X-Forwarded-For entry, the
	// one the load balancer appends for the client it accepted (a login request with fewer than
	// two is 400 REQUEST_INVALID); the edge's buckets are its own, so a flood there spends
	// nothing a caller of BROKER_LISTEN_ADDR is held to; a machine login naming an address on
	// BROKER_SIGNIN_DOMAIN or a BROKER_SIGNIN_WORKSPACE_DOMAINS domain is 400
	// EDGE_WORKSPACE_LOGIN before any of them is spent; and the token route takes the device
	// code, the refresh token and the trade of the broker's own access token, and answers every
	// other grant 400 unsupported_grant_type. Set together with BROKER_EDGE_URL and
	// BROKER_EDGE_MACHINE_LOGIN_URL, or none of the three: unset, the broker has no edge and binds
	// nothing for one. Needs BROKER_MACHINE_LOGIN_URL, since the device grant is what the edge
	// serves, and BROKER_SIGNIN_DOMAIN, the Workspace it refuses.
	EdgeListenAddr string
	// BROKER_EDGE_URL: the broker's own address as a caller of its edge reaches it, an absolute
	// http or https URL with no path, whose origin is not BROKER_PUBLIC_URL's: the two are
	// compared as a DPoP proof's htu is, the scheme and the host without regard to case and the
	// scheme's default port the same as none, so https://secrets.example:443 is refused beside
	// https://secrets.example. A machine login's request object made at the edge names it as its
	// audience and every DPoP proof sent there names it and the route's path, as one made on
	// BROKER_PUBLIC_URL names that, so neither is spendable at the other. Set with
	// BROKER_EDGE_LISTEN_ADDR.
	EdgeURL string
	// BROKER_EDGE_MACHINE_LOGIN_URL: the absolute http(s) URL of the page a person decides a
	// machine login made at the edge on, which that login's verification_uri names in place of
	// BROKER_MACHINE_LOGIN_URL. Set with BROKER_EDGE_LISTEN_ADDR.
	EdgeMachineLoginURL string
	// BROKER_API_ACCOUNTS: the accounts that call the broker's named-caller routes — the
	// approval routes Dispatch's server calls on a person's behalf, the introspection route
	// a resource server reads one of the broker's access tokens back through, and the grant-log
	// route a quarterly access review reads the broker's audit trail through —
	// whitespace-separated <caller>=<account> entries. An account is a Kubernetes service
	// account, written as the subject a projected token carries
	// (dispatch=system:serviceaccount:example-ns:example-dispatch), or a Google service
	// account, written as google: and the account's address
	// (listener=google:example-listener@example-project.iam.gserviceaccount.com). The two kinds
	// are told apart by the entry's own text and never by guessing at its shape: a Kubernetes
	// subject names its kind already, and the google: tag names the other, so a mistyped
	// address is refused as the kind it was written as rather than read as the other.
	//
	// A Kubernetes caller presents its own projected token, for BROKER_K8S_OIDC_AUDIENCE,
	// verified against BROKER_K8S_OIDC_ISSUER as every other ServiceAccount token the broker
	// takes is, and is looked up by the token's sub; so a Kubernetes entry needs that pair. A
	// Google caller presents its machine's own Google identity token, for
	// BROKER_GOOGLE_OIDC_AUDIENCE — the audience every machine of the deployment already holds
	// one for — verified against BROKER_GOOGLE_OIDC_ISSUER, and is looked up by the token's
	// email; so a Google entry needs BROKER_GOOGLE_OIDC_AUDIENCE. Either token is refused if it
	// lives longer than an hour from its issue, as a service's login token is. A project's
	// default Google accounts and Google-managed service agents are refused here as they are in
	// BROKER_VM_SERVICES, since a token from one proves nothing about which machine is asking.
	//
	// The caller's name decides what the account may do, and the broker
	// knows three: dispatch reaches every one of those routes, the ten Dispatch's server relays
	// a person's decisions and reads through as well as introspection; listener reaches
	// introspection alone, since the Envoy listener reads a token back and relays no decision;
	// and access-review reaches GET /v1/grant-log alone, which answers every audit row of a
	// closed period, so whoever runs the quarterly review reads the whole trail and can decide
	// nothing, release no secret value and write no row. Dispatch's server is deliberately not
	// given that last one: it already holds approval authority over every person's secrets, and
	// the review is a reader, so the two stay separate names and separate accounts. A
	// name the broker does not know refuses the boot naming it and the names it knows, so a typo
	// never quietly produces a caller that reaches nothing, and a new caller is a deliberate
	// edit to the broker's own table (api.AccountFor). A listed caller reaching a route its name
	// does not carry is 403 ACCOUNT_LACKS_POWER, which is a different fact from an account that
	// is not listed at all (403 ACCOUNT_NOT_LISTED). A namespace's default account is refused, as
	// is an account given twice, since each account names one caller. APIAccounts is keyed by
	// ServiceAccount subject and GoogleAPIAccounts by Google account address, each answering its
	// caller and that caller's powers. Unset, no caller reaches those routes.
	APIAccounts       map[string]api.Account
	GoogleAPIAccounts map[string]api.Account
	// BROKER_SIGNIN_ISSUER, BROKER_SIGNIN_CLIENT_ID, BROKER_SIGNIN_CLIENT_SECRET_FILE,
	// BROKER_SIGNIN_DOMAIN, BROKER_SIGNIN_SESSION_SECONDS: the broker's own OpenID Connect client,
	// through which a person signs in to the broker's approval page to approve a secret request:
	// the provider's issuer (an absolute https URL, or http only to a loopback host, for a run on
	// one machine), the client's id, a file holding the client's secret (read at startup and
	// trimmed; no variable carries the secret itself, so BROKER_SIGNIN_CLIENT_SECRET is refused),
	// the Google Workspace domain a person's ID token must name in its hd claim, and how many
	// seconds a sign-in lasts (an absolute expiry nothing extends, at most 86400, 24 hours, the
	// default). The first four are required unless BROKER_SIGNIN is off: a broker that signs
	// people in and is missing any of them refuses to start, naming the ones missing, since
	// nobody could approve a secret request on a page nobody can sign in to. With BROKER_SIGNIN
	// off every one of the five is refused, and SignIn is empty.
	SignIn approval.SignIn
	// BROKER_SIGNIN: unset, or exactly off, which turns the broker's person sign-in off. Unset,
	// the broker signs people in to its approval page through BROKER_SIGNIN_ISSUER,
	// BROKER_SIGNIN_CLIENT_ID, BROKER_SIGNIN_CLIENT_SECRET_FILE and BROKER_SIGNIN_DOMAIN, as it
	// always has. off is a broker that signs nobody in, for a deployment whose way of signing
	// people in is not settled yet: it needs none of those settings and serves no
	// approval page (every route under /approve and /signin/ is a 404), and a request naming a
	// secret whose answer would be a person's approval — a human-tier secret, another person's
	// agent-tier secret, one the session's operator withheld from it — is denied at once, its
	// detail naming this setting, rather than left pending for a person who cannot come. Of the
	// requests a broker with sign-in on left or leaves pending, the next sweep expires the
	// overdue ones first, then denies the rest whose signed record verifies for this broker. What
	// the policy grants at once (a person's own or a shared agent-tier secret for their sessions,
	// a service's own secret for its pods) is granted as with sign-in on, and a machine login,
	// which a person decides on Dispatch's page, is unchanged. off refuses to start beside any
	// BROKER_SIGNIN_* setting, naming the ones set, and beside the model judge, which decides
	// only a request that waits for a person; any other value refuses naming it.
	SignInOff bool
	// BROKER_SIGNIN_WORKSPACE_DOMAINS: the other domains the Google Workspace owns beside
	// BROKER_SIGNIN_DOMAIN, separated by commas, each a lowercase domain name; an
	// internationalized one is written in Unicode or as its A-labels, and is held as its A-labels.
	// The broker's edge (BROKER_EDGE_LISTEN_ADDR) refuses a machine login naming an address on any
	// of them or on a subdomain of one, as it refuses one on BROKER_SIGNIN_DOMAIN, and serves no
	// token of a machine login whose person is on one, so nobody on the internet can name or use a
	// person of the Workspace there; nothing else reads it. Unset, the Workspace is
	// BROKER_SIGNIN_DOMAIN alone. An empty entry, or one that is no lowercase domain name, refuses
	// the start naming its position, and BROKER_SIGNIN=off refuses it, since that broker names no
	// Workspace.
	SignInWorkspaceDomains []string
	// BROKER_SIGNIN_DEVICE_AUDIENCE: the audience a person's own Google identity token must
	// carry to be traded at the RFC 8693 token exchange, where such a token is a subject token
	// (urn:envoy:params:oauth:token-type:person-google-identity) and the machine the person
	// signed in on presents its own as the actor token
	// (urn:envoy:params:oauth:token-type:machine-google-identity). It is the OAuth client id of
	// the device-flow client `agent-secrets login` runs Google's device authorization grant as,
	// since Google puts the client's id in an ID token's aud. Its issuer is
	// BROKER_GOOGLE_OIDC_ISSUER, Google's own unless a dev stack says otherwise, and the person
	// it names is held to BROKER_SIGNIN_DOMAIN through the hd claim, exactly as a person
	// signing in to the broker's own approval page is. Needs BROKER_GOOGLE_OIDC_AUDIENCE and
	// BROKER_VM_SERVICES: the listed machines are the boxes a person may sign in on. Unset, the
	// broker accepts no person's sign-in and registers neither token type, so a caller presenting
	// either is refused by name. It is not one of the four SignInSettings, which are required
	// together: those are what a person approves a secret request through and no broker that
	// signs people in serves without them, where this one is optional, as the whole Google
	// exchange is. Beside BROKER_SIGNIN=off it is refused with them, since that broker holds no
	// BROKER_SIGNIN_DOMAIN to hold a person to.
	SignInDeviceAudience string
	// BROKER_SECRETS_STORE: which secret store the agent secrets live in: aws-secrets-manager (the
	// default when unset, AWS Secrets Manager with the agent-secrets key in AWS KMS) or
	// gcp-secret-manager (Google Cloud Secret Manager with the agent-secrets key in Cloud KMS, read
	// under Application Default Credentials: on GKE, Workload Identity for the broker's
	// ServiceAccount, with no key file). It decides BROKER_SECRETS_PREFIX's form and which key
	// variable is required.
	SecretsStore string
	// BROKER_SECRETS_PREFIX: the namespace of the agent secrets. Required. With
	// aws-secrets-manager, a Secrets Manager name prefix ending in "/" (for example
	// production/agent-secrets/); with gcp-secret-manager,
	// projects/<project number>/secrets/<secret-id prefix>- (for example
	// projects/000000000000/secrets/agent-secrets-), the project by its number, since Secret
	// Manager answers every resource name with the number. The broker serves every secret under
	// it that carries an owner and a tier tag (Secret Manager: annotation) and is encrypted with
	// the agent-secrets key, and rereads them every five minutes.
	SecretsPrefix string
	// BROKER_SECRETS_KMS_KEY_ARN, BROKER_SECRETS_KMS_KEY_NAME: the agent-secrets key every agent
	// secret is encrypted with, one variable per store, each required with its store and refused
	// with the other. With aws-secrets-manager, BROKER_SECRETS_KMS_KEY_ARN, the AWS KMS key's ARN;
	// with gcp-secret-manager, BROKER_SECRETS_KMS_KEY_NAME, the Cloud KMS key's resource name
	// projects/<project>/locations/global/keyRings/<ring>/cryptoKeys/<key> (global, since a secret
	// with automatic replication takes a global key). A secret under the prefix encrypted with any
	// other key, the provider's default one included, is refused.
	SecretsKMSKey string
	// BROKER_AIRLOCK_RULES_FILE: the path of the airlock's rules file, airlock.json, which says
	// for each secret which hosts its value may reach and in which headers. The broker reads it
	// once at start, so a change to it rolls the pods; it refuses to start on a file it cannot
	// read, cannot parse, or whose entries it refuses, naming the file and the entry. A secret
	// the file names is never released as a value: a session is given a placeholder, and the
	// broker swaps the real value into the headers the entry allows. Empty (the default) is a
	// deployment with no airlock, where every granted secret is released as its value.
	AirlockRulesFile string
	// BROKER_AIRLOCK_LISTEN_ADDR: the address the airlock's proxy port binds, default
	// 127.0.0.1:13381. It is a second port of the same process, so it is bound and drained with
	// the API's. Read only when BROKER_AIRLOCK_RULES_FILE names a file; a deployment with no
	// airlock binds no proxy port.
	AirlockListenAddr string
	// BROKER_AIRLOCK_PROXY_URL: the URL a command reaches the airlock's proxy port at, which
	// every release of a name behind the airlock carries, so the command points its HTTPS_PROXY
	// at it. Required with BROKER_AIRLOCK_RULES_FILE, and refused at start unless it is an
	// absolute http or https URL with a host: a value with no scheme or no host would boot and
	// then fail every airlock run one command at a time. It is a setting of its own rather than
	// BROKER_PUBLIC_URL with another port, because the Service port a pod dials need not be the
	// container port the broker binds.
	AirlockProxyURL string
	// BROKER_AIRLOCK_CA_CERT_FILE: the PEM certificate of the airlock proxy's certificate
	// authority, the one every release hands the command to trust. Required with
	// BROKER_AIRLOCK_RULES_FILE, and read once at start as half of that authority, with
	// BROKER_AIRLOCK_CA_KEY_FILE; a pair the authority refuses refuses the start. What a release
	// carries is the authority's own certificate rather than this file's bytes, so a file that
	// holds more than the certificate hands a command nothing more. Both broker pods mount the
	// same file from one Kubernetes Secret, so a command's trust does not depend on which pod
	// released its values.
	AirlockCACertFile string
	// BROKER_AIRLOCK_CA_KEY_FILE: the PEM private key of BROKER_AIRLOCK_CA_CERT_FILE's
	// certificate authority, which the proxy port signs each host's certificate with. Required
	// with BROKER_AIRLOCK_RULES_FILE, and mounted from the same Kubernetes Secret, which no
	// container but the broker's mounts: it is the key that lets its holder be any host a
	// command trusts.
	AirlockCAKeyFile string
	// BROKER_AIRLOCK_IDLE_SECONDS: how long one proxy connection may go with no byte in either
	// direction before the proxy closes it, default 300, between 10 and 3600. A connection
	// carries a grant's placeholders, so an abandoned one is state the broker holds for a
	// command that is gone; a short bound costs a long-polling client its connection, which it
	// opens again.
	AirlockIdleSeconds int
	// BROKER_AIRLOCK_CLUSTER_RANGES: the broker cluster's own Service range, and each of its
	// pod and node ranges that RFC 1918 does not already make private, as a comma-separated list
	// of CIDR prefixes, IPv4 or IPv6, such as 34.118.224.0/20. The airlock proxy refuses to dial
	// an address inside any of them, as it refuses a private one: a GKE Service range is global
	// unicast, so without this a command under an airlock grant could tunnel to a ClusterIP of the
	// cluster the broker runs in. Each prefix is read as its network (34.118.230.7/20 is
	// 34.118.224.0/20); an empty element, a prefix that does not parse and an IPv4-mapped IPv6
	// prefix refuse the start, naming the element's position. With BROKER_AIRLOCK_RULES_FILE set
	// and KUBERNETES_SERVICE_HOST set to a non-empty value, which Kubernetes sets in every
	// container to the address of the cluster's kubernetes Service, it is required, and the start
	// is refused unless KUBERNETES_SERVICE_HOST is an IP address inside one of the ranges, so a
	// list that misses the cluster's Service range cannot pass silently. Outside a cluster
	// (KUBERNETES_SERVICE_HOST empty or unset) it is optional.
	AirlockClusterRanges []netip.Prefix
	// BROKER_AIRLOCK_TEST_GITHUB_API_BASE: the base URL a github-app secret is minted against,
	// in place of https://api.github.com, so a release of one mints at a stand-in GitHub
	// (internal/cipher/proxyrules/proxyrulestest, cmd/dev-proxy-stands). For the broker's local
	// stack alone: a production start, which is one with BROKER_FAKE_SECRETS_FILE unset, is
	// refused naming it.
	AirlockTestGitHubAPIBase string
	// BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL: the token endpoint a google-login secret is
	// refreshed at, in place of https://oauth2.googleapis.com/token, so a release of one mints
	// at a stand-in Google. For the broker's local stack alone: a production start, which is
	// one with BROKER_FAKE_SECRETS_FILE unset, is refused naming it.
	AirlockTestGoogleTokenURL string
	// BROKER_AIRLOCK_TEST_DIAL_ALLOW: whitespace-separated <host>:<port> addresses the airlock
	// proxy may dial although the address they resolve to is one it never dials - a loopback,
	// cluster, link-local or private address. It is a list rather than a switch for loopback
	// because a local stack needs both at once: its stand-in services on loopback reachable,
	// and the broker's own loopback port still refused. For the broker's local stack alone: a
	// production start, which is one with BROKER_FAKE_SECRETS_FILE unset, is refused naming
	// it.
	AirlockTestDialAllow []string
	// BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE: a PEM file of extra roots the airlock proxy
	// verifies a service's certificate against on its own calls upstream, beside the host's
	// own roots, so it trusts a stand-in service's certificate. Read once at start, and a file
	// it cannot read or that holds no certificate refuses the start. For the broker's local
	// stack alone: a production start, which is one with BROKER_FAKE_SECRETS_FILE unset, is
	// refused naming it.
	AirlockTestUpstreamCAFile string
	// BROKER_AIRLOCK_TEST_RESOLVE: whitespace-separated <host>=<address> entries, each an IP
	// address the airlock proxy reads that host as having and dials it at, in place of what
	// the resolver answers. It is how a client that resolves a real name of its own - az, the
	// AWS client - reaches a stand-in service through the proxy. A host named twice is
	// refused. For the broker's local stack alone: a production start, which is one with
	// BROKER_FAKE_SECRETS_FILE unset, is refused naming it.
	AirlockTestResolve map[string]netip.Addr
	// BROKER_AIRLOCK_TEST_KEEP_VALUES: with it true, the airlock proxy keeps a grant's values
	// across the reread of one of its names and past the five-minute margin before a minted
	// token's expiry, rather than reading and minting again. It is the negative control of the
	// check that both halves belong to. For the broker's local stack alone: a production
	// start, which is one with BROKER_FAKE_SECRETS_FILE unset, is refused naming it.
	AirlockTestKeepValues bool
	// BROKER_AIRLOCK_TEST_NO_KIND: with it true, the airlock proxy reads every rules entry as
	// having no kind, so an azure entry's exchange and an aws entry's signing are off and each
	// takes the plain header swap. It is the negative control of the check those two kinds
	// belong to. For the broker's local stack alone: a production start, which is one with
	// BROKER_FAKE_SECRETS_FILE unset, is refused naming it.
	AirlockTestNoKind bool
	// BROKER_AIRLOCK_TEST_AZURE_AUTHORITY: the host of the token authority the airlock proxy
	// makes every exchange of an azure entry's federated token at, in place of
	// login.microsoftonline.com, so the exchange is made at a stand-in Microsoft. A host name an
	// airlock rules entry can bind, in lower case and with no "*."; anything else refuses the
	// start. For the broker's local stack alone: a production start, which is one with
	// BROKER_FAKE_SECRETS_FILE unset, is refused naming it.
	AirlockTestAzureAuthority string
	// BROKER_JUDGE_MODEL_URL: the base URL the broker's model judge calls its model at, an
	// absolute http(s) URL. Required with the rest of JudgeSettings, which are all-or-none:
	// with none of the five set there is no judge, and every human-tier request waits for the
	// person who decides it today.
	JudgeModelURL string
	// BROKER_JUDGE_MODEL: the id of the model the judge asks, as the endpoint at
	// BROKER_JUDGE_MODEL_URL names it. Required with the rest of JudgeSettings.
	JudgeModel string
	// BROKER_JUDGE_MODEL_PROTOCOL: the wire protocol that endpoint speaks, one of
	// anthropic-messages (the default), openai-responses, openai-chat and azure-anthropic,
	// which are agentloop's four Run kinds; any other value refuses the start naming the
	// setting, the value and the four it takes. It is the one judge setting with a default, so
	// it is never among the ones a half-configured judge is refused for; and because it says
	// nothing on its own, setting it with the other five unset is refused as the same mistake,
	// naming the five a judge needs, rather than read as a deployment that meant no judge.
	JudgeModelProtocol string
	// BROKER_JUDGE_MODEL_AUDIENCE: the audience of the Google identity token the broker
	// presents to the endpoint at BROKER_JUDGE_MODEL_URL, minted for the broker's own machine
	// identity, or for the service account BROKER_JUDGE_MODEL_ACCOUNT names when that is set.
	// Required with the rest of JudgeSettings.
	JudgeModelAudience string
	// BROKER_JUDGE_MODEL_ACCOUNT: optional, the Google service account, by its email, whose
	// identity token the judge presents to the endpoint at BROKER_JUDGE_MODEL_URL. With it set,
	// the broker mints that account's token for BROKER_JUDGE_MODEL_AUDIENCE at IAM Service
	// Account Credentials' generateIdToken, with includeEmail, spending the pod's own credential,
	// the access token its metadata server answers; it never reads the metadata server's identity
	// token and never falls back to it, and a mint that fails hands the request to its person with
	// an error naming the account and the audience. Unset, the judge reads the metadata server's
	// identity token as before. It is for a pod that runs as a bare Workload Identity principal,
	// whose metadata server mints no identity token. The broker's principal needs
	// roles/iam.serviceAccountOpenIdTokenCreator on that account, and the account's project needs
	// the IAM Service Account Credentials API (iamcredentials.googleapis.com) turned on.
	//
	// It is not one of JudgeSettings, so the five set without it start the judge as before. Set
	// while none of the five is, it names a judge that was never configured, and the start is
	// refused as a half-configured judge's is. Its value is one user-managed Google service
	// account, <account id>@<project id>.iam.gserviceaccount.com in lowercase, held to
	// secrets.ValidServiceAccountEmail, and a Google-managed service agent is refused, as it is in
	// BROKER_SERVICE_ACCOUNT_SECRETS: Google's own services run as those accounts and the broker
	// must not be able to mint their tokens. Each refusal names the setting and the value.
	JudgeModelAccount string
	// BROKER_JUDGE_DISPATCH_URL: the base URL of the Dispatch the judge reads an issue from, an
	// absolute http(s) URL. It makes GETs there — one for the issue a request's reason names,
	// which answers that issue's key, title and status, one for the whole markdown of the issue's
	// spec document, and one for the ask the reason cites — and writes nothing: no ask, no
	// comment, no status. The broker also reads one issue there for a request a Legion pod makes
	// for a shared human-tier secret: the issue its claim names, whose assignee alone is asked to
	// decide it in their Dispatch Inbox. Required with the rest of JudgeSettings.
	JudgeDispatchURL string
	// BROKER_JUDGE_DISPATCH_TOKEN_FILE: the file holding the broker's own projected
	// ServiceAccount token for the Dispatch at BROKER_JUDGE_DISPATCH_URL. Read when the judge
	// calls Dispatch and not at boot, through the reader every other projected token in this
	// binary goes through (ciphertoken.AccountToken), which reads the file again once what it
	// last read is 30 seconds old, so a rotation reaches the judge without a restart.
	// Required with the rest of JudgeSettings.
	JudgeDispatchTokenFile string
	// BROKER_JUDGE_TEST_SKIP_CHECKS: with it true, the judge's checks before the model are off,
	// so a request one of them would hold back is asked about anyway. It is the negative
	// control of the check the judge belongs to. For the broker's local stack alone: a
	// production start, which is one with BROKER_FAKE_SECRETS_FILE unset, is refused naming it.
	JudgeTestSkipChecks bool
	// BROKER_PROJECT_ACCESS_URL: the base URL of the Dispatch the broker asks whether a project
	// is private, whether a person may see it, and what kind of member a person is
	// (internal/cipher/projectaccess), an absolute http(s) URL, read without the slashes it ends
	// in. Set with BROKER_PROJECT_ACCESS_TOKEN_FILE or not at all: with neither, the broker asks
	// Dispatch nothing. A token that names no project, and an operator entry's token, whose
	// project that operator's own setting chose, are minted as before; a person's trade that
	// names a project is refused, since nothing could say that person may see it.
	ProjectAccessURL string
	// BROKER_PROJECT_ACCESS_TOKEN_FILE: the file holding the broker's own projected
	// ServiceAccount token for that Dispatch, minted for Dispatch's audience. Read when the broker
	// asks and not at boot, through ciphertoken.AccountToken, which reads the file again once what
	// it last read is 30 seconds old. Set with BROKER_PROJECT_ACCESS_URL or not at all.
	ProjectAccessTokenFile string
	// BROKER_AZURE_TOKEN_FILE: the path of the broker pod's projected ServiceAccount token for
	// azure-login releases, default /var/run/secrets/azure/tokens/azure-identity-token. Read only
	// on release, not at boot, and read anew each time so the rotated token is used. The token's
	// exp claim supplies the released expires_at; an unreadable, malformed or expired token
	// refuses that release. Mount the projection read-only in the broker container alone, with
	// audience api://AzureADTokenExchange and a one-hour lifetime. Only one Entra identity may
	// trust this ServiceAccount subject: a released token can sign in to any identity that does.
	AzureTokenFile string
	// BROKER_AZURE_LOGIN_SECRET: the one agent-secret name allowed to release the broker's Azure
	// projected token, for example AZURE_LOGIN. Empty (the default) disables azure-login
	// releases. Must be an uppercase agent-secret name, not its store id or slug. That secret
	// must be human tier in the policy snapshot checked for each release, with a person's
	// approval recorded for that name in its grant. Another secret with type=azure-login,
	// the named secret retagged to agent tier, or an automatic grant releases no token.
	AzureLoginSecret string
	// BROKER_SERVICE_ACCOUNT_SECRETS: the one Google service account each google-service-account
	// agent secret mints a token for, whitespace-separated <SECRET NAME>=<account email>
	// entries, such as GCP_ADMIN=gcp-admin@example-project.iam.gserviceaccount.com. The name is
	// the uppercase agent-secret name a session asks for (DEV_PERSON, GCP_ADMIN), not that
	// secret's store id or its slug, and each secret is named at most once; the account is a
	// user-managed Google service account, <account id>@<project id>.iam.gserviceaccount.com in
	// lowercase, held to secrets.ValidServiceAccountEmail, the minter's own rule, so no binding
	// can ever name an address the minter would refuse. A Google-managed service agent is
	// refused here as it is in BROKER_VM_SERVICES: Google's own services run as those accounts
	// and the broker must not be able to impersonate one. One account may be bound to two
	// secrets, since only the secret's name is the key.
	//
	// This setting, and not a secret's stored value, is what chooses the account a
	// google-service-account secret mints for (secrets.GoogleServiceAccount.Accounts, which the
	// broker fills from it): the broker holds roles/iam.serviceAccountTokenCreator on every
	// account bound here, so a stored value alone choosing the account would let whoever may
	// create an agent secret store another person's service account in a secret of their own,
	// list themselves among its requesters and mint that person's token. A mint happens only
	// where this binding and the stored value name the same account; either missing or the two
	// disagreeing is refused before any call reaches Google.
	//
	// Unset, no secret is bound and every google-service-account mint is refused with
	// secrets.ErrMintNotBound, logged class=binding. That is the window a deployment is in
	// between this build and the apply that writes its bindings, and it is deliberately the
	// safe side of the change: a broker that went on minting on the stored value alone until
	// the setting arrived is the very thing the setting exists to stop. Nothing else needs it:
	// a broker with no binding still boots and still serves every other type. The policy loader
	// reads it as well (policy.Loader.ServiceAccounts), for one rule — a secret bound to a
	// dev-<name> account is one person's own, so it may list no registered service among its
	// requesters.
	ServiceAccountSecrets map[string]string
	// BROKER_SERVICES: the registered services, whitespace-separated name=<service account>
	// entries, such as legion-daemon=system:serviceaccount:legion:legion-worker. A name (lowercase
	// letters, digits and hyphens, at most 64, not shared) is what a secret's owner tag may name; the
	// service account (system:serviceaccount:<namespace>:<name>) is one the service's pods run as. A
	// service may be named for several accounts, one entry each, but each account is given once and
	// proves exactly one service. A session is the service's when a launcher logged in as the
	// service enrolled it and it is a pod whose projected token proved it runs as one of that
	// service's accounts. A machine login's service name is the machine's own claim, approved by
	// whoever its login names, so the pod's verified service account is what proves the service,
	// and an account bound to two names would prove neither. A namespace's default account
	// (system:serviceaccount:<namespace>:default) is refused: every pod in the namespace whose spec
	// names no account runs as it and can mount a token for the broker's audience, so it proves
	// nothing about which pods are the service's. Keyed by account, each answering its service. A
	// secret owned by a listed service goes at once to those pods and is refused to every other
	// session. Unset, no service is registered, and a secret whose owner tag names one is refused
	// as owner-tag-malformed.
	ServicesByAccount map[string]string
	// BROKER_SERVICE_LOGINS: the ServiceAccounts that log in as a service with their own projected
	// token (POST /v1/launcher-credentials/token), and that authenticate the routes a service
	// calls as itself (serviceAuth: POST /v1/oauth/exchange/pod-sessions, where a deployment's
	// daemon registers a pod it starts under a session name, and POST
	// /v1/oauth/exchange/pod-sessions/release, where it releases the names it registered). A
	// login authenticates the caller of those routes and is not what lets it register a pod:
	// BROKER_POD_REGISTRARS carries the registration's authority, since a session pod logs in
	// here too. Whitespace-separated <service>=<service account> entries, such as
	// legion-daemon=system:serviceaccount:legion-system:legion-daemon-legion. Each account
	// (system:serviceaccount:<namespace>:<name>, named exactly: there is no wildcard) logs in
	// as the one service its entry names and is given once; one service may have several accounts,
	// one per machine. A namespace's default account is refused, since every pod there that names
	// no account runs as it. An account BROKER_SERVICES binds for a service's pods may be listed
	// here as well, which is a pod of that account enrolling itself: every such pod holds a token
	// for the account, so each of them logs the service in, and what keeps one of them from
	// enrolling another is enroll.Service.podUnderTokenLogin, which gives such a login the one pod
	// whose token made it and answers ErrPodSelfEnrollOnly for any other pod UID. Keyed by account,
	// each answering its service. A token whose subject is not listed is refused, and a credential
	// an account's login issued stops authenticating once its entry is gone. Needs
	// BROKER_K8S_OIDC_ISSUER, whose tokens it trusts. Unset, no service logs in with a token.
	//
	// A login of an account that BROKER_POD_OPERATORS maps to a person acts for that person, when
	// the login token is bound to a pod of the account. No other login of this setting acts for a
	// person: the credential of each other login has no operator. A mapped account is held to
	// rules that no other account of this setting is held to, and PodOperators lists them. Among
	// them are these two. BROKER_SERVICES must not bind a mapped account, though the paragraph
	// above lets BROKER_SERVICES bind any other account listed here. A mapped account
	// authenticates none of the routes a service calls as itself: serviceAuth refuses it
	// 403 PERSON_ACCOUNT.
	ServiceLogins map[string]string
	// BROKER_POD_REGISTRARS: who may register which pods at POST /v1/oauth/exchange/pod-sessions,
	// whitespace-separated <registrar account>=<pod account> entries, such as
	// system:serviceaccount:legion-system:legion-daemon-legion=system:serviceaccount:legion:legion-worker,
	// one entry per pair. Each account is a ServiceAccount subject
	// (system:serviceaccount:<namespace>:<name>, named exactly), the rule BROKER_SERVICE_LOGINS
	// holds its accounts to: the registrar is the account the caller logs in as on that route,
	// and the pod account the subject of the pod token the registration's body carries. The
	// route admits a registration only where an entry pairs the two, and refuses every other one
	// 403 POD_REGISTRAR_NOT_LISTED, naming both accounts. This list, and not BROKER_SERVICE_LOGINS,
	// is the registration's authority: a session pod logs in as a service too, and must not
	// register a pod whose token it holds for a session name of its choosing. A registrar may be
	// paired with several pod accounts and a pod account with several registrars; an account
	// paired with itself is a daemon registering the sibling pods of its own account, and the
	// route still refuses it the pod it runs in (403 POD_CALLER_IS_THE_POD). Refused at start,
	// naming the setting and the entry: an entry without exactly one "=", an account of any other
	// form (a google: one included), a namespace's default account, which every pod there whose
	// spec names no account runs as, and a pair given twice; the list set without
	// BROKER_K8S_OIDC_ISSUER, whose tokens both accounts present; and a registrar that
	// BROKER_POD_OPERATORS maps to a person (mappedAccountRefusal). A registrar
	// BROKER_SERVICE_LOGINS does not list is not refused, since the list may be derived from another source than the
	// logins and a refusal would turn that into an outage: it registers nothing, and the boot
	// logs a WARN line naming it. Unset, the broker starts and the route admits no registration,
	// so a deployment sets this list before its first daemon sets exchange_audience. The token
	// exchange does not read it: a registration written while its entry stood is honoured until
	// it lapses, so taking an entry out stops that registrar's new registrations at once and its
	// old ones within 24 hours.
	PodRegistrars api.PodRegistrars
	// BROKER_POD_REGISTRATION_AUDIENCE: an audience POST /v1/oauth/exchange/pod-sessions accepts
	// for the pod token a registration's body carries, beside the broker's own
	// BROKER_K8S_OIDC_AUDIENCE, which a daemon presents today: a token of either audience,
	// verified against BROKER_K8S_OIDC_ISSUER, which this needs, names the pod, and an expired
	// one of either is 401 POD_TOKEN_EXPIRED. No other route accepts a token of this audience:
	// enrolment and every ServiceAccount login verify at the broker's own audience, the token
	// exchange at BROKER_EXCHANGE_K8S_AUDIENCE and the NATS auth service at
	// BROKER_NATS_K8S_AUDIENCE, so a registrar may be handed a pod's token that registers the pod
	// and is spendable nowhere else. Read trimmed. Refused at start, naming it and the other
	// setting, when it equals any of those three, each compared when it is set, since a token of
	// it would then be spendable on that setting's routes too. Unset, the route accepts the
	// broker's own audience alone.
	PodRegistrationAudience string
	// BROKER_POD_OPERATORS: the Kubernetes accounts whose pods act for a person,
	// whitespace-separated <email>=system:serviceaccount:<namespace>:<name> entries, such as
	// ada@example.com=system:serviceaccount:example-ns:ada. The email is the person, in lower
	// case, as the person signs in (record.CanonicalLogin). A mapped account is an account that
	// this setting names. A person may have several mapped accounts. Keyed by account, each
	// answering its person.
	//
	// A pod of a mapped account logs in with its own projected token, bound to that pod, and
	// enrols itself (agent-secrets enroll --pod --service <its login service>) under no session
	// name. That pod acts as the person at the broker, on the terms of the person's dev box. The
	// broker serves the person's own agent-tier secrets to the pod with no approval. A request for
	// one of the person's human-tier secrets waits for the person to approve each grant. A request
	// for another person's secret waits for its owner to approve it. Each mapped account logs in
	// under a login service of its own, which BROKER_SERVICE_LOGINS names for that account. A token
	// of a mapped account that is bound to no pod, such as one kubectl create token mints, logs in
	// for no person. An account that this setting does not name acts for no person at the broker,
	// also when BROKER_EXCHANGE_ACCOUNTS maps it to a person for Dispatch.
	//
	// The operator routes that list and revoke the person's machine logins and grants refuse such a
	// pod's login 403 PERSON_POD_LOGIN. The routes a service calls as itself refuse the mapped
	// account 403 PERSON_ACCOUNT. The broker reads this setting at each decision of what
	// the pod may do. So after the deployment removes an entry, or moves it to another person, the
	// enrollment that a pod of that account already holds acts for no person from its next call.
	//
	// The broker refuses to start in each case below, and the refusal names the entry or the
	// account. It refuses an entry that is not of the form above. It refuses a person that is not a
	// lower-case email. It refuses an account that is not system:serviceaccount:<namespace>:<name>.
	// It refuses a namespace's default account, because every pod of the namespace that names no
	// account runs as it. It refuses an account given twice, because one account acts for one
	// person. It refuses a mapped account that BROKER_SERVICE_LOGINS does not name. It refuses a
	// mapped account whose login service a BROKER_SERVICES entry names, because a secret could then
	// name that service as its owner. It refuses a mapped account that a BROKER_SERVICES entry
	// binds, because a launcher logged in as that service could then enrol the person's pods as its
	// own workers, with that service's secrets. It refuses a mapped account whose login service a
	// BROKER_VM_SERVICES entry names, because the broker registers the services of BROKER_SERVICES
	// and BROKER_VM_SERVICES alike as owners a secret may name. It refuses a mapped account that a
	// BROKER_EXCHANGE_ACCOUNTS entry maps to another person. It refuses a mapped account that a
	// BROKER_POD_REGISTRARS entry names as its registrar, because the person's pod would then
	// choose the session names other pods exchange under. It refuses a mapped account that a
	// BROKER_EXCHANGE_READ_ACCOUNTS entry names, because the exchange would mint the person's pod a
	// token that names a service and no person. It refuses a mapped account that a
	// BROKER_API_ACCOUNTS entry names, because that caller's routes relay decisions in the name the
	// caller gives. It refuses a mapped account that a BROKER_NATS_SERVICES entry names, because
	// the NATS auth service would then answer a connection of the person's pod with a service's
	// NATS grant. It refuses a mapped account that either side of a BROKER_SESSION_ACCOUNT_PAIRS
	// pair names, because a pair lets the session names the token exchange binds for the account
	// and for the other account cross. Unset, the setting maps no account.
	PodOperators map[string]string
	// BROKER_SESSION_ACCOUNT_PAIRS: pairs of Kubernetes service accounts that take each other's
	// session names, whitespace-separated <account>=<account> entries, each account
	// system:serviceaccount:<namespace>:<name> as BROKER_SERVICES writes one, such as
	// system:serviceaccount:example-ns:example-worker=system:serviceaccount:example-ns:example-project-worker.
	// A session name belongs to the principal that claimed it, and a pod's principal is the
	// account its projected token proves, so a pod moved to another account is refused the name
	// its role holds (oauthstore.Store.BindSession). Either account of a pair takes a session name
	// the other holds, by an enrollment or by a token exchange, and an enrollment under that name
	// takes what the name carries: the grants, the pending requests and the withheld secrets the
	// other account's enrollments of it hold that a login of the same account enrolled, since a
	// pod's grants follow its account and its launcher's (enroll.carryPrincipal), so move the pods'
	// account and keep the launcher's. A pair works both ways, so moving the pods back needs no
	// change here. A pair does not chain: with A=B and B=C, C does not take a name A holds. A
	// name the configuration fixes for a principal, the session an entry of
	// BROKER_EXCHANGE_ACCOUNTS, BROKER_EXCHANGE_READ_ACCOUNTS, BROKER_EXCHANGE_WORKFLOWS or
	// BROKER_VM_EXCHANGE_ACCOUNTS names, is never taken. An account may be in several pairs. An
	// entry not of that form, a namespace's default account, an account paired with itself and a
	// pair given twice, in either order, are refused, and so is an account BROKER_POD_OPERATORS
	// maps to a person, on either side, since a person's account shares no session name with
	// another account. Each pair is held as its two accounts, in the order the setting gives them.
	//
	// It is for an operator moving a deployment's pods from one account to another, and it must
	// be live on the broker before the deployment moves its pods to the new account: until then
	// each live role's new pod is refused its own session name (403 SESSION_HELD), and the role's
	// grants, pending requests and withheld secrets stay behind. While a pair stands, a pod of
	// either account can claim any name the other holds, not only its own role's, so remove the
	// pair once no live pod runs as the old account. Unset, no account is paired.
	SessionAccountPairs [][2]string
	// BROKER_K8S_OIDC_ISSUER: the issuer of the Kubernetes service-account tokens pods enroll
	// with. Set it with BROKER_K8S_OIDC_AUDIENCE, or neither, in which case no pod can enroll.
	K8sOIDCIssuer string
	// BROKER_K8S_OIDC_AUDIENCE: the audience a pod's projected service-account token must carry.
	K8sOIDCAudience string
	// BROKER_GOOGLE_OIDC_ISSUER: the issuer of the Google identity tokens machines present,
	// https://accounts.google.com when unset, which is where every real one comes from. It is a
	// variable because a dev stack has no Google: cmd/dev-oidc-issuer stands in for it on a
	// loopback port, so a broker, its machines and Dispatch can be driven end to end on one
	// machine with no cloud. Read only where BROKER_GOOGLE_OIDC_AUDIENCE is set, which is what
	// turns Google identity tokens on at all.
	GoogleOIDCIssuer string
	// BROKER_GOOGLE_OIDC_AUDIENCE: the audience a machine's Google identity token must carry,
	// both for a caller listed in BROKER_API_ACCOUNTS and at the RFC 8693 token exchange, where
	// it is a subject token (urn:envoy:params:oauth:token-type:google-identity). Its issuer is
	// BROKER_GOOGLE_OIDC_ISSUER, Google's own unless a dev stack says otherwise. Needs a Google
	// caller, a google: entry in BROKER_API_ACCOUNTS or a VM service in BROKER_VM_SERVICES, since
	// with neither no token for it could ever be accepted; API callers need no VM service. The
	// exchange accepts only accounts listed in BROKER_VM_SERVICES, and
	// BROKER_SIGNIN_DEVICE_AUDIENCE needs a VM service for its machine actor token. Unset, the
	// broker accepts no Google identity token, and BROKER_VM_SERVICES, BROKER_VM_PROJECTS,
	// BROKER_VM_INSTANCES, BROKER_VM_EXCHANGE_ACCOUNTS and BROKER_SIGNIN_DEVICE_AUDIENCE are refused.
	GoogleOIDCAudience string
	// BROKER_VM_SERVICES: the VM services, whitespace-separated name=<service account email>
	// entries, such as example-service=example-fleet@example-project.iam.gserviceaccount.com. A name
	// is lowercase letters, digits and hyphens, at most 64, not shared; the account is a user-managed
	// Google service account (<account id>@<project id>.iam.gserviceaccount.com, in lowercase, as a
	// token carries it) that the service's machines run as, so every machine of that account is one
	// identity. A
	// service may be named for several accounts, one entry each, but each account is given once and
	// proves exactly one service. A project's default service accounts (Compute Engine's and App
	// Engine's) and Google-managed service agents are refused: a project's workloads run as the first
	// by default and Google's own services as the second, so neither proves which VMs are the
	// service's. An account may be pinned to one Compute Engine project and one instance-name
	// pattern (BROKER_VM_PROJECTS and BROKER_VM_INSTANCES), or to neither, in which case its
	// machines are taken wherever they run. Keyed
	// by account, each answering its service. Needs BROKER_GOOGLE_OIDC_AUDIENCE. Unset, no VM service
	// is listed.
	VMServicesByAccount map[string]string
	// BROKER_VM_EXCHANGE_ACCOUNTS: the person and the Dispatch project each fleet writes for,
	// whitespace-separated <service account email>=<project>=<person>[=<session>] entries, the
	// grammar BROKER_EXCHANGE_ACCOUNTS uses for a pod, such as
	// example-fleet@example-project.iam.gserviceaccount.com=EXAMPLE=ada@example.com. A token the
	// exchange mints for a machine of that account may write that one project, acts for that
	// person, and writes under the session the entry fixes; an entry that fixes none requires the
	// machine to name its own session. The session is everything after the third "=", so it alone
	// may carry one. Each account is named at most once and must be one BROKER_VM_SERVICES lists.
	// An account BROKER_VM_SERVICES lists and this setting does not gets a token that names its
	// service and no person at all, which a resource server holds to what a service may do. Needs
	// BROKER_GOOGLE_OIDC_AUDIENCE. Unset, every listed account's machines get such a token. The
	// person an entry names is also the one whose Dispatch Inbox lists a request one of that
	// fleet's machines makes for a shared human-tier secret; it reaches no secret.
	//
	// The entries this answers are the whole of what the exchange decides a machine's token on:
	// each one carries its account's service (BROKER_VM_SERVICES), the pins (BROKER_VM_PROJECTS
	// and BROKER_VM_INSTANCES) and, where this setting names them, the person, the project and
	// the session.
	ExchangeGoogleAccounts []exchange.GoogleEntry
	// BROKER_EXCHANGE_K8S_AUDIENCE: the audience a pod's projected service-account token must
	// carry to be exchanged (RFC 8693) for a Dispatch access token. Its issuer is the one cluster
	// the broker is told about, BROKER_K8S_OIDC_ISSUER, which this needs; the audience is its own
	// because an exchanging pod projects a token for it beside the one it enrolls with
	// (BROKER_K8S_OIDC_AUDIENCE), so neither token is spendable where the other is. Unset, no
	// pod exchanges.
	ExchangeK8sAudience string
	// BROKER_EXCHANGE_GITHUB_ISSUER: the issuer of the token a continuous-integration workflow
	// run presents to the exchange, https://token.actions.githubusercontent.com for runs on
	// GitHub's own hosts. Set it with BROKER_EXCHANGE_GITHUB_AUDIENCE, or neither, in which case
	// the broker accepts no workflow token at all.
	ExchangeGitHubIssuer string
	// BROKER_EXCHANGE_GITHUB_AUDIENCE: the audience a workflow run's token must carry, the one
	// the run asks GitHub to mint it for.
	ExchangeGitHubAudience string
	// BROKER_EXCHANGE_ACCOUNTS: the Kubernetes service accounts whose pods exchange their
	// projected token, whitespace-separated
	// <service account>=<project>=<person>[=<session>] entries, such as
	// system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com. The token minted
	// for a pod of the account may write that one project, a project key that exchange.NewSet
	// refuses the start over when it is not one, acts for that person, and writes under
	// the session the entry fixes; an entry that fixes none requires the pod to name its own
	// session. An entry may name no project (<service account>==<person>): the shared
	// public-session account's, whose tokens act for that person and name no project. The
	// session is everything after the third "=", so it alone may carry one. A
	// namespace's default account is refused, as is an account listed twice. Needs
	// BROKER_K8S_OIDC_ISSUER and BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present.
	// Unset, no account exchanges.
	ExchangeAccounts []exchange.AccountEntry
	// BROKER_EXCHANGE_READ_ACCOUNTS: the Kubernetes service accounts whose pods exchange their
	// projected token for one that only reads, whitespace-separated
	// <service account>=<service>=<session> entries, such as
	// system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session. The
	// token minted for a pod of the account holds only the OAuth scope read, the project scope
	// none, the service and the session, and names no person and no project: Dispatch serves it
	// on the routes its read list names and refuses it everywhere else, and every other service
	// that reads the broker's tokens, the Envoy listener among them, refuses it. That token reads the
	// whole answer of GET /api/v1/projects/{key}/architecture for every project that has an
	// architecture source: each source, each component, and every issue of the project, closed and
	// iceboxed ones included. No setting holds a read entry to some projects.
	// The session is everything after the second "=", so it alone may carry one. A namespace's
	// default account is refused, as is a service that is not lowercase letters, digits and
	// hyphens, at most 64, or is shared, an account listed twice, and an account
	// BROKER_EXCHANGE_ACCOUNTS lists too. Needs BROKER_K8S_OIDC_ISSUER and
	// BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present. A broker of a build older
	// than this setting does not read it, so it mints those accounts no token at all. Unset, no
	// account exchanges for a read-only token.
	ExchangeReadAccounts []exchange.ReadEntry
	// BROKER_EXCHANGE_WORKFLOWS: the GitHub workflows whose runs exchange their token,
	// whitespace-separated <repository>=<workflow path>=<ref>=<project>=<person>[=<session>]
	// entries, such as
	// example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=EXAMPLE=ada@example.com.
	// Each field is compared exactly and nothing is wildcarded: a run of another workflow, or of
	// this one on another ref, is not this entry. The session is everything after the fifth "=",
	// so it alone may carry one, and a workflow listed twice is refused. Needs
	// BROKER_EXCHANGE_GITHUB_ISSUER and BROKER_EXCHANGE_GITHUB_AUDIENCE, whose tokens its runs
	// present. Unset, no workflow exchanges.
	ExchangeWorkflows []exchange.WorkflowEntry
	// BROKER_ENVOY_URL: an Envoy listener the broker notifies, best effort, when a session's
	// pending request expires undecided. Unset, the broker sends nothing. Needs
	// BROKER_ENVOY_ACCOUNT_TOKEN_FILE, the credential it calls the listener with.
	EnvoyURL string
	// BROKER_ENVOY_ACCOUNT_TOKEN_FILE: the broker's own projected Kubernetes ServiceAccount
	// token, read afresh at each call (ciphertoken.AccountToken), which the broker presents to
	// BROKER_ENVOY_URL as its bearer: the listener admits it as the account it names, as it
	// admits every caller's platform token, so no bearer is shared. Required with
	// BROKER_ENVOY_URL, and read only then.
	EnvoyAccountTokenFile string
	// BROKER_NATS_URLS, BROKER_NATS_SEED_FILE, BROKER_NATS_ISSUER_SEED_FILE,
	// BROKER_NATS_XKEY_SEED_FILE, BROKER_NATS_SERVICES, BROKER_NATS_K8S_AUDIENCE,
	// BROKER_NATS_GOOGLE_AUDIENCE: the NATS auth service the broker runs
	// (internal/cipher/natscallout), which the Envoy hub sends the authorization of every
	// connection its configuration lists no static user for, and which answers each with a NATS
	// user of that connection's own. BROKER_NATS_URLS is the hub's servers, comma separated, each
	// refused at start, by its place in the list and never its text, when it carries user info or
	// is no URL: the service connects as its seed's user, and the URLs are written to its log;
	// BROKER_NATS_SEED_FILE the service's own NATS user seed, a user the hub lists as an auth
	// user; BROKER_NATS_ISSUER_SEED_FILE the account seed the hub names as its auth_callout
	// issuer, which signs every user the service admits; BROKER_NATS_XKEY_SEED_FILE the curve
	// seed the hub seals each request to. Each file is read at start and refused unless it holds
	// a seed of its kind (a user, an account, a curve key), naming the variable and never the
	// value. BROKER_NATS_SERVICES lists the services admitted by platform identity, as
	// whitespace-separated <kind>/<machine id or ->/<* or ->=<account> entries (dispatch,
	// listener, daemon or measure; * for the kind's whole grant, - for the projects the account's
	// exchange entry lists), each account written as BROKER_API_ACCOUNTS writes one and read at
	// start (natscallout.ParseServices). BROKER_NATS_K8S_AUDIENCE is the audience a service's
	// projected Kubernetes token must carry, verified against BROKER_K8S_OIDC_ISSUER, which it
	// needs; BROKER_NATS_GOOGLE_AUDIENCE the audience a service's Google identity token must
	// carry, verified against BROKER_GOOGLE_OIDC_ISSUER; a service entry whose kind of token has
	// no audience is refused at start naming it. A session presents its broker access token and
	// needs neither. All unset, the broker runs no auth service; any set needs the URLs and the
	// three seed files.
	NATS NATSCallout
	// BROKER_LEASE_SECONDS: an enrollment's lease. A session renews it while it runs; one whose
	// lease runs out is no longer enrolled and its calls are refused. Time while no broker was
	// serving is not counted against it: a lease that ran out then stays live until a broker serves
	// again, which gives it that time back. A gap of up to three BROKER_SWEEP_SECONDS still counts
	// as a broker serving and is not given back, so a lease must outlive such a gap and the retries
	// its renewer then takes to renew, counting each renewal the outage refuses as failing at
	// whatever instant costs the renewer most, up to the 30-second client timeout: 91 s at a
	// sweep of 1 or 5 seconds, 105 s at 10, 225 s at 30 and 360 s at 60. The broker refuses a
	// shorter pair at boot and names the shortest lease that survives that sweep.
	LeaseSeconds int
	// BROKER_SESSION_CARRY_SECONDS: how long what a session holds — its grants and its pending
	// requests — waits on the enrollment of a box the session has left, for the next enrollment of
	// that session name under the same principal, or under an account BROKER_SESSION_ACCOUNT_PAIRS
	// pairs with it whose pod a login of the same account enrols, to take it. An enrollment that
	// ends while it holds either is left dormant instead of having them revoked
	// (enroll.endEnrollment), and the sweep ends it for good once this long has passed since it
	// went dormant (enroll.Service.EndCarried).
	//
	// 12 hours is requests.DefaultGrantLifetime, the life the broker already gives a grant whose
	// request named none, so the default carry never outlives the grant it carries; and it is what
	// an overnight approval takes, a request filed in the morning, the box replaced an hour later
	// and the approval granted later that day. 0 turns the carry off: the next sweep ends every
	// dormant enrollment it finds, which is what every end did before the carry. The maximum is
	// one day, because a dormant enrollment holds a grant no session is proving and the approval
	// behind it was given for one day's work: past a day the person who approved yesterday has not
	// approved today, and the session asking again is the right outcome.
	SessionCarrySeconds int
	// BROKER_PROOF_SKEW_SECONDS: how far the issue time of a signed proof or request object may
	// differ from the broker's clock.
	ProofSkewSeconds int
	// BROKER_ACCESS_TOKEN_SECONDS: how long an access token the broker's OAuth provider issues
	// lasts, a machine login's and an exchanged one's alike. A caller renews within it rather
	// than holding one token for a whole run, so it trades how often every caller renews against
	// how long a revoked credential's last token stays spendable.
	AccessTokenSeconds int
	// BROKER_LAUNCHER_CREDENTIAL_SECONDS: how long a machine login lasts without a refresh. An
	// approval mints its credential for this long, and each refresh moves its expiry to this long
	// from then; past it the machine logs in again, with a new key, a new code and a new approval.
	LauncherCredentialSeconds int
	// BROKER_SWEEP_SECONDS: how often the broker ends the enrollments whose lease lapsed and expires
	// the pending requests and machine logins nobody decided in time. It is also how often a serving
	// broker records that it serves: a gap of more than three sweeps between two such records is
	// time no broker was serving, which the broker gives back to every lease still running when the
	// gap began. A shorter gap is given back to nobody, so a long sweep demands a long
	// BROKER_LEASE_SECONDS.
	SweepSeconds int
	// BROKER_DRAIN_SECONDS: how long the broker goes on serving after SIGTERM while answering
	// GET /healthz with 503, so Kubernetes has stopped sending it new requests before it stops
	// accepting them. It is configuration rather than a compiled constant because the right value
	// is the cluster's EndpointSlice propagation time — how long a pod the control plane has
	// marked for deletion keeps receiving traffic — which lives in the deployment's own
	// repository, not this one, and must be tunable there without a new image. Removal from a
	// Service's EndpointSlice starts when the pod's deletion timestamp appears, concurrently with
	// the kubelet beginning termination, so what the drain must outlast is that propagation and
	// not a readiness probe's periodSeconds × failureThreshold, which no longer stands between
	// the pod and the endpoint controller by then; 5 seconds is what this repository's own
	// Dispatch deployment sizes its preStop wait at, for the same reason. 0 is for a local run or
	// a dev stack, where nothing propagates and the wait is pure delay.
	DrainSeconds int
	// BROKER_TRUSTED_PROXY_HEADER: the request header (e.g. X-Forwarded-For) whose last entry, on
	// its last line, the machine-login and secret-reread rate limiters take as the caller's
	// address. Unset, they use the connection's own address, which is right only when nothing
	// proxies the broker. Set it only when every request passes through your own reverse proxy,
	// which appends that entry; otherwise a caller can forge the header and pick its own rate-limit
	// bucket. The last line is read because a proxy that folds the lines into one, appends to the
	// last line, or adds a line of its own leaves its entry at the end of the last line, after any
	// line the client sent. The broker's edge (BROKER_EDGE_LISTEN_ADDR) does not read it: its
	// buckets key on the second-to-last entry of its one X-Forwarded-For line, the one its load
	// balancer appends for the client.
	TrustedProxyHeader string
}

// The four settings the broker's sign-in needs, each read by its own name: the provider's issuer,
// the client's id, the file holding the client's secret, and the Workspace domain.
const (
	signInIssuerSetting       = "BROKER_SIGNIN_ISSUER"
	signInClientIDSetting     = "BROKER_SIGNIN_CLIENT_ID"
	signInClientSecretSetting = "BROKER_SIGNIN_CLIENT_SECRET_FILE"
	signInDomainSetting       = "BROKER_SIGNIN_DOMAIN"
)

// SignInSettings are those four settings, every one of them required unless BROKER_SIGNIN is
// off. Every message that names them names them in this order.
var SignInSettings = []string{signInIssuerSetting, signInClientIDSetting, signInClientSecretSetting, signInDomainSetting}

// signInSwitchSetting is the one setting that turns the broker's person sign-in off, and
// signInOffValue the one value it takes; unset, the broker signs people in.
const (
	signInSwitchSetting = "BROKER_SIGNIN"
	signInOffValue      = "off"
)

// signInOffRefuses are the settings BROKER_SIGNIN=off refuses beside it, in the order every
// message naming them uses: the four of the sign-in client, how long a sign-in lasts, the
// audience a person's own device sign-in is traded at, which holds the person to the sign-in's
// domain, and the Workspace's other domains, which are listed beside that domain.
var signInOffRefuses = append(slices.Clone(SignInSettings), "BROKER_SIGNIN_SESSION_SECONDS", "BROKER_SIGNIN_DEVICE_AUDIENCE", workspaceDomainsSetting)

// airlockProxySettings are the settings the airlock's proxy port cannot run without, every one of
// them required with BROKER_AIRLOCK_RULES_FILE. Every message that names them names them in this
// order.
var airlockProxySettings = []string{"BROKER_AIRLOCK_PROXY_URL", "BROKER_AIRLOCK_CA_CERT_FILE", "BROKER_AIRLOCK_CA_KEY_FILE"}

// The five settings the broker's model judge needs, each read by its own name: the endpoint it
// calls its model at, the model's id, the audience of the identity token it presents there, the
// Dispatch it reads an issue from, and the file holding its token for that Dispatch. The
// protocol that endpoint speaks has a default, and the service account whose identity token the
// judge presents is optional, so neither is one of them.
const (
	judgeModelURLSetting          = "BROKER_JUDGE_MODEL_URL"
	judgeModelSetting             = "BROKER_JUDGE_MODEL"
	judgeModelAudienceSetting     = "BROKER_JUDGE_MODEL_AUDIENCE"
	judgeDispatchURLSetting       = "BROKER_JUDGE_DISPATCH_URL"
	judgeDispatchTokenFileSetting = "BROKER_JUDGE_DISPATCH_TOKEN_FILE"
	judgeModelProtocolSetting     = "BROKER_JUDGE_MODEL_PROTOCOL"
	judgeModelAccountSetting      = "BROKER_JUDGE_MODEL_ACCOUNT"
)

// JudgeSettings are those five settings, all-or-none: none of them set is a broker with no
// judge, and some of them set refuses the start naming the ones missing. Every message that
// names them names them in this order.
var JudgeSettings = []string{judgeModelURLSetting, judgeModelSetting, judgeModelAudienceSetting, judgeDispatchURLSetting, judgeDispatchTokenFileSetting}

// judgeModelProtocols are the wire protocols BROKER_JUDGE_MODEL_PROTOCOL takes, in the order
// every refusal naming them uses. They are agentloop's four Run kinds — AnthropicMessages,
// OpenAIResponses, OpenAIChat and AzureAnthropic, the Kind constants of
// packages/envoy/agentloop/agentloop.go, where SystemOne is Decide's kind and no Run's — written
// out here rather than imported: agentloop carries the Anthropic, OpenAI, Google and gRPC client
// libraries with it, and config, which every broker command loads before anything else, imports
// none of them. A value added there is added here.
var judgeModelProtocols = []string{"anthropic-messages", "openai-responses", "openai-chat", "azure-anthropic"}

// noUIToken is why BROKER_UI_TOKEN and its file are gone: the routes it authenticated take the
// caller's own Kubernetes ServiceAccount token now.
const noUIToken = "the broker's approval routes take the calling service's own Kubernetes ServiceAccount token, verified against BROKER_K8S_OIDC_ISSUER, so list that account in BROKER_API_ACCOUNTS instead of sharing a bearer with Dispatch's server"

// noDispatchCredential is why the broker's five Dispatch variables are gone: they named a
// Dispatch the broker asked and issued in, which it still does not do. What it reads there now it
// reads under settings of its own: the model judge's read of whether an issue is open and whom a
// Legion pod's issue is assigned to, and the token exchange's read of whether a person may see a
// project. So a deployment that still sets one of the five is configured for a Dispatch the broker
// never writes to and a credential it never holds.
const noDispatchCredential = "the broker opens no Dispatch ask and issues nothing in Dispatch; it reads there with two projected ServiceAccount tokens of its own, and writes nothing: its model judge, whether an issue is open and whom a Legion pod's issue is assigned to, at BROKER_JUDGE_DISPATCH_URL (BROKER_JUDGE_DISPATCH_TOKEN_FILE), and its token exchange, whether a person may see a project, at BROKER_PROJECT_ACCESS_URL (BROKER_PROJECT_ACCESS_TOKEN_FILE)"

// noDatabasePassword is why BROKER_DATABASE_PASSWORD and its placeholder are gone: BROKER_DATABASE_URL
// alone says how the broker signs in.
const noDatabasePassword = "the broker substitutes no password into BROKER_DATABASE_URL: on Amazon RDS it signs in by IAM token (https://cybermonklabs.github.io/legion/broker/operate/#signing-in-to-rds-by-iam-token), and any other database's password goes in the URL itself"

// noRulesFile is why these three variables are gone. They named a file that said who owned each
// secret and at what tier, which the secret's own tags say now. The broker does read a rules
// file again, BROKER_AIRLOCK_RULES_FILE, but it answers another question — where a secret's
// value may go — so the reason names it rather than letting a deployment point the old variables
// at it.
const noRulesFile = "the broker reads each secret's owner and tier from the secret's own tags under BROKER_SECRETS_PREFIX; where a secret's value may go is BROKER_AIRLOCK_RULES_FILE, the airlock's rules file, and no variable names any other rules file"

// noGrantCap is why BROKER_MAX_GRANT_SECONDS is gone: a request chooses its lifetime with --ttl;
// with none its grant lasts 12 hours, or until the requesting session ends when every requested
// secret carries default-grant=session.
const noGrantCap = "a request's --ttl chooses a shorter grant lifetime, otherwise its grant lasts 12 hours, or until the requesting session ends for a secret tagged default-grant=session"

// noMintTargets is why BROKER_MINT_TARGETS is gone: each secret's type tag names its minter.
const noMintTargets = "the broker no longer reads one list of mint targets; each secret's type tag names the minter it is served by"

// removedVars are environment variables the broker no longer reads. A stale deployment still
// setting one must fail loudly rather than silently running on configuration that means nothing
// any more.
var removedVars = []struct{ name, reason string }{
	{"BROKER_DISPATCH_URL", noDispatchCredential},
	{"BROKER_DISPATCH_TOKEN", noDispatchCredential},
	{"BROKER_DISPATCH_TOKEN_FILE", noDispatchCredential},
	{"BROKER_DISPATCH_PROJECT", noDispatchCredential},
	{"BROKER_ASK_POLL_SECONDS", noDispatchCredential},
	{"BROKER_UI_ORIGIN", "a person approves on the broker's own page, signed in to the broker (BROKER_SIGNIN_*), so the broker checks no WebAuthn origin"},
	{"BROKER_UI_TOKEN", noUIToken},
	{"BROKER_UI_TOKEN_FILE", noUIToken},
	{"BROKER_DATABASE_PASSWORD", noDatabasePassword},
	{"BROKER_RULES_FILE", noRulesFile},
	{"BROKER_RULES_S3_URI", noRulesFile},
	{"BROKER_RULES_RELOAD_SECONDS", noRulesFile},
	{"BROKER_MAX_GRANT_SECONDS", noGrantCap},
	{"BROKER_MINT_TARGETS", noMintTargets},
	{"BROKER_SIGNIN_CLIENT_SECRET", "the broker reads its sign-in client's secret only from the file BROKER_SIGNIN_CLIENT_SECRET_FILE names, so the secret is never in its environment"},
	{"BROKER_ENVOY_TOKEN", noSharedEnvoyBearer},
	{"BROKER_ENVOY_TOKEN_FILE", noSharedEnvoyBearer},
}

// NATSCallout is the NATS auth service's settings: Config.NATS documents each.
type NATSCallout struct {
	URLs                                   []string
	SeedFile, IssuerSeedFile, XKeySeedFile string
	// Services is BROKER_NATS_SERVICES as given, which natscallout.ParseServices reads at start.
	Services                    string
	K8sAudience, GoogleAudience string
}

// Enabled reports whether the broker runs the auth service.
func (n NATSCallout) Enabled() bool { return len(n.URLs) > 0 }

// noSharedEnvoyBearer is why BROKER_ENVOY_TOKEN and BROKER_ENVOY_TOKEN_FILE are gone.
const noSharedEnvoyBearer = "set BROKER_ENVOY_ACCOUNT_TOKEN_FILE to the broker's own projected ServiceAccount token instead: the listener admits the account it names, and a bearer the broker shared with every other caller of the listener is no longer read"

// serviceAccountPattern is a Kubernetes service account's subject, the one a pod's projected token
// carries: system:serviceaccount:<namespace>:<name>, the namespace a DNS label and the name a DNS
// subdomain.
var serviceAccountPattern = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?:[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$`)

// defaultServiceAccount is the account name Kubernetes gives every namespace and runs every pod
// whose spec names no account as.
const defaultServiceAccount = "default"

// serviceAccountEntry is one <service>=system:serviceaccount:<namespace>:<name> entry of
// BROKER_SERVICES or BROKER_SERVICE_LOGINS.
type serviceAccountEntry struct{ service, subject string }

// parseServiceAccounts reads setting's whitespace-separated <service>=<ServiceAccount subject>
// entries, the form BROKER_SERVICES and BROKER_SERVICE_LOGINS share, in order. It refuses, naming
// the entry: one not of that form or naming the service policy.OwnerShared; a namespace's default
// account, which every pod in that namespace whose spec names no account runs as and can get a
// token for the broker's audience with, so it proves nothing; and an account given twice, since
// each account proves one service: named for two services, the refusal names both; named twice
// for the same service, it names the account and that service. A service named for several
// accounts is allowed.
func parseServiceAccounts(setting, value string) ([]serviceAccountEntry, error) {
	var entries []serviceAccountEntry
	seen := map[string]string{}
	for _, text := range strings.Fields(value) {
		service, subject, ok := strings.Cut(text, "=")
		if !ok || !record.ValidService(service) || service == policy.OwnerShared || !serviceAccountPattern.MatchString(subject) {
			return nil, fmt.Errorf("%s must be <service>=system:serviceaccount:<namespace>:<name> entries, each service lowercase letters, digits and hyphens, at most 64, not %s, got %q", setting, policy.OwnerShared, text)
		}
		if isDefaultServiceAccount(subject) {
			return nil, defaultServiceAccountRefusal(setting, text)
		}
		if err := bindOnce(seen, setting, subject, service); err != nil {
			return nil, err
		}
		entries = append(entries, serviceAccountEntry{service: service, subject: subject})
	}
	return entries, nil
}

// servicesByAccount keys parseServiceAccounts' entries by account, each answering its service,
// the shape Config.ServicesByAccount and Config.ServiceLogins both hold. No entry, no map: a
// setting left unset reads nil rather than an empty map.
func servicesByAccount(entries []serviceAccountEntry) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	byAccount := make(map[string]string, len(entries))
	for _, entry := range entries {
		byAccount[entry.subject] = entry.service
	}
	return byAccount
}

// isDefaultServiceAccount reports whether subject is a namespace's default account, the one
// Kubernetes gives every namespace and runs every pod whose spec names no account as. No setting
// takes one: a token for it says only that some pod of that namespace is asking.
func isDefaultServiceAccount(subject string) bool {
	_, account, _ := strings.Cut(strings.TrimPrefix(subject, "system:serviceaccount:"), ":")
	return account == defaultServiceAccount
}

// defaultServiceAccountRefusal is the refusal of setting's entry text for naming a namespace's
// default account (isDefaultServiceAccount), whose token any pod of that namespace whose spec
// names no account can get for the broker's audience. parseServiceAccounts, parsePodRegistrars
// and ParseAccount share it, so each setting they read refuses such an account in the same words.
func defaultServiceAccountRefusal(setting, text string) error {
	return fmt.Errorf("%s names a namespace's default service account, got %q: every pod in that namespace whose spec names no account runs as it and can get a token for the broker's audience, so it proves nothing; name a dedicated account", setting, text)
}

// bindOnce records in seen that account proves service, refusing an account seen already, since
// each account proves one service: named for two services, the refusal names both; named twice for
// the same service, it names the account and that service. parseServiceAccounts and
// parseVMServiceAccounts share it.
func bindOnce(seen map[string]string, setting, account, service string) error {
	if other, twice := seen[account]; twice {
		if other == service {
			return fmt.Errorf("%s names the service account %s twice for %q; each account proves one service, so name it once", setting, account, service)
		}
		return fmt.Errorf("%s names the service account %s for both %q and %q; each account proves one service, so name it once", setting, account, other, service)
	}
	seen[account] = service
	return nil
}

// parsePodRegistrars reads setting's whitespace-separated <registrar account>=<pod account>
// entries, BROKER_POD_REGISTRARS, as the set of pairs the registration route admits. Both
// accounts are ServiceAccount subjects held to the rule BROKER_SERVICE_LOGINS holds its accounts
// to (serviceAccountPattern, isDefaultServiceAccount): the registrar is a login account and the
// pod account the subject of a pod's projected token. It refuses, naming setting and the entry:
// one without exactly one "=", an account of any other form (a google: one included), a
// namespace's default account, and a pair given twice. A registrar with several pod accounts, a
// pod account with several registrars and an account paired with itself are each allowed. No
// entries answer nil, as an unset setting does.
func parsePodRegistrars(setting, value string) (api.PodRegistrars, error) {
	var registrars api.PodRegistrars
	for _, text := range strings.Fields(value) {
		registrar, pod, _ := strings.Cut(text, "=")
		if strings.Count(text, "=") != 1 || !serviceAccountPattern.MatchString(registrar) || !serviceAccountPattern.MatchString(pod) {
			return nil, fmt.Errorf("%s must be <registrar account>=<pod account> entries, each account system:serviceaccount:<namespace>:<name>, got %q", setting, text)
		}
		if isDefaultServiceAccount(registrar) || isDefaultServiceAccount(pod) {
			return nil, defaultServiceAccountRefusal(setting, text)
		}
		if registrars.Lists(registrar, pod) {
			return nil, fmt.Errorf("%s names the pair %q twice; name each pair once", setting, text)
		}
		if registrars == nil {
			registrars = api.PodRegistrars{}
		}
		registrars[api.PodRegistrar{Registrar: registrar, Pod: pod}] = struct{}{}
	}
	return registrars, nil
}

// parseSessionAccountPairs reads setting's whitespace-separated <account>=<account> entries, the
// pairs BROKER_SESSION_ACCOUNT_PAIRS lists, in order, each as its two ServiceAccount subjects in
// the order the entry gives them. It refuses, naming the setting and the entry: one that is not
// two accounts joined by one "=" (no "=", a second one, or an empty side); an account that is not
// system:serviceaccount:<namespace>:<name>, or is a namespace's default account
// (podAccountRefusal), since every pod of that namespace whose spec names no account runs as it
// and so would take the names the other account holds; an account paired with itself, which
// pairs nothing and so is a mistake in the setting, most likely the other account mistyped; and
// a pair given twice, in either order, since a pair works both ways and the second entry says
// nothing the first did not. An account may be in several pairs, each its own.
func parseSessionAccountPairs(setting, value string) ([][2]string, error) {
	var pairs [][2]string
	// seen answers each pair given so far, keyed by its two accounts in sorted order so that
	// A=B and B=A are one key, with the entry that gave it.
	seen := map[[2]string]string{}
	for _, text := range strings.Fields(value) {
		a, b, ok := strings.Cut(text, "=")
		if !ok || a == "" || b == "" || strings.Contains(b, "=") {
			return nil, fmt.Errorf("%s must be <account>=<account> entries, two system:serviceaccount:<namespace>:<name> accounts joined by one =, got %q", setting, text)
		}
		for _, account := range []string{a, b} {
			if err := podAccountRefusal(setting, text, account); err != nil {
				return nil, err
			}
		}
		if a == b {
			return nil, fmt.Errorf("%s pairs an account with itself, got %q: a pair is two accounts, each taking the session names the other holds, so name two", setting, text)
		}
		key := [2]string{min(a, b), max(a, b)}
		if first, twice := seen[key]; twice {
			return nil, fmt.Errorf("%s gives the pair %q again as %q: a pair works both ways, so give it once", setting, first, text)
		}
		seen[key] = text
		pairs = append(pairs, [2]string{a, b})
	}
	return pairs, nil
}

// parseServiceAccountSecrets reads setting's whitespace-separated <SECRET NAME>=<account email>
// entries, the deployment's binding of each google-service-account agent secret to the one Google
// service account the broker may mint a token for it, and answers them keyed by secret name, the
// map secrets.GoogleServiceAccount.Accounts and policy.Loader.ServiceAccounts are both filled
// from. Its grammar is its own and not parseServiceAccounts' <service>=<ServiceAccount subject>:
// the name is an agent secret's, held to policy.NameToSlug exactly as BROKER_AZURE_LOGIN_SECRET
// is in Load, and the value is a Google account address, held to
// secrets.ValidServiceAccountEmail, the rule the minter itself applies, so a binding can never
// name an address the minter would refuse and so turn every mint of that secret into
// ErrMintNotBound at release time, where only the deployment could fix it.
//
// It refuses, naming setting and the whole entry text as the other parsers' refusals do: an
// entry not of that form; a name that is no agent-secret name, quoting policy.NameToSlug's own
// refusal; an account that is not a user-managed service account's address; a Google-managed
// service agent (googleServiceAgentPattern), refused as it is for BROKER_VM_SERVICES, since
// Google's own services run as those accounts and the broker must not be able to impersonate
// one; and one secret named twice, since each secret is bound to one account. One account bound
// to two secrets is allowed, and stays allowed: two secrets may name one account, and only the
// secret name is the key. No entries answer nil, as an unset setting does.
func parseServiceAccountSecrets(setting, value string) (map[string]string, error) {
	var bindings map[string]string
	for _, text := range strings.Fields(value) {
		name, account, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%s must be <SECRET NAME>=<account email> entries, such as GCP_ADMIN=gcp-admin@example-project.iam.gserviceaccount.com, got %q", setting, text)
		}
		if _, err := policy.NameToSlug(name); err != nil {
			return nil, fmt.Errorf("%s names the secret %q, which is %w, got %q: write the uppercase name a session asks for, such as GCP_ADMIN, not the secret's store id or its slug", setting, name, err, text)
		}
		if !secrets.ValidServiceAccountEmail(account) {
			return nil, fmt.Errorf("%s must bind each secret to a user-managed Google service account, <account id>@<project id>.iam.gserviceaccount.com in lowercase, which is the only address the minter impersonates, got %q", setting, text)
		}
		if googleServiceAgentPattern.MatchString(account) {
			return nil, fmt.Errorf("%s names a Google-managed service agent, got %q: Google's own services run as it and the broker must not be able to impersonate one; bind the secret to a user-managed account of your own", setting, text)
		}
		if bound, twice := bindings[name]; twice {
			return nil, fmt.Errorf("%s names the secret %s twice, bound already to %s, got %q; each secret is bound to one account, so name it once", setting, name, bound, text)
		}
		if bindings == nil {
			bindings = map[string]string{}
		}
		bindings[name] = account
	}
	return bindings, nil
}

// googleIDPart is the form Google gives both a service account's ID and a project's ID: a
// lowercase letter, then 4 to 28 lowercase letters, digits or hyphens, then a lowercase letter or
// digit, so 6 to 30 characters that never end in a hyphen.
const googleIDPart = `[a-z][a-z0-9-]{4,28}[a-z0-9]`

// googleProjectIDPattern is a Google project ID.
var googleProjectIDPattern = regexp.MustCompile(`^` + googleIDPart + `$`)

// vmServiceAccountPattern is a user-managed Google service account's email,
// <account id>@<project id>.iam.gserviceaccount.com, the only form a VM service may name.
var vmServiceAccountPattern = regexp.MustCompile(`^` + googleIDPart + `@` + googleIDPart + `\.iam\.gserviceaccount\.com$`)

// defaultGoogleServiceAccountPattern is a project's Compute Engine default account
// (<project number>-compute@developer.gserviceaccount.com) or its App Engine default account
// (<project id>@appspot.gserviceaccount.com). Google makes them for the project and its workloads
// run as them by default, so a token from one proves nothing about which VMs are a service's.
var defaultGoogleServiceAccountPattern = regexp.MustCompile(`^([0-9]+-compute@developer|` + googleIDPart + `@appspot)\.gserviceaccount\.com$`)

// googleServiceAgentPattern is a Google-managed service agent: an account named
// service-<number>, service-org-<number> or service-folder-<number>, or any account of a gcp-sa-
// domain. Google's own services run as these accounts, never a VM.
var googleServiceAgentPattern = regexp.MustCompile(`^service-(org-|folder-)?[0-9]+@|@gcp-sa-`)

// instancePatternChars are the characters an instance-name glob may hold: an instance name's own
// (lowercase letters, digits and hyphens) and path.Match's *, ?, [, ] and ^ (validInstancePattern
// takes ^ only where it negates a class). Any other character could never match an instance name,
// and a ! would read as negation to someone who knows shell globs, where path.Match takes it
// literally.
var instancePatternChars = regexp.MustCompile(`^[a-z0-9*?\[\]^-]+$`)

// vmAccountRefusal refuses, naming setting and the entry text, an account no VM service may name:
// a user-managed account's address written with capital letters (a Google identity token carries
// it in lowercase), a project's default account, a Google-managed service agent, and any address
// that is not a user-managed service account's. It answers nil for an account a VM service may
// name.
func vmAccountRefusal(setting, text, account string) error {
	switch {
	case account != strings.ToLower(account) && vmServiceAccountPattern.MatchString(strings.ToLower(account)):
		return fmt.Errorf("%s names a service account with capital letters, got %q: a Google identity token carries the address in lowercase, so write it in lowercase", setting, text)
	case defaultGoogleServiceAccountPattern.MatchString(account):
		return fmt.Errorf("%s names a project's default service account, got %q: the project's workloads run as it by default, so it proves nothing about which VMs are a service's; name a dedicated account", setting, text)
	case googleServiceAgentPattern.MatchString(account):
		return fmt.Errorf("%s names a Google-managed service agent, got %q: Google's own services run as it, never a VM; name a dedicated account", setting, text)
	case !vmServiceAccountPattern.MatchString(account):
		return fmt.Errorf("%s must name user-managed service accounts, <account id>@<project id>.iam.gserviceaccount.com, got %q", setting, text)
	}
	return nil
}

// googleAPIAccountTag opens the account of a BROKER_API_ACCOUNTS entry whose caller presents a
// Google identity token. A Kubernetes entry already names its own kind — every such subject
// begins system:serviceaccount: — so tagging this one leaves every entry written today
// unchanged and still lets a reader, and the parser, tell the two kinds apart by the entry's
// own text rather than by whether an address happens to look like an email.
const googleAPIAccountTag = "google:"

// apiAccountEntry is one <caller>=<account> entry of BROKER_API_ACCOUNTS: the caller's name, the
// account as the broker looks it up (a ServiceAccount subject, or a Google account address with
// its tag removed), which kind it is, and the entry's text as the setting gave it.
type apiAccountEntry struct {
	caller, account string
	google          bool
	text            string
}

// parseAPIAccounts reads setting's whitespace-separated <caller>=<account> entries, in order. An
// account written google:<address> is a Google service account, looked up by the email its
// identity token carries; any other is a Kubernetes service account, looked up by the subject
// its projected token carries. It refuses, naming the entry: one of neither form; a namespace's
// default account, as parseServiceAccounts does; a Google account vmAccountRefusal refuses,
// which is the rule BROKER_VM_SERVICES already holds every Google account to; and an account
// given twice, since each account calls as one caller. Whether the caller's name is one this
// broker knows is its own table's to answer (api.AccountFor).
func parseAPIAccounts(setting, value string) ([]apiAccountEntry, error) {
	var entries []apiAccountEntry
	seen := map[string]string{}
	for _, text := range strings.Fields(value) {
		caller, account, ok := strings.Cut(text, "=")
		if !ok || !record.ValidService(caller) {
			return nil, apiAccountForm(setting, text)
		}
		parsed, err := ParseAccount(setting, text, account)
		if errors.Is(err, ErrAccountForm) {
			return nil, apiAccountForm(setting, text)
		}
		if err != nil {
			return nil, err
		}
		entry := apiAccountEntry{caller: caller, account: parsed.Address, google: parsed.Google, text: text}
		if other, twice := seen[entry.account]; twice {
			if other == caller {
				return nil, fmt.Errorf("%s names the account %s twice for %q; each account calls as one caller, so name it once", setting, entry.account, caller)
			}
			return nil, fmt.Errorf("%s names the account %s for both %q and %q; each account calls as one caller, so name it once", setting, entry.account, other, caller)
		}
		seen[entry.account] = caller
		entries = append(entries, entry)
	}
	return entries, nil
}

// Account is a platform account as BROKER_API_ACCOUNTS writes one: a Kubernetes service account's
// subject, or a Google service account's address.
type Account struct {
	// Address is the account as the broker looks it up: the subject a projected token carries,
	// system:serviceaccount:<namespace>:<name>, or the email a Google identity token carries,
	// without its google: tag.
	Address string
	// Google is whether the account is a Google service account.
	Google bool
}

// ErrAccountForm is ParseAccount's answer for an account written in neither form, which each
// setting refuses naming its own entry form.
var ErrAccountForm = errors.New("config: an account is system:serviceaccount:<namespace>:<name> or google:<service account email>")

// ParseAccount reads account, written as a BROKER_API_ACCOUNTS entry writes one: google:<address>
// for a Google service account, looked up by the email its identity token carries, and any other
// a Kubernetes service account, looked up by the subject its projected token carries. It refuses,
// naming setting and text, the entry it came from: a namespace's default account, as
// parseServiceAccounts does, and a Google account vmAccountRefusal refuses, which is the rule
// BROKER_VM_SERVICES holds every Google account to. An account of neither form is ErrAccountForm.
// Every setting that names a caller's account reads it here (BROKER_API_ACCOUNTS, and
// BROKER_NATS_SERVICES through internal/cipher/natscallout), so no setting admits an account
// another refuses.
func ParseAccount(setting, text, account string) (Account, error) {
	switch address, tagged := strings.CutPrefix(account, googleAPIAccountTag); {
	case tagged:
		if err := vmAccountRefusal(setting, text, address); err != nil {
			return Account{}, err
		}
		return Account{Address: address, Google: true}, nil
	case !serviceAccountPattern.MatchString(account):
		return Account{}, ErrAccountForm
	case isDefaultServiceAccount(account):
		return Account{}, defaultServiceAccountRefusal(setting, text)
	}
	return Account{Address: account}, nil
}

// apiAccountForm is the refusal of a BROKER_API_ACCOUNTS entry of neither form, which names both.
func apiAccountForm(setting, text string) error {
	return fmt.Errorf("%s must be <caller>=system:serviceaccount:<namespace>:<name> or <caller>=%s<service account email> entries, each caller lowercase letters, digits and hyphens, at most 64, got %q", setting, googleAPIAccountTag, text)
}

// vmServiceAccountEntry is one <service>=<service account email> entry of BROKER_VM_SERVICES,
// with its text as the setting gave it.
type vmServiceAccountEntry struct{ service, account, text string }

// parseVMServiceAccounts reads setting's whitespace-separated <service>=<service account email>
// entries, in order. It refuses, naming the entry: one not of that form or naming the service
// policy.OwnerShared, as parseServiceAccounts does; an account vmAccountRefusal refuses; and an
// account given twice, through bindOnce, which parseServiceAccounts shares. A service named for
// several accounts is allowed.
func parseVMServiceAccounts(setting, value string) ([]vmServiceAccountEntry, error) {
	var entries []vmServiceAccountEntry
	seen := map[string]string{}
	for _, text := range strings.Fields(value) {
		service, account, ok := strings.Cut(text, "=")
		if !ok || !record.ValidService(service) || service == policy.OwnerShared {
			return nil, fmt.Errorf("%s must be <service>=<service account email> entries, each service lowercase letters, digits and hyphens, at most 64, not %s, got %q", setting, policy.OwnerShared, text)
		}
		if err := vmAccountRefusal(setting, text, account); err != nil {
			return nil, err
		}
		if err := bindOnce(seen, setting, account, service); err != nil {
			return nil, err
		}
		entries = append(entries, vmServiceAccountEntry{service: service, account: account, text: text})
	}
	return entries, nil
}

// vmAccountValue is one <service account email>=<value> entry of BROKER_VM_PROJECTS or
// BROKER_VM_INSTANCES, with its text as the setting gave it.
type vmAccountValue struct{ account, value, text string }

// parseVMAccountValues reads setting's whitespace-separated <service account email>=<kind>
// entries, in order. It refuses, naming the entry: one not of that form or whose value valid
// refuses; an account vmAccountRefusal refuses; and an account given twice, since a VM identity
// must have one value of each kind.
func parseVMAccountValues(setting, value, kind string, valid func(string) bool) ([]vmAccountValue, error) {
	var entries []vmAccountValue
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		account, attribute, ok := strings.Cut(text, "=")
		if !ok || !valid(attribute) {
			return nil, fmt.Errorf("%s must be <service account email>=<%s> entries, got %q", setting, kind, text)
		}
		if err := vmAccountRefusal(setting, text, account); err != nil {
			return nil, err
		}
		if seen[account] {
			return nil, fmt.Errorf("%s names the service account %s twice; each account has one %s, so name it once", setting, account, kind)
		}
		seen[account] = true
		entries = append(entries, vmAccountValue{account: account, value: attribute, text: text})
	}
	return entries, nil
}

// validInstancePattern reports whether pattern is an instance-name glob: path.Match's syntax,
// holding only instancePatternChars, with ^ only as a class's negation ("[^"), the one place
// path.Match does not take it as a literal no instance name holds.
func validInstancePattern(pattern string) bool {
	_, err := path.Match(pattern, "")
	return instancePatternChars.MatchString(pattern) && strings.Count(pattern, "^") == strings.Count(pattern, "[^") && err == nil
}

// vmAccountMap keys entries by account, refusing, naming setting and the entry, one whose account
// services does not bind: a project or an instance pattern for an account no VM service lists
// would configure nothing. No entries answer nil, as an unset setting does.
func vmAccountMap(setting string, entries []vmAccountValue, services map[string]string) (map[string]string, error) {
	var values map[string]string
	for _, entry := range entries {
		if _, listed := services[entry.account]; !listed {
			return nil, fmt.Errorf("%s names %s, got %q, but BROKER_VM_SERVICES does not list it; a project or instance entry must name a listed VM account", setting, entry.account, entry.text)
		}
		if values == nil {
			values = map[string]string{}
		}
		values[entry.account] = entry.value
	}
	return values, nil
}

// exchangeFields splits one entry of an exchange setting into want fields on "=", the last field
// — a session name — taking everything after the last separator, since a session name may carry
// "=" and no other field may. An entry one field short fixes no session and answers "" for it;
// any other count is not of the form.
func exchangeFields(text string, want int) ([]string, bool) {
	fields := strings.SplitN(text, "=", want)
	switch len(fields) {
	case want:
		return fields, true
	case want - 1:
		return append(fields, ""), true
	}
	return nil, false
}

// podAccountRefusal refuses, naming setting and the entry text, an account no exchange entry for
// a pod, no BROKER_POD_OPERATORS entry and no side of a session account pair may name: one that
// is not system:serviceaccount:<namespace>:<name>, and a namespace's default account. It answers
// nil for an account such an entry may name, and is to a pod's account what vmAccountRefusal is
// to a VM's.
func podAccountRefusal(setting, text, account string) error {
	switch {
	case !serviceAccountPattern.MatchString(account):
		return fmt.Errorf("%s must name system:serviceaccount:<namespace>:<name> accounts, got %q", setting, text)
	case isDefaultServiceAccount(account):
		return fmt.Errorf("%s names a namespace's default service account, got %q: every pod in that namespace whose spec names no account runs as it, so its token says nothing about which pod is asking; name a dedicated account", setting, text)
	}
	return nil
}

// parsePodOperators reads setting's whitespace-separated <person>=<ServiceAccount subject>
// entries, the accounts BROKER_POD_OPERATORS maps to a person. It answers them keyed by account,
// each answering its person, which is the map Config.PodOperators holds. It cuts each entry at
// its first "=", because the person comes first and no account holds an "=". Each refusal names
// setting and the entry text. It refuses an entry with no "=". It refuses a person that is not a
// lower-case email in record.CanonicalLogin's form, because every login comparison of the broker
// reads that form. It refuses an account podAccountRefusal refuses. It refuses an account given
// twice, because one account acts for one person. A person may have several accounts. No entry
// gives a nil map, as an unset setting does. Load checks the mapped accounts against the other
// settings (mappedAccountRefusal).
func parsePodOperators(setting, value string) (map[string]string, error) {
	var people map[string]string
	for _, text := range strings.Fields(value) {
		person, account, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%s must be <email>=system:serviceaccount:<namespace>:<name> entries, such as ada@example.com=system:serviceaccount:example-ns:ada, got %q", setting, text)
		}
		if record.CanonicalLogin(person) != person || !policy.ValidPersonOwner(person) {
			return nil, fmt.Errorf("%s must name each person by their email in lower case, as they sign in, such as ada@example.com, got %q", setting, text)
		}
		if err := podAccountRefusal(setting, text, account); err != nil {
			return nil, err
		}
		if _, twice := people[account]; twice {
			return nil, fmt.Errorf("%s names the service account %s twice, got %q again; one account acts for one person, so name it once", setting, account, text)
		}
		if people == nil {
			people = map[string]string{}
		}
		people[account] = person
	}
	return people, nil
}

// mappedAccountRefusal refuses an account that BROKER_POD_OPERATORS maps (cfg.PodOperators) when
// another setting gives that account, or its login service, a role that a person's account must
// not have. Load calls it after it reads each setting it checks. Each refusal names the account
// and both settings, and says what to change. It examines the mapped accounts in sorted order, so
// one configuration always gets the same refusal first.
//
// The class is every setting that gives a Kubernetes account a role of its own, a service, a
// caller, a token of its own, a registrar's choice of session names or another account's session
// names, since a mapped account acts for its person and for nothing else. The settings it checks
// are BROKER_SERVICE_LOGINS, BROKER_SERVICES, BROKER_VM_SERVICES, BROKER_EXCHANGE_ACCOUNTS,
// BROKER_POD_REGISTRARS, BROKER_EXCHANGE_READ_ACCOUNTS, BROKER_API_ACCOUNTS, BROKER_NATS_SERVICES
// and BROKER_SESSION_ACCOUNT_PAIRS, in the order it checks them. A setting that Load reads through
// getenv on the two configurations of TestEveryBrokerSettingLoadReadsIsMarked (the person sign-in
// on and off, each with every condition in front of a read set) fails that test until its author
// marks it, as giving a Kubernetes account a role, beside its refusal here and its row in
// TestPodOperatorsRefusesAMappedAccountAnotherSettingGivesARole, or as giving none. A setting read
// under a condition those configurations do not set, or read other than through getenv, is outside
// the test: cmd/cipherd's BROKER_FAKE_SECRETS_FILE and the PG* variables pgconn.ParseConfig reads in
// databaseIAM are two today. For such a setting the class is a convention rather than a rule.
//
// It refuses a mapped account that BROKER_SERVICE_LOGINS does not name, because a mapped account
// logs in with its own token. It refuses a mapped account whose login service a BROKER_SERVICES
// entry names, because a secret could then name that service as its owner. It refuses a mapped
// account that a BROKER_SERVICES entry binds, because a launcher logged in as that service could
// then enrol the person's pods as its own workers (enroll.Service.launches). It refuses a mapped
// account whose login service a BROKER_VM_SERVICES entry names, because cmd/cipherd's serviceNames
// gives the policy loader the services of BROKER_SERVICES and BROKER_VM_SERVICES alike. It refuses
// a mapped account that a BROKER_EXCHANGE_ACCOUNTS entry maps to another person, after it writes
// both people in record.CanonicalLogin's form. It refuses a mapped account that a
// BROKER_POD_REGISTRARS entry names as its registrar (cfg.PodRegistrars), because a registrar
// chooses the session names that the pods it registers exchange under, and each name stays bound to
// the registrar's account for good (oauthstore.Store.RegisterPodSession), so the person's pod would
// choose the session names other pods exchange under. The registration routes refuse a mapped
// account already (api's serviceCaller, 403 PERSON_ACCOUNT), so such an entry could register
// nothing. A mapped account on the pod side of an entry is not refused: a registration of its pod
// is admitted only under a BROKER_EXCHANGE_ACCOUNTS entry that fixes no session (the pod-session
// policy's 409 SESSION_FIXED), and every BROKER_EXCHANGE_ACCOUNTS entry for the account names the
// same person (the refusal above), so the pod acts for that person alone. It refuses a mapped
// account that a BROKER_EXCHANGE_READ_ACCOUNTS entry names (cfg.ExchangeReadAccounts), because the
// exchange would then mint the person's pod a read-only token that names that entry's service and
// no person. It refuses a mapped account that a BROKER_API_ACCOUNTS entry names (cfg.APIAccounts),
// because the named-caller routes it reaches act on the caller's word: the approval relays take the
// deciding person's name from the caller, so the person's pod could decide any request in any
// person's name. It refuses a mapped account that a BROKER_NATS_SERVICES entry names, because the
// NATS auth service (internal/cipher/natscallout) would then answer a connection of the person's
// pod with that entry's service grant. It reads each entry's account with CutNATSServiceEntry, the
// reading natscallout.ParseServices makes too, which holds the account to ParseAccount's form at
// the boot. It refuses a mapped account that either side of a BROKER_SESSION_ACCOUNT_PAIRS pair
// names (cfg.SessionAccountPairs), because a pair lets one account take the session names the other
// holds. A person's pod login enrols under no session name (enroll.Service.podUnderTokenLogin), so
// a pair moves no enrollment of it, but the session names the token exchange binds for the account
// would cross: the other account could take the person's, and the person's pod the other account's.
func mappedAccountRefusal(cfg *Config) error {
	const setting = "BROKER_POD_OPERATORS"
	if len(cfg.PodOperators) == 0 {
		return nil
	}
	services := make(map[string]bool, len(cfg.ServicesByAccount))
	for _, service := range cfg.ServicesByAccount {
		services[service] = true
	}
	vmServices := make(map[string]bool, len(cfg.VMServicesByAccount))
	for _, service := range cfg.VMServicesByAccount {
		vmServices[service] = true
	}
	exchangePeople := make(map[string]string, len(cfg.ExchangeAccounts))
	for _, entry := range cfg.ExchangeAccounts {
		exchangePeople[entry.Account] = entry.Person
	}
	natsEntries := make(map[string]string)
	for _, text := range strings.Fields(cfg.NATS.Services) {
		if _, account, ok := CutNATSServiceEntry(text); ok {
			natsEntries[account] = text
		}
	}
	pairs := make(map[string]string)
	for _, pair := range cfg.SessionAccountPairs {
		for _, side := range pair {
			if _, named := pairs[side]; !named {
				pairs[side] = pair[0] + "=" + pair[1]
			}
		}
	}
	// Each registrar's entry with the first pod account in sort order, so one configuration always
	// names the same entry.
	registrarPods := make(map[string]string, len(cfg.PodRegistrars))
	for pair := range cfg.PodRegistrars {
		if pod, named := registrarPods[pair.Registrar]; !named || pair.Pod < pod {
			registrarPods[pair.Registrar] = pair.Pod
		}
	}
	readServices := make(map[string]string, len(cfg.ExchangeReadAccounts))
	for _, entry := range cfg.ExchangeReadAccounts {
		readServices[entry.Account] = entry.Service
	}
	for _, account := range slices.Sorted(maps.Keys(cfg.PodOperators)) {
		person := cfg.PodOperators[account]
		login, listed := cfg.ServiceLogins[account]
		if !listed {
			return fmt.Errorf("%s maps the service account %s to %s, and BROKER_SERVICE_LOGINS does not name it; a mapped account logs in with its own token under a login service of the person's own, so add <service>=%s to BROKER_SERVICE_LOGINS", setting, account, person, account)
		}
		if services[login] {
			return fmt.Errorf("%s maps the service account %s to %s, and BROKER_SERVICES names its login service %q (BROKER_SERVICE_LOGINS); a secret could then name that service as its owner, and a person's login service must grant nothing by itself, so log the account in under a service BROKER_SERVICES does not name", setting, account, person, login)
		}
		if bound, binds := cfg.ServicesByAccount[account]; binds {
			return fmt.Errorf("%s maps the service account %s to %s, and BROKER_SERVICES binds it to the service %q; a launcher logged in as that service would enrol the person's pods as its own workers, with that service's secrets, so take the account out of BROKER_SERVICES or out of %s", setting, account, person, bound, setting)
		}
		if vmServices[login] {
			return fmt.Errorf("%s maps the service account %s to %s, and BROKER_VM_SERVICES names its login service %q (BROKER_SERVICE_LOGINS); the broker registers that setting's services as owners a secret may name, as it does BROKER_SERVICES', and a person's login service must grant nothing by itself, so log the account in under a service BROKER_VM_SERVICES does not name", setting, account, person, login)
		}
		if other, exchanges := exchangePeople[account]; exchanges && record.CanonicalLogin(other) != record.CanonicalLogin(person) {
			return fmt.Errorf("%s maps the service account %s to %s, and BROKER_EXCHANGE_ACCOUNTS maps it to %s; one account acts for one person, so map it to the same person in both settings or take it out of one of them", setting, account, person, other)
		}
		if pod, registers := registrarPods[account]; registers {
			return fmt.Errorf("%s maps the service account %s to %s, and the BROKER_POD_REGISTRARS entry %q names it as a registrar; a registrar chooses the session names that the pods it registers exchange under, and each name stays bound to the registrar's account for good, so take the account out of BROKER_POD_REGISTRARS as a registrar or out of %s", setting, account, person, account+"="+pod, setting)
		}
		if service, reads := readServices[account]; reads {
			return fmt.Errorf("%s maps the service account %s to %s, and BROKER_EXCHANGE_READ_ACCOUNTS names it as a reader of the service %q; the exchange would mint the person's pod a read-only token that names that service and no person, so take the account out of BROKER_EXCHANGE_READ_ACCOUNTS or out of %s", setting, account, person, service, setting)
		}
		if caller, calls := cfg.APIAccounts[account]; calls {
			return fmt.Errorf("%s maps the service account %s to %s, and BROKER_API_ACCOUNTS names it as the caller %q; that caller's routes act on the caller's word, and the approval relays take the deciding person's name from it, so take the account out of BROKER_API_ACCOUNTS or out of %s", setting, account, person, caller.Name, setting)
		}
		if entry, names := natsEntries[account]; names {
			return fmt.Errorf("%s maps the service account %s to %s, and the BROKER_NATS_SERVICES entry %q names it; the NATS auth service would answer a connection of the person's pod with that service's NATS grant, so take the account out of BROKER_NATS_SERVICES or out of %s", setting, account, person, entry, setting)
		}
		if pair, paired := pairs[account]; paired {
			return fmt.Errorf("%s maps the service account %s to %s, and the BROKER_SESSION_ACCOUNT_PAIRS pair %q names it; a pair lets either account take the session names the other holds, so the session names the token exchange binds for the person's account and for the other account would cross, and a person's account shares no session name with another account, so take the account out of the pair or out of %s", setting, account, person, pair, setting)
		}
	}
	return nil
}

// parseExchangeAccounts reads setting's whitespace-separated
// <service account>=<project>=<person>[=<session>] entries, the identities BROKER_EXCHANGE_ACCOUNTS
// lists, in order. The project may be empty (<service account>==<person>), which is the shared
// public-session account's entry: its named tokens act for the person and name no project. It
// refuses, naming the entry: one not of that form or with a blank account or person; an account
// that is not system:serviceaccount:<namespace>:<name>, or a namespace's default account
// (podAccountRefusal); and an account listed twice, since one account writes as one identity.
// exchange.NewSet is the second gate, and the session name's own form is its to refuse.
func parseExchangeAccounts(setting, value string) ([]exchange.AccountEntry, error) {
	var entries []exchange.AccountEntry
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		fields, ok := exchangeFields(text, 4)
		if !ok || fields[0] == "" || fields[2] == "" {
			return nil, fmt.Errorf("%s must be <service account>=<project>=<person>[=<session>] entries, the project empty for an entry naming none and the session everything after the third =, got %q", setting, text)
		}
		if err := podAccountRefusal(setting, text, fields[0]); err != nil {
			return nil, err
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("%s names the service account %s twice; one account writes as one identity, so name it once", setting, fields[0])
		}
		seen[fields[0]] = true
		entries = append(entries, exchange.AccountEntry{Account: fields[0], Project: fields[1], Person: fields[2], Session: fields[3]})
	}
	return entries, nil
}

// parseExchangeReadAccounts reads setting's whitespace-separated
// <service account>=<service>=<session> entries, the read entries BROKER_EXCHANGE_READ_ACCOUNTS
// lists, in order. Every field is required and the session is everything after the second "=",
// so it alone may carry one. It refuses, naming the setting and the entry: one with fewer than
// three fields or a blank one; an account that is not system:serviceaccount:<namespace>:<name>,
// or a namespace's default account (podAccountRefusal); a service that is not a service's name,
// or is policy.OwnerShared, the rule parseServiceAccounts holds a service to; and an account
// listed twice, since one account reads as one identity. An account BROKER_EXCHANGE_ACCOUNTS
// lists too is Load's to refuse, since it reads both settings. exchange.NewSet is the second
// gate, and the session name's own form is its to refuse.
func parseExchangeReadAccounts(setting, value string) ([]exchange.ReadEntry, error) {
	var entries []exchange.ReadEntry
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		// An entry with no session comes back with a blank one (exchangeFields), which the blank
		// check refuses as it refuses any other blank field: a read entry's session is required.
		fields, ok := exchangeFields(text, 3)
		if !ok || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			return nil, fmt.Errorf("%s must be <service account>=<service>=<session> entries, every field required and the session everything after the second =, got %q", setting, text)
		}
		account, service, session := fields[0], fields[1], fields[2]
		if err := podAccountRefusal(setting, text, account); err != nil {
			return nil, err
		}
		if !record.ValidService(service) || service == policy.OwnerShared {
			return nil, fmt.Errorf("%s must name services of lowercase letters, digits and hyphens, at most 64, not %s, got %q", setting, policy.OwnerShared, text)
		}
		if seen[account] {
			return nil, fmt.Errorf("%s names the service account %s twice, got %q again; one account reads as one identity, so name it once", setting, account, text)
		}
		seen[account] = true
		entries = append(entries, exchange.ReadEntry{Account: account, Service: service, Session: session})
	}
	return entries, nil
}

// vmExchangeGrant is one BROKER_VM_EXCHANGE_ACCOUNTS entry: the account it names, the Dispatch
// project its tokens write, the person they act for, the session name it fixes, if any, and its
// text as the setting gave it.
type vmExchangeGrant struct{ account, project, person, session, text string }

// parseVMExchangeGrants reads setting's whitespace-separated
// <service account email>=<project>=<person>[=<session>] entries, the grants
// BROKER_VM_EXCHANGE_ACCOUNTS gives the accounts BROKER_VM_SERVICES lists, in order. It refuses,
// naming the entry: one not of that form or with a blank field; an account vmAccountRefusal
// refuses; and an account named twice, since one account writes as one identity. Whether the
// account is listed at all is the caller's to refuse, as it is for a project or an instance
// pattern (vmAccountMap), and exchange.NewSet is the second gate.
func parseVMExchangeGrants(setting, value string) ([]vmExchangeGrant, error) {
	var grants []vmExchangeGrant
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		fields, ok := exchangeFields(text, 4)
		if !ok || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			return nil, fmt.Errorf("%s must be <service account email>=<project>=<person>[=<session>] entries, the session everything after the third =, got %q", setting, text)
		}
		if err := vmAccountRefusal(setting, text, fields[0]); err != nil {
			return nil, err
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("%s names the service account %s twice; one account writes as one identity, so name it once", setting, fields[0])
		}
		seen[fields[0]] = true
		grants = append(grants, vmExchangeGrant{account: fields[0], project: fields[1], person: fields[2], session: fields[3], text: text})
	}
	return grants, nil
}

// parseAirlockTestDialAllow reads BROKER_AIRLOCK_TEST_DIAL_ALLOW's whitespace-separated
// <host>:<port> addresses, in order. It refuses, naming the entry: one that is no host and
// port, and one whose port is not a number, since an address the proxy compares a CONNECT
// target against has to be the exact string that target becomes.
func parseAirlockTestDialAllow(value string) ([]string, error) {
	const setting = "BROKER_AIRLOCK_TEST_DIAL_ALLOW"
	var allowed []string
	for _, text := range strings.Fields(value) {
		host, port, err := net.SplitHostPort(text)
		if err != nil || host == "" {
			return nil, fmt.Errorf("%s must be <host>:<port> addresses, got %q", setting, text)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, fmt.Errorf("%s: %q names no port number", setting, text)
		}
		allowed = append(allowed, text)
	}
	return allowed, nil
}

// parseAirlockTestResolve reads BROKER_AIRLOCK_TEST_RESOLVE's whitespace-separated
// <host>=<address> entries. It refuses, naming the entry: one not of that form, one whose
// address is no IP address, and a host named twice, since a host has one address here and the
// second would silently replace the first.
func parseAirlockTestResolve(value string) (map[string]netip.Addr, error) {
	const setting = "BROKER_AIRLOCK_TEST_RESOLVE"
	var hosts map[string]netip.Addr
	for _, text := range strings.Fields(value) {
		host, address, ok := strings.Cut(text, "=")
		if !ok || host == "" {
			return nil, fmt.Errorf("%s must be <host>=<address> entries, got %q", setting, text)
		}
		addr, err := netip.ParseAddr(address)
		if err != nil {
			return nil, fmt.Errorf("%s: %q names no IP address", setting, text)
		}
		if _, twice := hosts[host]; twice {
			return nil, fmt.Errorf("%s names the host %s twice; a host resolves to one address here, so name it once", setting, host)
		}
		if hosts == nil {
			hosts = map[string]netip.Addr{}
		}
		hosts[host] = netaddr.Canonical(addr)
	}
	return hosts, nil
}

// parseAirlockClusterRanges reads BROKER_AIRLOCK_CLUSTER_RANGES' comma-separated CIDR prefixes, in
// order, each as its network; spaces around a comma are allowed, and a blank value is none. It
// refuses, naming the element's position and never the element, which could be anything an
// operator pasted: an empty element, one netip.ParsePrefix does not read, and an IPv4-mapped IPv6
// prefix, which no address the proxy checks is ever inside, since the proxy reads every address as
// the IPv4 address it holds.
func parseAirlockClusterRanges(value string) ([]netip.Prefix, error) {
	const setting = "BROKER_AIRLOCK_CLUSTER_RANGES"
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	elements := strings.Split(value, ",")
	ranges := make([]netip.Prefix, 0, len(elements))
	for i, element := range elements {
		text := strings.TrimSpace(element)
		if text == "" {
			return nil, fmt.Errorf("%s: element %d is empty; the setting is a comma-separated list of CIDR prefixes such as 34.118.224.0/20", setting, i+1)
		}
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, fmt.Errorf("%s: element %d is not a CIDR prefix such as 34.118.224.0/20", setting, i+1)
		}
		if prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("%s: element %d is an IPv4-mapped IPv6 prefix, which no address the proxy checks is ever inside; write it as an IPv4 prefix", setting, i+1)
		}
		ranges = append(ranges, prefix.Masked())
	}
	return ranges, nil
}

// requireClusterRanges is the check of BROKER_AIRLOCK_CLUSTER_RANGES against the cluster a broker
// with the airlock on runs in. KUBERNETES_SERVICE_HOST, which Kubernetes sets in every container
// to the ClusterIP of the cluster's kubernetes Service, says the broker runs in a cluster; empty
// or unset, the broker runs outside one and the ranges are optional. Set to a non-empty value,
// they are required and must hold that address: it is drawn from the cluster's Service range, so
// a list that does not hold it has missed the one range the proxy's fixed rules cannot refuse.
// Each refusal names both variables, and a KUBERNETES_SERVICE_HOST that is no address is not
// quoted.
func requireClusterRanges(ranges []netip.Prefix, serviceHost string) error {
	// Empty or unset is outside a cluster, and nothing else is: any non-empty value is a cluster,
	// a value of spaces among them, which is refused below as a missing setting or as no address.
	if serviceHost == "" {
		return nil
	}
	serviceHost = strings.TrimSpace(serviceHost)
	if len(ranges) == 0 {
		return errors.New("BROKER_AIRLOCK_CLUSTER_RANGES is required: BROKER_AIRLOCK_RULES_FILE turns the airlock on and KUBERNETES_SERVICE_HOST is set, so the broker runs in a cluster, and its proxy must refuse that cluster's own Service, pod and node ranges, which only this setting names")
	}
	host, err := netip.ParseAddr(serviceHost)
	if err != nil {
		return errors.New("KUBERNETES_SERVICE_HOST is not an IP address, so the broker cannot check that BROKER_AIRLOCK_CLUSTER_RANGES holds the cluster's Service range")
	}
	// The address the proxy reads (netaddr.Canonical): a zone is no part of whether it is inside
	// a range, and the refusal below repeats a parsed address and nothing after a zone's %.
	host = netaddr.Canonical(host)
	named := make([]string, len(ranges))
	for i, own := range ranges {
		if own.Contains(host) {
			return nil
		}
		named[i] = own.String()
	}
	return fmt.Errorf("KUBERNETES_SERVICE_HOST %s is inside no range BROKER_AIRLOCK_CLUSTER_RANGES names (%s): the setting misses the cluster's Service range, and the airlock proxy would dial the cluster's ClusterIPs",
		host, strings.Join(named, ", "))
}

// parseAirlockTestAzureAuthority reads BROKER_AIRLOCK_TEST_AZURE_AUTHORITY, the host the proxy
// holds every exchange to and compares the ASCII lower-cased host of a token request's
// connection with, exactly. It refuses, naming the value and the reason, a host rulehost.Refusal
// refuses, since a token request reaches the proxy only at a host an entry binds. On top of that
// grammar it refuses two hosts a rules entry may hold: a "*." suffix, which is no one host, and a
// capital letter, since the proxy compares the value as it is written with a lower-cased host. A
// value of any of those would refuse every exchange.
func parseAirlockTestAzureAuthority(value string) (string, error) {
	const setting = "BROKER_AIRLOCK_TEST_AZURE_AUTHORITY"
	host := strings.TrimSpace(value)
	if host == "" {
		return "", nil
	}
	if why := rulehost.Refusal(host); why != "" {
		return "", fmt.Errorf("%s must be a host name that an airlock rules entry can bind, got %q, which %s", setting, host, why)
	}
	if strings.HasPrefix(host, "*.") || strings.ToLower(host) != host {
		return "", fmt.Errorf("%s must be a host name in lower case, with no \"*.\", since the proxy compares it as written with one lower-cased host, got %q", setting, host)
	}
	return host, nil
}

// testOnlySwitch reads one of the local stack's test-only switches, the airlock's two and
// the judge's one: unset is off, and anything strconv.ParseBool does not read is refused rather
// than taken as off, since a misspelled value would otherwise turn a negative control into the
// ordinary run it is meant to be compared against.
func testOnlySwitch(setting, value string) (bool, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return false, nil
	}
	on, err := strconv.ParseBool(text)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", setting, text)
	}
	return on, nil
}

// parseExchangeWorkflows reads setting's whitespace-separated
// <repository>=<workflow path>=<ref>=<project>=<person>[=<session>] entries, the identities
// BROKER_EXCHANGE_WORKFLOWS lists, in order. It refuses, naming the entry: one not of that form or
// with a blank field, and a repository, workflow and ref listed twice, since one workflow run
// writes as one identity. exchange.NewSet is the second gate, and the repository's own form is its
// to refuse.
func parseExchangeWorkflows(setting, value string) ([]exchange.WorkflowEntry, error) {
	var entries []exchange.WorkflowEntry
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		fields, ok := exchangeFields(text, 6)
		if !ok || fields[0] == "" || fields[1] == "" || fields[2] == "" || fields[3] == "" || fields[4] == "" {
			return nil, fmt.Errorf("%s must be <repository>=<workflow path>=<ref>=<project>=<person>[=<session>] entries, the session everything after the fifth =, got %q", setting, text)
		}
		key := strings.Join(fields[:3], "=")
		if seen[key] {
			return nil, fmt.Errorf("%s names the workflow %s twice; one workflow run writes as one identity, so name it once", setting, key)
		}
		seen[key] = true
		entries = append(entries, exchange.WorkflowEntry{
			Repository: fields[0], Workflow: fields[1], Ref: fields[2],
			Project: fields[3], Person: fields[4], Session: fields[5],
		})
	}
	return entries, nil
}

// databasePasswordPlaceholder is the removed BROKER_DATABASE_PASSWORD's placeholder. Load refuses a
// BROKER_DATABASE_URL that names it, as it refuses the variable, rather than sign in with it as a
// literal password.
const databasePasswordPlaceholder = "${BROKER_DATABASE_PASSWORD}"

// rdsHostSuffix ends every Amazon RDS endpoint's host name, the hosts RDS IAM auth tokens sign in
// to.
const rdsHostSuffix = ".rds.amazonaws.com"

// databaseIAM reports whether databaseURL signs in by RDS IAM auth tokens: a postgres:// URL that
// names a user (in its user info or its query) and whose host is an RDS endpoint, for which pgx
// finds no password, whether in the URL or beneath it, from PGPASSWORD or a passfile. Such a URL
// is refused, naming the host and never the URL, unless pgx reads it as that one host alone,
// verified: sslmode=verify-full, which checks the server's certificate and that it names the
// host, against the roots an sslrootcert file holds. sslmode=require encrypts and verifies
// nothing (pgx sets InsecureSkipVerify), and verify-ca checks no name, so either would hand a
// 15-minute password to whoever answers on the path; sslrootcert=system verifies against the
// system trust store, which holds no RDS CA, so every sign-in would fail. Every other URL
// connects as given.
func databaseIAM(databaseURL string) (bool, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return false, nil
	}
	query := parsed.Query()
	if parsed.User.Username() == "" && query.Get("user") == "" {
		return false, nil
	}
	if _, hasPassword := parsed.User.Password(); hasPassword || query.Has("password") {
		return false, nil
	}
	var host string
	for _, named := range append(strings.Split(parsed.Host, ","), strings.Split(query.Get("host"), ",")...) {
		hostname := named
		if h, _, err := net.SplitHostPort(named); err == nil {
			hostname = h
		}
		if strings.HasSuffix(strings.ToLower(hostname), rdsHostSuffix) {
			host = hostname
			break
		}
	}
	if host == "" {
		return false, nil
	}
	refuse := func(why string) (bool, error) {
		return false, fmt.Errorf("BROKER_DATABASE_URL signs in to %s by RDS IAM token, a password to the database for 15 minutes, so it must verify that host: name it alone, with sslmode=verify-full and an sslrootcert file (the broker image ships the RDS CA bundle at /etc/ssl/rds/global-bundle.pem); %s", host, why)
	}
	conn, err := pgconn.ParseConfig(databaseURL)
	if err != nil {
		// A ParseConfigError's own text quotes the URL; what it wraps says what was wrong.
		var parseErr *pgconn.ParseConfigError
		if errors.As(err, &parseErr) && errors.Unwrap(parseErr) != nil {
			err = errors.Unwrap(parseErr)
		} else {
			err = errors.New("its connection settings do not parse")
		}
		return refuse("pgx cannot read it: " + err.Error())
	}
	// pgx signs in with the password it reads, which net/url can miss: a query pair holding a ';'
	// (net/url drops it), PGPASSWORD, or a passfile entry. A connection that has one signs in with
	// it, as given.
	if conn.Password != "" {
		return false, nil
	}
	// pgx tries the primary and then each fallback in turn: another host is a fallback, and so is
	// the plaintext retry sslmode=prefer (pgx's default) makes. Every attempt must be to the host,
	// verified.
	attempts := []*pgconn.FallbackConfig{{Host: conn.Host, Port: conn.Port, TLSConfig: conn.TLSConfig}}
	attempts = append(attempts, conn.Fallbacks...)
	for _, attempt := range attempts {
		if !strings.EqualFold(attempt.Host, host) {
			return refuse("it names another host too")
		}
	}
	for _, attempt := range attempts {
		if attempt.TLSConfig == nil || attempt.TLSConfig.InsecureSkipVerify || attempt.TLSConfig.ServerName != attempt.Host {
			return refuse("its sslmode is not verify-full")
		}
		if attempt.TLSConfig.RootCAs == nil {
			return refuse("it names no sslrootcert")
		}
	}
	// sslrootcert=system, from the URL or PGSSLROOTCERT, gives pgx Go's system root pool, which
	// holds no RDS CA, so the broker would boot and then fail every sign-in's TLS handshake. A
	// file holding exactly the system roots reads the same and fails the same way.
	if system, err := x509.SystemCertPool(); err == nil && conn.TLSConfig.RootCAs.Equal(system) {
		return refuse("its sslrootcert is the system trust store (sslrootcert=system), which holds no RDS CA, so every sign-in would fail")
	}
	return true, nil
}

// httpURLSetting refuses a setting whose value is no absolute http or https URL, naming the
// setting and quoting the value with its own credentials and query taken out (redact.URL). The
// settings held to this form are a page a machine opens in a browser, a proxy a command dials,
// and the two the judge calls — its model's endpoint and its Dispatch — and none of them works
// at all as a bare host and port.
func httpURLSetting(name, value string) error {
	if parsed, err := url.Parse(value); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL: %q", name, redact.URL(value))
	}
	return nil
}

func Load(getenv func(string) string) (Config, error) {
	for _, removed := range removedVars {
		if getenv(removed.name) != "" {
			return Config{}, fmt.Errorf("%s is removed; %s", removed.name, removed.reason)
		}
	}
	if strings.Contains(getenv("BROKER_DATABASE_URL"), databasePasswordPlaceholder) {
		return Config{}, fmt.Errorf("BROKER_DATABASE_URL names the removed %s placeholder; %s", databasePasswordPlaceholder, noDatabasePassword)
	}
	// The switch is read first, so off beside a sign-in setting is refused as that, before any
	// refusal of the setting on its own (BROKER_SIGNIN_DEVICE_AUDIENCE's, below) could name a
	// gap the operator never meant to fill.
	signInOff, err := parseSignInSwitch(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:                orDefault(getenv("BROKER_LISTEN_ADDR"), "127.0.0.1:13380"),
		MetricsListenAddr:         orDefault(getenv("BROKER_METRICS_LISTEN_ADDR"), "127.0.0.1:13382"),
		DatabaseURL:               getenv("BROKER_DATABASE_URL"),
		PublicURL:                 getenv("BROKER_PUBLIC_URL"),
		MachineLoginURL:           strings.TrimSpace(getenv("BROKER_MACHINE_LOGIN_URL")),
		EdgeListenAddr:            strings.TrimSpace(getenv("BROKER_EDGE_LISTEN_ADDR")),
		EdgeURL:                   strings.TrimSpace(getenv("BROKER_EDGE_URL")),
		EdgeMachineLoginURL:       strings.TrimSpace(getenv("BROKER_EDGE_MACHINE_LOGIN_URL")),
		SecretsStore:              orDefault(getenv("BROKER_SECRETS_STORE"), secrets.StoreAWS),
		SecretsPrefix:             getenv("BROKER_SECRETS_PREFIX"),
		AirlockRulesFile:          getenv("BROKER_AIRLOCK_RULES_FILE"),
		AirlockListenAddr:         orDefault(getenv("BROKER_AIRLOCK_LISTEN_ADDR"), "127.0.0.1:13381"),
		AirlockProxyURL:           strings.TrimSpace(getenv("BROKER_AIRLOCK_PROXY_URL")),
		AirlockCACertFile:         strings.TrimSpace(getenv("BROKER_AIRLOCK_CA_CERT_FILE")),
		AirlockCAKeyFile:          strings.TrimSpace(getenv("BROKER_AIRLOCK_CA_KEY_FILE")),
		AirlockTestGitHubAPIBase:  strings.TrimSpace(getenv("BROKER_AIRLOCK_TEST_GITHUB_API_BASE")),
		AirlockTestGoogleTokenURL: strings.TrimSpace(getenv("BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL")),
		AirlockTestUpstreamCAFile: strings.TrimSpace(getenv("BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE")),
		JudgeModelURL:             strings.TrimSpace(getenv("BROKER_JUDGE_MODEL_URL")),
		JudgeModel:                strings.TrimSpace(getenv("BROKER_JUDGE_MODEL")),
		JudgeModelProtocol:        orDefault(getenv("BROKER_JUDGE_MODEL_PROTOCOL"), "anthropic-messages"),
		JudgeModelAudience:        strings.TrimSpace(getenv("BROKER_JUDGE_MODEL_AUDIENCE")),
		JudgeModelAccount:         strings.TrimSpace(getenv(judgeModelAccountSetting)),
		JudgeDispatchURL:          strings.TrimSpace(getenv("BROKER_JUDGE_DISPATCH_URL")),
		JudgeDispatchTokenFile:    strings.TrimSpace(getenv("BROKER_JUDGE_DISPATCH_TOKEN_FILE")),
		ProjectAccessURL:          strings.TrimRight(strings.TrimSpace(getenv("BROKER_PROJECT_ACCESS_URL")), "/"),
		ProjectAccessTokenFile:    strings.TrimSpace(getenv("BROKER_PROJECT_ACCESS_TOKEN_FILE")),
		AzureTokenFile:            orDefault(getenv("BROKER_AZURE_TOKEN_FILE"), secrets.DefaultAzureTokenFile),
		AzureLoginSecret:          getenv("BROKER_AZURE_LOGIN_SECRET"),
		K8sOIDCIssuer:             getenv("BROKER_K8S_OIDC_ISSUER"),
		K8sOIDCAudience:           getenv("BROKER_K8S_OIDC_AUDIENCE"),
		GoogleOIDCIssuer:          orDefault(strings.TrimSpace(getenv("BROKER_GOOGLE_OIDC_ISSUER")), DefaultGoogleOIDCIssuer),
		GoogleOIDCAudience:        strings.TrimSpace(getenv("BROKER_GOOGLE_OIDC_AUDIENCE")),
		EnvoyURL:                  getenv("BROKER_ENVOY_URL"),
		TrustedProxyHeader:        getenv("BROKER_TRUSTED_PROXY_HEADER"),
		SignInOff:                 signInOff,
	}
	// The local stack's test-only settings, the airlock's and the judge's: each is read
	// whatever the rules file says, since the two minter bases are read with no airlock at all.
	// cmd/cipherd refuses every one of them on a production start; what is refused here is a
	// value the broker could not act on.
	var testSwitchErr error
	if cfg.AirlockTestDialAllow, testSwitchErr = parseAirlockTestDialAllow(getenv("BROKER_AIRLOCK_TEST_DIAL_ALLOW")); testSwitchErr != nil {
		return Config{}, testSwitchErr
	}
	if cfg.AirlockTestResolve, testSwitchErr = parseAirlockTestResolve(getenv("BROKER_AIRLOCK_TEST_RESOLVE")); testSwitchErr != nil {
		return Config{}, testSwitchErr
	}
	if cfg.AirlockTestKeepValues, testSwitchErr = testOnlySwitch("BROKER_AIRLOCK_TEST_KEEP_VALUES", getenv("BROKER_AIRLOCK_TEST_KEEP_VALUES")); testSwitchErr != nil {
		return Config{}, testSwitchErr
	}
	if cfg.AirlockTestNoKind, testSwitchErr = testOnlySwitch("BROKER_AIRLOCK_TEST_NO_KIND", getenv("BROKER_AIRLOCK_TEST_NO_KIND")); testSwitchErr != nil {
		return Config{}, testSwitchErr
	}
	if cfg.AirlockTestAzureAuthority, testSwitchErr = parseAirlockTestAzureAuthority(getenv("BROKER_AIRLOCK_TEST_AZURE_AUTHORITY")); testSwitchErr != nil {
		return Config{}, testSwitchErr
	}
	if cfg.JudgeTestSkipChecks, testSwitchErr = testOnlySwitch("BROKER_JUDGE_TEST_SKIP_CHECKS", getenv("BROKER_JUDGE_TEST_SKIP_CHECKS")); testSwitchErr != nil {
		return Config{}, testSwitchErr
	}
	if cfg.AzureLoginSecret != "" {
		if _, err := policy.NameToSlug(cfg.AzureLoginSecret); err != nil {
			return Config{}, fmt.Errorf("BROKER_AZURE_LOGIN_SECRET: %w", err)
		}
	}
	if cfg.SecretsStore != secrets.StoreAWS && cfg.SecretsStore != secrets.StoreGCP {
		return Config{}, fmt.Errorf("BROKER_SECRETS_STORE must be %s or %s, got %q", secrets.StoreAWS, secrets.StoreGCP, cfg.SecretsStore)
	}
	// Each store takes its own key variable and refuses the other's, so a deployment moved
	// between stores cannot keep the old key set and silently mean nothing by it.
	keyVar, otherKeyVar := "BROKER_SECRETS_KMS_KEY_ARN", "BROKER_SECRETS_KMS_KEY_NAME"
	if cfg.SecretsStore == secrets.StoreGCP {
		keyVar, otherKeyVar = otherKeyVar, keyVar
	}
	if getenv(otherKeyVar) != "" {
		return Config{}, fmt.Errorf("%s is not read with BROKER_SECRETS_STORE=%s; set %s instead", otherKeyVar, cfg.SecretsStore, keyVar)
	}
	cfg.SecretsKMSKey = getenv(keyVar)
	for _, req := range []struct{ name, value string }{
		{"BROKER_DATABASE_URL", cfg.DatabaseURL}, {"BROKER_PUBLIC_URL", cfg.PublicURL},
		{"BROKER_SECRETS_PREFIX", cfg.SecretsPrefix}, {keyVar, cfg.SecretsKMSKey},
	} {
		if strings.TrimSpace(req.value) == "" {
			return Config{}, fmt.Errorf("%s is required", req.name)
		}
	}
	if cfg.DatabaseIAM, err = databaseIAM(cfg.DatabaseURL); err != nil {
		return Config{}, err
	}
	// publicURL is BROKER_PUBLIC_URL parsed, held to an absolute URL with no path here and to an
	// http or https scheme by Load's last check.
	publicURL, parseErr := url.Parse(cfg.PublicURL)
	if parseErr != nil || publicURL.Scheme == "" || publicURL.Host == "" || publicURL.Path != "" {
		return Config{}, fmt.Errorf("BROKER_PUBLIC_URL must be an absolute URL with no path: %q", redact.URL(cfg.PublicURL))
	}
	// Optional, and still held to its form when it is given: a page the broker cannot build an
	// absolute URL for is a verification_uri no machine could follow, which is worse than the
	// device grant the broker simply does not serve when the setting is unset.
	if cfg.MachineLoginURL != "" {
		if err := httpURLSetting("BROKER_MACHINE_LOGIN_URL", cfg.MachineLoginURL); err != nil {
			return Config{}, err
		}
	}
	// The edge's settings, held here beside BROKER_MACHINE_LOGIN_URL, whose device grant the edge
	// serves (loadEdge), and the Workspace's other domains, which only the edge reads.
	if err := loadEdge(&cfg); err != nil {
		return Config{}, err
	}
	if cfg.SignInWorkspaceDomains, err = parseWorkspaceDomains(getenv(workspaceDomainsSetting)); err != nil {
		return Config{}, err
	}
	// The broker cluster's own ranges, read whatever the rules file says: a list the broker could
	// not act on is a mistake in the deployment whether or not its airlock is on today.
	if cfg.AirlockClusterRanges, err = parseAirlockClusterRanges(getenv("BROKER_AIRLOCK_CLUSTER_RANGES")); err != nil {
		return Config{}, err
	}
	// A broker that reads the airlock's rules but cannot proxy would hold back the value of
	// every name the file names and release nothing in its place, so the three settings the
	// proxy cannot run without are required with the rules file, and the refusal names the ones
	// missing. With no rules file there is no proxy and all four are ignored.
	if cfg.AirlockRulesFile != "" {
		// One value per airlockProxySettings entry, read in that order, so the missing ones
		// are named in the order the message naming them uses and one list is walked rather
		// than two that could part.
		values := []string{cfg.AirlockProxyURL, cfg.AirlockCACertFile, cfg.AirlockCAKeyFile}
		var missing []string
		for i, setting := range airlockProxySettings {
			if values[i] == "" {
				missing = append(missing, setting)
			}
		}
		if len(missing) > 0 {
			return Config{}, fmt.Errorf("BROKER_AIRLOCK_RULES_FILE names a rules file, so the broker proxies every secret the file names and needs %s; missing %s",
				strings.Join(airlockProxySettings, ", "), strings.Join(missing, ", "))
		}
		// And the proxy's URL held to its form, as BROKER_MACHINE_LOGIN_URL above is: every
		// release of a name the rules hold back carries this string, and a command's own
		// client reads it as the proxy to dial. A value with no scheme or no host
		// ("proxy:13381") boots and then fails every airlock run one command at a time, with
		// a refusal in the CLI rather than in the pod that was configured wrong.
		if err := httpURLSetting("BROKER_AIRLOCK_PROXY_URL", cfg.AirlockProxyURL); err != nil {
			return Config{}, err
		}
		// And, in a cluster, the cluster's own ranges, which no fixed rule of the proxy covers
		// (requireClusterRanges).
		if err := requireClusterRanges(cfg.AirlockClusterRanges, getenv("KUBERNETES_SERVICE_HOST")); err != nil {
			return Config{}, err
		}
	}
	// The judge's five settings are all-or-none, as the sign-in client's four are: with none of
	// them set the broker has no judge and every human-tier request waits for a person, and
	// with some of them set the start is refused naming the ones missing, since a judge given
	// four of the five would decide nothing and say so one request at a time.
	//
	// BROKER_JUDGE_MODEL_PROTOCOL has a default, so it is never one of the missing ones; it is
	// still counted as naming a judge, so a deployment that sets it alone is refused rather
	// than read as one that meant no judge. On its own it configures nothing — it says which
	// shape an endpoint that was never named speaks — and that is the same mistake as the four
	// of five above, which the broker refuses rather than runs past. BROKER_JUDGE_MODEL_ACCOUNT
	// is optional, so it is never one of the missing ones either, and it counts as naming a judge
	// for the same reason: on its own it names whose token a judge that was never configured
	// would present. Its refusal names it and its value in front of the five, since it is the
	// one setting that named the judge.
	//
	// One value per JudgeSettings entry, read in that order, so the missing ones are named in
	// the order every message naming them uses and one list is walked rather than two that
	// could part.
	judgeValues := []string{cfg.JudgeModelURL, cfg.JudgeModel, cfg.JudgeModelAudience, cfg.JudgeDispatchURL, cfg.JudgeDispatchTokenFile}
	var judgeMissing []string
	for i, setting := range JudgeSettings {
		if judgeValues[i] == "" {
			judgeMissing = append(judgeMissing, setting)
		}
	}
	switch {
	case len(judgeMissing) == len(JudgeSettings) && getenv(judgeModelProtocolSetting) == "" && cfg.JudgeModelAccount == "":
		// No judge, which is every deployment until one is configured.
	case len(judgeMissing) == len(JudgeSettings) && cfg.JudgeModelAccount != "":
		return Config{}, fmt.Errorf("%s is set to %q, the service account whose identity token a model judge presents, and no judge is configured: the broker's model judge needs %s; missing %s",
			judgeModelAccountSetting, cfg.JudgeModelAccount, strings.Join(JudgeSettings, ", "), strings.Join(judgeMissing, ", "))
	case len(judgeMissing) > 0:
		return Config{}, fmt.Errorf("the broker's model judge needs %s; missing %s",
			strings.Join(JudgeSettings, ", "), strings.Join(judgeMissing, ", "))
	default:
		// The two URLs held to their form, as BROKER_MACHINE_LOGIN_URL and the proxy's are: a
		// value with no scheme or no host is one no call could be made to, and a judge that
		// boots and then fails every call decides nothing while looking configured.
		for _, u := range []struct{ setting, value string }{
			{judgeModelURLSetting, cfg.JudgeModelURL},
			{judgeDispatchURLSetting, cfg.JudgeDispatchURL},
		} {
			if err := httpURLSetting(u.setting, u.value); err != nil {
				return Config{}, err
			}
		}
		if !slices.Contains(judgeModelProtocols, cfg.JudgeModelProtocol) {
			return Config{}, fmt.Errorf("%s must be one of %s, got %q",
				judgeModelProtocolSetting, strings.Join(judgeModelProtocols, ", "), cfg.JudgeModelProtocol)
		}
		// The account is held to the rule BROKER_SERVICE_ACCOUNT_SECRETS' accounts are held to
		// (parseServiceAccountSecrets), for the same reasons: a value that is no one service
		// account's address would boot and then hand every request the judge holds to its person
		// on a refused mint, and a service agent is an account of Google's own that the broker
		// must not mint for.
		if cfg.JudgeModelAccount != "" {
			if !secrets.ValidServiceAccountEmail(cfg.JudgeModelAccount) {
				return Config{}, fmt.Errorf("%s must name one user-managed Google service account, <account id>@<project id>.iam.gserviceaccount.com in lowercase, got %q", judgeModelAccountSetting, cfg.JudgeModelAccount)
			}
			if googleServiceAgentPattern.MatchString(cfg.JudgeModelAccount) {
				return Config{}, fmt.Errorf("%s names a Google-managed service agent, got %q: Google's own services run as it and the broker must not be able to mint its token; name a user-managed account of your own", judgeModelAccountSetting, cfg.JudgeModelAccount)
			}
		}
	}
	// A judge decides a request that waits for a person, and with sign-in off no request waits:
	// every one is refused at once (requests.Machine.SignInOff). So a judge beside off is a
	// setting that configures nothing, refused as the broker refuses every other such setting.
	// A judge named in part, or by its protocol alone, was refused above.
	if cfg.SignInOff && len(judgeMissing) == 0 {
		return Config{}, fmt.Errorf("%s=%s refuses at once every request that would wait for a person, so the broker's model judge, which decides such a request, would never run: unset the judge's settings (%s) or %s",
			signInSwitchSetting, signInOffValue, strings.Join(JudgeSettings, ", "), signInSwitchSetting)
	}
	// The project-access settings are both or neither, as the judge's are: a URL with no token to
	// present would be refused at every call, and a token with nowhere to present it asks nothing,
	// so either alone is a deployment that meant to turn the check on and did not.
	switch {
	case cfg.ProjectAccessURL == "" && cfg.ProjectAccessTokenFile == "":
		// No project-access check, which is every deployment with no private project.
	case cfg.ProjectAccessTokenFile == "":
		return Config{}, errors.New("BROKER_PROJECT_ACCESS_URL is set and BROKER_PROJECT_ACCESS_TOKEN_FILE is not: the broker presents its own projected ServiceAccount token from that file on every call to Dispatch, so set both or neither")
	case cfg.ProjectAccessURL == "":
		return Config{}, errors.New("BROKER_PROJECT_ACCESS_TOKEN_FILE is set and BROKER_PROJECT_ACCESS_URL is not: that URL names the Dispatch the broker asks about projects, so set both or neither")
	default:
		if err := httpURLSetting("BROKER_PROJECT_ACCESS_URL", cfg.ProjectAccessURL); err != nil {
			return Config{}, err
		}
	}
	switch cfg.SecretsStore {
	case secrets.StoreAWS:
		if !strings.HasSuffix(cfg.SecretsPrefix, "/") || strings.HasPrefix(cfg.SecretsPrefix, "/") || strings.ContainsFunc(cfg.SecretsPrefix, unicode.IsSpace) {
			return Config{}, fmt.Errorf("BROKER_SECRETS_PREFIX must be a Secrets Manager name prefix ending in /, such as production/agent-secrets/, got %q", cfg.SecretsPrefix)
		}
		if !secrets.ValidAWSKeyARN(cfg.SecretsKMSKey) {
			return Config{}, fmt.Errorf("BROKER_SECRETS_KMS_KEY_ARN must be a KMS key ARN, arn:aws:kms:<region>:<account>:key/<key id>, got %q", cfg.SecretsKMSKey)
		}
	case secrets.StoreGCP:
		if err := secrets.ValidGCPPrefix(cfg.SecretsPrefix); err != nil {
			return Config{}, fmt.Errorf("BROKER_SECRETS_PREFIX: %w", err)
		}
		if err := secrets.ValidGCPKeyName(cfg.SecretsKMSKey); err != nil {
			return Config{}, fmt.Errorf("BROKER_SECRETS_KMS_KEY_NAME: %w", err)
		}
	}
	services, err := parseServiceAccounts("BROKER_SERVICES", getenv("BROKER_SERVICES"))
	if err != nil {
		return Config{}, err
	}
	cfg.ServicesByAccount = servicesByAccount(services)
	logins, err := parseServiceAccounts("BROKER_SERVICE_LOGINS", getenv("BROKER_SERVICE_LOGINS"))
	if err != nil {
		return Config{}, err
	}
	// An account BROKER_SERVICES already binds may be listed here too, which is a pod of that
	// service's own account logging the service in with its own projected token and enrolling
	// itself (agent-secrets enroll --pod). What narrows such a login to one pod is
	// enroll.Service.podUnderTokenLogin rather than this loader, as the ServiceLogins field
	// doc says. An account BROKER_POD_OPERATORS maps is the exception: mappedAccountRefusal,
	// below, refuses it.
	cfg.ServiceLogins = servicesByAccount(logins)
	if cfg.SessionAccountPairs, err = parseSessionAccountPairs("BROKER_SESSION_ACCOUNT_PAIRS", getenv("BROKER_SESSION_ACCOUNT_PAIRS")); err != nil {
		return Config{}, err
	}
	if cfg.ServiceAccountSecrets, err = parseServiceAccountSecrets("BROKER_SERVICE_ACCOUNT_SECRETS", getenv("BROKER_SERVICE_ACCOUNT_SECRETS")); err != nil {
		return Config{}, err
	}
	cfg.SignInDeviceAudience = strings.TrimSpace(getenv("BROKER_SIGNIN_DEVICE_AUDIENCE"))
	if cfg.GoogleOIDCAudience == "" {
		for _, setting := range []string{"BROKER_VM_SERVICES", "BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES", "BROKER_VM_EXCHANGE_ACCOUNTS", "BROKER_SIGNIN_DEVICE_AUDIENCE"} {
			if strings.TrimSpace(getenv(setting)) != "" {
				return Config{}, fmt.Errorf("%s needs BROKER_GOOGLE_OIDC_AUDIENCE, whose Google identity tokens it configures", setting)
			}
		}
	}
	vmServices, err := parseVMServiceAccounts("BROKER_VM_SERVICES", getenv("BROKER_VM_SERVICES"))
	if err != nil {
		return Config{}, err
	}
	if cfg.SignInDeviceAudience != "" && len(vmServices) == 0 {
		return Config{}, fmt.Errorf("BROKER_SIGNIN_DEVICE_AUDIENCE needs BROKER_VM_SERVICES: a person's device sign-in requires a listed machine's Google identity token as its actor")
	}
	for _, entry := range vmServices {
		if cfg.VMServicesByAccount == nil {
			cfg.VMServicesByAccount = map[string]string{}
		}
		cfg.VMServicesByAccount[entry.account] = entry.service
	}
	projects, err := parseVMAccountValues("BROKER_VM_PROJECTS", getenv("BROKER_VM_PROJECTS"), "project id", googleProjectIDPattern.MatchString)
	if err != nil {
		return Config{}, err
	}
	instances, err := parseVMAccountValues("BROKER_VM_INSTANCES", getenv("BROKER_VM_INSTANCES"), "instance-name glob", validInstancePattern)
	if err != nil {
		return Config{}, err
	}
	// BROKER_VM_PROJECTS and BROKER_VM_INSTANCES are locals of Load: the entry each one pins
	// (ExchangeGoogleAccounts' CloudProject and Instance, built below) is the only thing that
	// reads them, and no field of Config holds them. What each one means is documented in
	// cmd/cipherd/main.go, because broker-refgen takes a setting's description from a Config
	// field's own comment or from a comment in cmd/cipherd, and these two have no field.
	vmProjects, err := vmAccountMap("BROKER_VM_PROJECTS", projects, cfg.VMServicesByAccount)
	if err != nil {
		return Config{}, err
	}
	vmInstancePatterns, err := vmAccountMap("BROKER_VM_INSTANCES", instances, cfg.VMServicesByAccount)
	if err != nil {
		return Config{}, err
	}
	grants, err := parseVMExchangeGrants("BROKER_VM_EXCHANGE_ACCOUNTS", getenv("BROKER_VM_EXCHANGE_ACCOUNTS"))
	if err != nil {
		return Config{}, err
	}
	granted := map[string]vmExchangeGrant{}
	for _, grant := range grants {
		if _, listed := cfg.VMServicesByAccount[grant.account]; !listed {
			return Config{}, fmt.Errorf("BROKER_VM_EXCHANGE_ACCOUNTS names %s, got %q, but BROKER_VM_SERVICES does not list it; a person and a project are given to a listed VM account", grant.account, grant.text)
		}
		granted[grant.account] = grant
	}
	for _, entry := range vmServices {
		// Each pin is optional and the two go together: an account pinned to a project but no
		// instance name, or the other way round, says nothing clear about which machines it
		// means, so it is refused here rather than read as either "every instance of that
		// project" or "that name in any project".
		project, pinnedProject := vmProjects[entry.account]
		instance, pinnedInstance := vmInstancePatterns[entry.account]
		if pinnedProject != pinnedInstance {
			named, missing := "BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"
			if pinnedInstance {
				named, missing = missing, named
			}
			return Config{}, fmt.Errorf("%s names %s but %s does not, got %q: an account is pinned to a Compute Engine project and an instance-name pattern together, or to neither, so that which machines it takes is clear", named, entry.account, missing, entry.text)
		}
		machine := exchange.GoogleEntry{
			Account: entry.account, Service: entry.service,
			CloudProject: project, Instance: instance,
		}
		if grant, found := granted[entry.account]; found {
			machine.Project, machine.Person, machine.Session = grant.project, grant.person, grant.session
		}
		cfg.ExchangeGoogleAccounts = append(cfg.ExchangeGoogleAccounts, machine)
	}
	issuer, audience, err := oidc.ConfigFromEnv(getenv, "BROKER_K8S_OIDC_ISSUER", "BROKER_K8S_OIDC_AUDIENCE")
	if err != nil {
		return Config{}, err
	}
	cfg.K8sOIDCIssuer, cfg.K8sOIDCAudience = issuer, audience
	if len(cfg.ServiceLogins) > 0 && cfg.K8sOIDCIssuer == "" {
		return Config{}, fmt.Errorf("BROKER_SERVICE_LOGINS needs BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE, whose tokens its accounts log in with")
	}
	callers, err := parseAPIAccounts("BROKER_API_ACCOUNTS", getenv("BROKER_API_ACCOUNTS"))
	if err != nil {
		return Config{}, err
	}
	for _, entry := range callers {
		// A caller's name is an authority and not a label — it decides which of the broker's
		// named-caller routes that account reaches — so a name the broker's own table does not
		// know is refused here, rather than listed as a caller that reaches nothing.
		account, known := api.AccountFor(entry.caller)
		if !known {
			return Config{}, fmt.Errorf("BROKER_API_ACCOUNTS names the caller %q, which this broker does not know, got %q: its callers are %s", entry.caller, entry.text, strings.Join(api.KnownCallers(), ", "))
		}
		if entry.google {
			if cfg.GoogleAPIAccounts == nil {
				cfg.GoogleAPIAccounts = map[string]api.Account{}
			}
			cfg.GoogleAPIAccounts[entry.account] = account
			continue
		}
		if cfg.APIAccounts == nil {
			cfg.APIAccounts = map[string]api.Account{}
		}
		cfg.APIAccounts[entry.account] = account
	}
	// Each kind of entry needs the issuer whose tokens its callers present, and only that one: a
	// deployment whose callers are all Google accounts is told about no cluster, and one whose
	// callers are all Kubernetes accounts needs no Google audience.
	if len(cfg.APIAccounts) > 0 && cfg.K8sOIDCIssuer == "" {
		return Config{}, fmt.Errorf("BROKER_API_ACCOUNTS names a Kubernetes service account, which needs BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE, whose tokens its callers present")
	}
	if len(cfg.GoogleAPIAccounts) > 0 && cfg.GoogleOIDCAudience == "" {
		return Config{}, fmt.Errorf("BROKER_API_ACCOUNTS names a Google service account, which needs BROKER_GOOGLE_OIDC_AUDIENCE, the audience its caller's Google identity token must carry")
	}
	// The audience is for a Google caller of either kind, and needs one: with neither, the broker
	// would read Google's keys at boot and then accept no Google identity token at all.
	if cfg.GoogleOIDCAudience != "" && len(cfg.GoogleAPIAccounts) == 0 && len(cfg.VMServicesByAccount) == 0 {
		return Config{}, fmt.Errorf("BROKER_GOOGLE_OIDC_AUDIENCE needs a Google caller: a google: entry in BROKER_API_ACCOUNTS or a VM service in BROKER_VM_SERVICES; with neither, no Google identity token for that audience could ever be accepted")
	}
	githubIssuer, githubAudience, err := oidc.ConfigFromEnv(getenv, "BROKER_EXCHANGE_GITHUB_ISSUER", "BROKER_EXCHANGE_GITHUB_AUDIENCE")
	if err != nil {
		return Config{}, err
	}
	cfg.ExchangeGitHubIssuer, cfg.ExchangeGitHubAudience = githubIssuer, githubAudience
	cfg.ExchangeK8sAudience = strings.TrimSpace(getenv("BROKER_EXCHANGE_K8S_AUDIENCE"))
	if cfg.ExchangeK8sAudience != "" && cfg.K8sOIDCIssuer == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_K8S_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, the cluster whose pods carry it")
	}
	if cfg.ExchangeAccounts, err = parseExchangeAccounts("BROKER_EXCHANGE_ACCOUNTS", getenv("BROKER_EXCHANGE_ACCOUNTS")); err != nil {
		return Config{}, err
	}
	if len(cfg.ExchangeAccounts) > 0 && cfg.ExchangeK8sAudience == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_ACCOUNTS needs BROKER_K8S_OIDC_ISSUER and BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present")
	}
	if cfg.ExchangeReadAccounts, err = parseExchangeReadAccounts("BROKER_EXCHANGE_READ_ACCOUNTS", getenv("BROKER_EXCHANGE_READ_ACCOUNTS")); err != nil {
		return Config{}, err
	}
	if len(cfg.ExchangeReadAccounts) > 0 {
		if cfg.ExchangeK8sAudience == "" {
			return Config{}, fmt.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS needs BROKER_K8S_OIDC_ISSUER and BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present")
		}
		// One account mints one kind of token: listed in both settings, it would have two
		// answers, a token that writes as its person and one that only reads. exchange.NewSet
		// refuses it too, but only this refusal can name both settings.
		writers := make(map[string]bool, len(cfg.ExchangeAccounts))
		for _, entry := range cfg.ExchangeAccounts {
			writers[entry.Account] = true
		}
		for _, entry := range cfg.ExchangeReadAccounts {
			if writers[entry.Account] {
				return Config{}, fmt.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS and BROKER_EXCHANGE_ACCOUNTS both name the service account %s; one account mints one kind of token, a read-only one or one that writes as its person, so name it in one of them", entry.Account)
			}
		}
	}
	if cfg.ExchangeWorkflows, err = parseExchangeWorkflows("BROKER_EXCHANGE_WORKFLOWS", getenv("BROKER_EXCHANGE_WORKFLOWS")); err != nil {
		return Config{}, err
	}
	if len(cfg.ExchangeWorkflows) > 0 && cfg.ExchangeGitHubIssuer == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_WORKFLOWS needs BROKER_EXCHANGE_GITHUB_ISSUER and BROKER_EXCHANGE_GITHUB_AUDIENCE, whose tokens its runs present")
	}
	if cfg.EnvoyURL != "" {
		if cfg.EnvoyAccountTokenFile = strings.TrimSpace(getenv("BROKER_ENVOY_ACCOUNT_TOKEN_FILE")); cfg.EnvoyAccountTokenFile == "" {
			return Config{}, errors.New("BROKER_ENVOY_URL needs BROKER_ENVOY_ACCOUNT_TOKEN_FILE, the broker's own projected ServiceAccount token, which it presents to the listener")
		}
	}
	if cfg.NATS, err = loadNATSCallout(getenv, cfg.K8sOIDCIssuer); err != nil {
		return Config{}, err
	}
	// The registration route's two settings, read after every other audience of the cluster
	// issuer's tokens, so the registration audience is compared with each of them.
	if cfg.PodRegistrars, err = parsePodRegistrars("BROKER_POD_REGISTRARS", getenv("BROKER_POD_REGISTRARS")); err != nil {
		return Config{}, err
	}
	if len(cfg.PodRegistrars) > 0 && cfg.K8sOIDCIssuer == "" {
		return Config{}, errors.New("BROKER_POD_REGISTRARS needs BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE, whose tokens its registrars log in with and its pods' tokens are verified against")
	}
	cfg.PodRegistrationAudience = strings.TrimSpace(getenv("BROKER_POD_REGISTRATION_AUDIENCE"))
	if cfg.PodRegistrationAudience != "" {
		if cfg.K8sOIDCIssuer == "" {
			return Config{}, errors.New("BROKER_POD_REGISTRATION_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, the cluster whose pods' tokens carry it")
		}
		// Each other audience a token of the cluster's issuer is accepted at, by the route that
		// accepts it: equal to one of them, a token minted for registering a pod would be
		// spendable there too, and one minted for there would register a pod.
		for _, other := range []struct{ setting, audience string }{
			{"BROKER_K8S_OIDC_AUDIENCE", cfg.K8sOIDCAudience},
			{"BROKER_EXCHANGE_K8S_AUDIENCE", cfg.ExchangeK8sAudience},
			{"BROKER_NATS_K8S_AUDIENCE", cfg.NATS.K8sAudience},
		} {
			if other.audience == cfg.PodRegistrationAudience {
				return Config{}, fmt.Errorf("BROKER_POD_REGISTRATION_AUDIENCE is %q, which %s is too: a token of the registration audience is accepted at POST /v1/oauth/exchange/pod-sessions alone, so give it an audience no other setting names", cfg.PodRegistrationAudience, other.setting)
			}
		}
	}
	// BROKER_POD_OPERATORS is read after every setting mappedAccountRefusal checks it against, so
	// a refusal added there reads a setting this function has already read.
	if cfg.PodOperators, err = parsePodOperators("BROKER_POD_OPERATORS", getenv("BROKER_POD_OPERATORS")); err != nil {
		return Config{}, err
	}
	if err := mappedAccountRefusal(&cfg); err != nil {
		return Config{}, err
	}
	var sessionSeconds int
	ints := []struct {
		name     string
		dst      *int
		def      int
		min, max int
	}{
		{"BROKER_LEASE_SECONDS", &cfg.LeaseSeconds, 900, 1, 3600},
		{"BROKER_SESSION_CARRY_SECONDS", &cfg.SessionCarrySeconds, 43200, 0, 86400},
		{"BROKER_PROOF_SKEW_SECONDS", &cfg.ProofSkewSeconds, 60, 1, 300},
		{"BROKER_ACCESS_TOKEN_SECONDS", &cfg.AccessTokenSeconds, 900, 1, 3600},
		{"BROKER_LAUNCHER_CREDENTIAL_SECONDS", &cfg.LauncherCredentialSeconds, 2592000, 1, 2592000},
		{"BROKER_SWEEP_SECONDS", &cfg.SweepSeconds, 5, 1, 60},
		{"BROKER_DRAIN_SECONDS", &cfg.DrainSeconds, 5, 0, 120},
		{"BROKER_AIRLOCK_IDLE_SECONDS", &cfg.AirlockIdleSeconds, 300, 10, 3600},
		{"BROKER_SIGNIN_SESSION_SECONDS", &sessionSeconds, 86400, 1, 86400},
	}
	for _, i := range ints {
		raw := getenv(i.name)
		if raw == "" {
			*i.dst = i.def
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < i.min || n > i.max {
			return Config{}, fmt.Errorf("%s must be a whole number between %d and %d, got %q", i.name, i.min, i.max, raw)
		}
		*i.dst = n
	}
	if !cfg.SignInOff {
		if cfg.SignIn, err = parseSignIn(getenv, time.Duration(sessionSeconds)*time.Second); err != nil {
			return Config{}, err
		}
	}
	// A gap of up to leasetime.NotServingSweeps sweeps still reads as a broker serving, so none
	// of it is given back and a lease that ran out inside it is simply over. Surviving one is
	// not one wait but the whole retry schedule of whichever renewer holds the lease, each
	// renewal the outage refuses failing at whatever instant from at once to the client timeout
	// costs the renewer most, which leasetime walks for both of them.
	//
	// This refuses pairs an older broker booted: the rule it replaced counted only two thirds
	// of the lease, so a deployment whose BROKER_LEASE_SECONDS is below the floor this prints
	// has to raise it before the broker rolls, which operate.md says too.
	lease, sweep := time.Duration(cfg.LeaseSeconds)*time.Second, time.Duration(cfg.SweepSeconds)*time.Second
	if model := leasetime.Production(); !model.Survives(lease, sweep) {
		return Config{}, fmt.Errorf("BROKER_LEASE_SECONDS (%d) is too short for BROKER_SWEEP_SECONDS (%d): a session must outlive a gap of %d s that still counts as a broker serving and the time its renewer then takes to renew. The shortest lease that survives both renewers at this sweep is %d s.",
			cfg.LeaseSeconds, cfg.SweepSeconds, leasetime.NotServingSweeps*cfg.SweepSeconds, int(model.Shortest(sweep)/time.Second))
	}
	// BROKER_PUBLIC_URL's scheme, http or https and nothing else: every proof, request object and
	// metadata endpoint the broker names is under this URL, which its clients reach over HTTP.
	// approval.New refuses any other scheme too, but a broker with BROKER_SIGNIN=off builds no
	// page, so the rule is held here. It is Load's last check, so every other refusal comes first,
	// in the order it always did, and only a configuration the page would have refused is refused
	// here. The scheme alone is printed: it holds no credential.
	if publicURL.Scheme != "https" && publicURL.Scheme != "http" {
		return Config{}, fmt.Errorf("BROKER_PUBLIC_URL must be an http or https URL, got the scheme %q", publicURL.Scheme)
	}
	return cfg, nil
}

// parseSignInSwitch reads BROKER_SIGNIN: unset (or empty) is a broker that signs people in,
// exactly off one that signs nobody in, and any other value, a word a person would read as off
// included, refuses naming the setting and the value, so a misspelled off never starts a broker
// whose sign-in its operator thought was off, nor one whose sign-in they thought they had kept.
// off refuses beside every setting signInOffRefuses lists that is set, naming each one: a
// deployment that names a sign-in and turns sign-in off has said two things, and the broker
// does not pick one for it. A setting holding only spaces counts as unset, as parseSignIn reads
// it.
func parseSignInSwitch(getenv func(string) string) (bool, error) {
	switch value := getenv(signInSwitchSetting); value {
	case "":
		return false, nil
	case signInOffValue:
	default:
		return false, fmt.Errorf("%s must be %s or unset, got %q", signInSwitchSetting, signInOffValue, value)
	}
	var set []string
	for _, setting := range signInOffRefuses {
		if strings.TrimSpace(getenv(setting)) != "" {
			set = append(set, setting)
		}
	}
	if len(set) > 0 {
		verb := "is"
		if len(set) > 1 {
			verb = "are"
		}
		return false, fmt.Errorf("%s=%s turns the broker's person sign-in off, and %s %s set: set %s=%s or the sign-in settings, not both",
			signInSwitchSetting, signInOffValue, strings.Join(set, ", "), verb, signInSwitchSetting, signInOffValue)
	}
	return true, nil
}

// parseSignIn reads the broker's sign-in client, its session lasting sessionLifetime, the
// BROKER_SIGNIN_SESSION_SECONDS Load read. Load calls it unless BROKER_SIGNIN is off, and then
// all four of the client's settings are required, since a broker that signs people in and that
// nobody can sign in to is a broker on which no secret request is ever approved. It refuses: any
// of them missing, naming each one; an issuer that is not an absolute https URL, or http to a
// loopback host; and an unreadable or blank secret file. No refusal carries the secret.
func parseSignIn(getenv func(string) string, sessionLifetime time.Duration) (approval.SignIn, error) {
	signIn := approval.SignIn{SessionLifetime: sessionLifetime}
	// One destination per SignInSettings entry, read in that order, so the missing ones are named
	// in the order every message naming them uses. The secret's path is held aside rather than
	// kept in the SignIn, whose field carries the secret itself.
	var secretPath string
	into := []*string{&signIn.Issuer, &signIn.ClientID, &secretPath, &signIn.Domain}
	var missing []string
	for i, setting := range SignInSettings {
		*into[i] = strings.TrimSpace(getenv(setting))
		if *into[i] == "" {
			missing = append(missing, setting)
		}
	}
	if len(missing) > 0 {
		return approval.SignIn{}, fmt.Errorf("the broker's approval page needs %s; missing %s", strings.Join(SignInSettings, ", "), strings.Join(missing, ", "))
	}
	if !signInIssuerOK(signIn.Issuer) {
		return approval.SignIn{}, fmt.Errorf("%s must be an absolute https URL, or http to a loopback host, got %q", signInIssuerSetting, signIn.Issuer)
	}
	var err error
	if signIn.ClientSecret, err = secretFile(signInClientSecretSetting, secretPath); err != nil {
		return approval.SignIn{}, err
	}
	return signIn, nil
}

// signInIssuerOK reports whether issuer is an absolute https URL, or an http one whose host is
// localhost or a loopback address: a sign-in's code exchange sends the client secret to it, which
// only a local run may send in plain text, to its own machine.
func signInIssuerOK(issuer string) bool {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Host == "" {
		return false
	}
	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()
		addr, err := netip.ParseAddr(host)
		return host == "localhost" || err == nil && netaddr.Canonical(addr).IsLoopback()
	}
	return false
}

// secretValue reads NAME_FILE (trimmed contents) ahead of NAME; a set-but-unreadable or blank
// file is an error naming both, never a fallback.
func secretValue(getenv func(string) string, name string) (string, error) {
	if path := getenv(name + "_FILE"); path != "" {
		return secretFile(name+"_FILE", path)
	}
	return getenv(name), nil
}

// secretFile reads the trimmed contents of path, the file the variable setting names; an
// unreadable or blank file is an error naming setting and path, never what the file holds.
func secretFile(setting, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s names %s, which could not be read: %w", setting, path, err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("%s names %s, which is empty", setting, path)
	}
	return value, nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
