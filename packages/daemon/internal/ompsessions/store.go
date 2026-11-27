package ompsessions

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"cloud.google.com/go/cloudsqlconn/instance"
)

// StoreKind is which session store a deployment runs (runtime.kubernetes.session_store).
type StoreKind string

const (
	// StoreVolume keeps every session as a file on the issue's volume.
	StoreVolume StoreKind = "pvc"
	// StorePostgres keeps every session in the database whose whole postgres:// URL, password and
	// all, one providers Secret key holds.
	StorePostgres StoreKind = "postgres"
	// StoreCloudSQLIAM keeps every session in a Cloud SQL database each pod signs in to over IAM,
	// with no password written anywhere: the pod's launcher binds a unix socket of its own for each
	// generation and proxies it to the instance (internal/sessionproxy), signing in over the pod's
	// own cloud identity, or as the Google service account the deployment names, which that
	// identity impersonates (Database.ImpersonateServiceAccount), or which a token only the
	// launcher reads impersonates (Database.WriterAudience).
	StoreCloudSQLIAM StoreKind = "cloudsql-iam"
)

// Store is the settled session store: the kind, and the one field that kind alone carries, so no
// reader can reach a database without what opens it. The zero value is StoreVolume's: sessions are
// files.
type Store struct {
	Kind StoreKind
	// DSNSecret is the providers Secret's key that holds the session database's whole postgres://
	// URL, under StorePostgres alone.
	DSNSecret string
	// Database is the Cloud SQL database every pod signs in to, as itself or as the service account
	// it impersonates, under StoreCloudSQLIAM alone.
	Database Database
}

// Database is a Cloud SQL session database: the instance's connection name (ValidInstance), the
// database in it, the IAM login a pod signs in as, the service account, if any, whose login that
// is and which the pod impersonates to sign in as it, and the audience, if any, of the token that
// the pod impersonates that account with in place of its own credentials. It is a struct of
// strings, so a reader compares two databases, or one with the zero value, with ==.
type Database struct {
	Instance, Name, User string
	// ImpersonateServiceAccount is the email of the Google service account the pod's session proxy
	// impersonates and signs in as, so User is that account's IAM login (ServiceAccountLogin). The
	// pod's own credentials impersonate it, or the launcher's writer token does where
	// WriterAudience is set. Empty signs in as the pod's own identity.
	ImpersonateServiceAccount string
	// WriterAudience is the audience of the projected ServiceAccount token that only the pod's
	// launcher reads. It is also the audience the session proxy names to Google's security token
	// service when it exchanges that token: a workload identity provider's full resource name
	// (ValidWriterAudience). The proxy then impersonates ImpersonateServiceAccount with the
	// federated token, never with the pod's own credentials, so the audience is set only with an
	// account. Empty leaves the proxy to sign in with the pod's own credentials.
	WriterAudience string
}

// ValidInstance reports whether name is a Cloud SQL instance's connection name the connector
// uses whole: it holds no Unicode whitespace, the Cloud SQL connector's own parser
// (cloudsqlconn's instance.ParseConnName) accepts it, and the project, region and name it parses
// out, joined again as project:region:name, are name itself. The connector takes
// project:region:instance, or domain:project:region:instance for a legacy domain-scoped project,
// but its pattern is unanchored and would quietly take a part of a longer string (a:b:c:d:e parses
// as project a:b, region c, name d), so the round trip refuses whatever it would not use whole.
// It is the one rule, which both the loader that reads the instance from the file and the proxy
// that dials it hold to, so `legion config check` accepts nothing the connector would refuse or
// read otherwise.
func ValidInstance(name string) bool {
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return false
	}
	c, err := instance.ParseConnName(name)
	return err == nil && c.Project()+":"+c.Region()+":"+c.Name() == name
}

// serviceAccountEmail is a Google service account's email: a user-managed account's
// (<name>@<project>.iam.gserviceaccount.com) and the forms Google makes itself, such as
// <number>-compute@developer.gserviceaccount.com, <project>@appspot.gserviceaccount.com and
// service-<number>@gcp-sa-<service>.iam.gserviceaccount.com, all in lower case.
var serviceAccountEmail = regexp.MustCompile(`^[a-z0-9-]+@[a-z0-9-]+(\.[a-z0-9-]+)*\.gserviceaccount\.com$`)

// ServiceAccountLogin answers whether email is a Google service account's email and, when it is,
// the IAM database login Cloud SQL gives that account: the email without its .gserviceaccount.com
// suffix ("For a service account, this is the service account's email without the
// .gserviceaccount.com domain suffix", https://cloud.google.com/sql/docs/postgres/iam-logins). It
// refuses the login form itself (<name>@<project>.iam), a person's email, a bare name, upper case,
// white space and a blank value. It is the one rule, which both the loader that reads the account
// from the file and the proxy that impersonates it hold to, as both hold to ValidInstance for the
// instance, so `legion config check` accepts no account the proxy would refuse.
func ServiceAccountLogin(email string) (login string, ok bool) {
	if !serviceAccountEmail.MatchString(email) {
		return "", false
	}
	return strings.TrimSuffix(email, ".gserviceaccount.com"), true
}

// writerAudience is a workload identity provider's full resource name, with its pool id and its
// provider id captured.
var writerAudience = regexp.MustCompile(`^//iam\.googleapis\.com/projects/[0-9]+/locations/global/workloadIdentityPools/([a-z0-9-]{4,32})/providers/([a-z0-9-]{4,32})$`)

// WriterAudienceForm is ValidWriterAudience's rule in words: the form of a workload identity
// provider's full resource name and the rule each of its two ids holds to, Google's reserved gcp-
// prefix included. The loader's, the launcher's and the proxy's refusals of an audience and the
// key table's description all name it, so each says the whole rule, and in the same words.
const WriterAudienceForm = "//iam.googleapis.com/projects/<project number>/locations/global/workloadIdentityPools/<pool id>/providers/<provider id>," +
	" each id 4 to 32 characters of a-z, 0-9 and - that does not start with gcp-"

// ValidWriterAudience reports whether audience is the full resource name of a workload identity
// provider:
//
//	//iam.googleapis.com/projects/<project number>/locations/global/workloadIdentityPools/<pool id>/providers/<provider id>
//
// which is the audience of the projected ServiceAccount token only the launcher reads, and the
// audience the session proxy names to Google's security token service when it exchanges that
// token. The project is its number in decimal digits, never its id. Google holds a pool id and a
// provider id each to 4 to 32 characters of [a-z0-9-], and reserves the prefix gcp- for itself, so
// no pool or provider a deployment makes starts with it
// (https://cloud.google.com/iam/docs/reference/rest/v1/projects.locations.workloadIdentityPools/create
// and https://cloud.google.com/iam/docs/reference/rest/v1/projects.locations.workloadIdentityPools.providers/create).
// The rule is anchored, so it refuses white space, upper case, a trailing slash, another host or
// prefix, an extra path segment and a blank value. It is the one rule, which the loader that reads
// the audience from the file, the launcher that takes it as a flag and the proxy that exchanges
// the token all hold to, as the loader and the proxy hold to ValidInstance for the instance, so
// `legion config check` accepts no audience the launcher or the proxy would refuse.
func ValidWriterAudience(audience string) bool {
	ids := writerAudience.FindStringSubmatch(audience)
	return ids != nil && !strings.HasPrefix(ids[1], "gcp-") && !strings.HasPrefix(ids[2], "gcp-")
}

// InDatabase is whether a session is a row rather than a file, which both database kinds are: then
// nothing a volume holds says anything about a session. It is the kind's own answer, so a reader
// that holds the kind alone — the store a kept pod was built under, which is all of a store its
// start command names (sandbox.keptPod) — asks the question the settled store asks.
func (k StoreKind) InDatabase() bool { return k == StorePostgres || k == StoreCloudSQLIAM }

func (s Store) InDatabase() bool { return s.Kind.InDatabase() }

// DSNSecretKey is the providers Secret's key that holds the session database's URL, "" unless the
// store is the postgres one: under cloudsql-iam no pod mounts a session key, since each pod signs
// in to the instance over IAM, as itself or as the service account it impersonates, and there is no
// URL to keep anywhere.
func (s Store) DSNSecretKey() string {
	if s.Kind != StorePostgres {
		return ""
	}
	return s.DSNSecret
}

// SocketName is libpq's own name for a unix socket of a Postgres server on port 5432. pgx and
// libpq address such a socket by its directory and compose this name themselves, where Bun's
// client is handed the socket whole, so the name must be stated here for the agent's spelling
// (AgentURL) and for the launcher that binds it (Socket).
const SocketName = ".s.PGSQL.5432"

// DSNFileName is the file DSNFileVariable points at: the providers mount's projection of the
// session database's URL under StorePostgres, and the file a generation's own launcher writes in
// its private directory under StoreCloudSQLIAM.
const DSNFileName = "OMP_SESSION_SQL_DSN"

// Endpoint is one unix-socket session database as its two clients address it: the directory the
// socket is bound in, the database, and the login. A login is an IAM login and holds an `@`, so
// both spellings are built with net/url, which percent-encodes it wherever it lands.
type Endpoint struct{ SocketDir, Database, User string }

// Socket is the socket's path: what the launcher binds, and what AgentURL names.
func (e Endpoint) Socket() string { return e.SocketDir + "/" + SocketName }

// AgentURL is the connection URL Oh My Pi's client reads from the DSN file. Oh My Pi hands that
// text to Bun's SQL client, which takes a unix socket as the URL's `path` parameter and ignores the
// host, then sends a plain Postgres startup packet carrying the user and the database and no
// password — which is the whole packet the proxy's upstream expects, the login travelling in the
// connector's certificate exchange instead.
func (e Endpoint) AgentURL() string {
	u := url.URL{Scheme: "postgres", User: url.User(e.User), Host: "localhost", Path: "/" + e.Database}
	u.RawQuery = url.Values{"path": {e.Socket()}, "sslmode": {"disable"}}.Encode()
	return u.String()
}

// ClientURL is the same endpoint for the launcher's own pgx client. The two spellings are not
// interchangeable: pgx takes libpq's `host` as the socket's directory and appends SocketName
// itself, where AgentURL names the socket whole. url.Values writes a space as `+`, which pgx keeps
// as a `+` rather than reading it as a space, so each `+` the encoding leaves is spelt %20: a
// literal `+` is %2B by then. A SocketDir holding a comma cannot be named at all, since pgx splits
// `host` at each comma, however it is encoded; a pod's socket directories hold none.
func (e Endpoint) ClientURL() string {
	u := url.URL{Scheme: "postgres", Path: "/" + e.Database}
	u.RawQuery = strings.ReplaceAll(url.Values{"host": {e.SocketDir}, "sslmode": {"disable"}, "user": {e.User}}.Encode(), "+", "%20")
	return u.String()
}
