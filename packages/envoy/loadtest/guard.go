package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/loadtest/listenerclient"
	"github.com/sjawhar/envoy/redirects"
)

// stackFile is $LOADTEST_ROOT/stack.json. stack.sh setup writes it; stack.sh rewrites the pids as it
// starts and stops processes, so pids.dispatch is the running Dispatch (restart-dispatch moves it):
//
//	{
//	  "name": "loadtest-<random>",   NATS server_name (nats-server -n) and the listener's machine id
//	  "root": "<LOADTEST_ROOT>",
//	  "dispatch_token": "<random>",  Dispatch's DISPATCH_AGENT_TOKEN
//	  "listener_token": "<random>",  the listener's ENVOY_API_TOKEN
//	  "ports": {"postgres": 15432, "proxy": 15433, "proxy_stats": 15434, "nats": 14222,
//	            "nats_monitor": 18222, "listener": 19120, "dispatch": 18777, "pprof": 16061,
//	            "envoy_proxy": 19121},
//	  "pids": {"postgres": <postmaster>, "proxy": 0, "nats": 0, "listener": 0, "envoy_proxy": 0,
//	           "dispatch": 0},
//	  "pg_direct": false,            true when Dispatch reaches Postgres without the latency proxy
//	  "envoy_rtt": "",               the round trip of envoy_proxy, the proxy Dispatch's ENVOY_URL
//	                                 names; "" when Dispatch reaches the listener directly
//	  "dispatch_log": "<path>"       the log the running Dispatch writes
//	}
//
// Every process listens on 127.0.0.1. The loadgen samplers and the lock probe always reach
// Postgres directly (ports.postgres, database dispatch), never through the proxy, and loadgen
// reaches the listener directly (ports.listener), never through envoy_proxy. ports.proxy_stats
// is the proxy's wire counts (proxy.go); a stack.json from before it has none, and a step on it
// reports its round trips as not measured.
type stackFile struct {
	Name          string     `json:"name"`
	Root          string     `json:"root"`
	DispatchToken string     `json:"dispatch_token"`
	ListenerToken string     `json:"listener_token"`
	Ports         stackPorts `json:"ports"`
	PIDs          stackPIDs  `json:"pids"`
	PGDirect      bool       `json:"pg_direct"`
	EnvoyRTT      string     `json:"envoy_rtt"`
}

type stackPorts struct {
	Postgres    int `json:"postgres"`
	Proxy       int `json:"proxy"`
	ProxyStats  int `json:"proxy_stats"`
	NATS        int `json:"nats"`
	NATSMonitor int `json:"nats_monitor"`
	Listener    int `json:"listener"`
	Dispatch    int `json:"dispatch"`
	Pprof       int `json:"pprof"`
}

type stackPIDs struct {
	Postgres   int `json:"postgres"`
	Proxy      int `json:"proxy"`
	NATS       int `json:"nats"`
	Listener   int `json:"listener"`
	EnvoyProxy int `json:"envoy_proxy"`
	Dispatch   int `json:"dispatch"`
}

// byName is the pids by the sampler's process names.
func (p stackPIDs) byName() map[string]int {
	return map[string]int{
		"postgres": p.Postgres, "proxy": p.Proxy, "nats": p.NATS,
		"listener": p.Listener, "envoy_proxy": p.EnvoyProxy, "dispatch": p.Dispatch,
	}
}

func loadStack(path string) (*stackFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st stackFile
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("stack file %s: %w", path, err)
	}
	missing := []string{}
	for _, field := range []struct {
		name  string
		empty bool
	}{
		{"name", st.Name == ""}, {"dispatch_token", st.DispatchToken == ""}, {"listener_token", st.ListenerToken == ""},
		{"ports.postgres", st.Ports.Postgres == 0}, {"ports.nats", st.Ports.NATS == 0},
		{"ports.nats_monitor", st.Ports.NATSMonitor == 0}, {"ports.listener", st.Ports.Listener == 0},
		{"ports.dispatch", st.Ports.Dispatch == 0}, {"ports.pprof", st.Ports.Pprof == 0},
	} {
		if field.empty {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("stack file %s lacks %s", path, strings.Join(missing, ", "))
	}
	return &st, nil
}

// endpoints is every address and credential a seed or a step uses.
type endpoints struct {
	dispatch, listener, nats, natsMon, pprof, pg string
	token, listenerToken                         string
	// natsServerName is the /varz server_name the guard expects; empty skips that check.
	natsServerName string
	// pids are the processes the sampler reads from /proc, by name ("postgres" is the
	// postmaster); empty under -allow-foreign.
	pids     map[string]int
	stack    string
	pgDirect bool
	// envoyRTT is the round trip of the latency proxy in front of the listener that Dispatch's
	// ENVOY_URL names (stack.json envoy_rtt); "" when Dispatch reaches the listener directly.
	envoyRTT string
	// proxyStats is the latency proxy's wire-count URL (http://127.0.0.1:<proxy_stats>), from
	// stack.json only; empty when stack.json names no such port, and under -allow-foreign. It is
	// no endpoint flag: a foreign run takes nothing from stack.json, so it reads no proxy's counts
	// (a foreign stack has no latency proxy of loadgen's) and reports its round trips as not
	// measured; the rule that a foreign run gives every endpoint flag does not cover it.
	proxyStats string
}

// endpointFlags are the endpoint flags seed and run share. Each defaults empty, so the stack's
// value applies; one that differs from it is refused unless allowForeign is set, and then every
// one is required -- the two tokens by their -…-file forms, the only ones they have, and -pg by
// either of its two (readCredentialFiles).
type endpointFlags struct {
	stack                                   string
	url, listener, nats, natsMon, pprof, pg string
	// pgGiven is -pg as the command line gave it (argvFlag): pg is its last value, or the content
	// of pgFile when that gave it.
	pgGiven argvFlag
	// token and listenerToken are the contents of tokenFile and listenerTokenFile: no flag takes
	// a token's value, and tokenGiven and listenerTokenGiven stand where -token and
	// -listener-token were (removedFlag).
	token, listenerToken                 string
	tokenGiven, listenerTokenGiven       removedFlag
	tokenFile, listenerTokenFile, pgFile string
	natsServerName                       string
	allowForeign                         bool
}

func (f *endpointFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.stack, "stack", "", "stack.json path (default $LOADTEST_ROOT/stack.json)")
	fs.StringVar(&f.url, "url", "", "Dispatch base URL (default: the stack's)")
	fs.StringVar(&f.listener, "listener", "", "listener base URL (default: the stack's)")
	fs.StringVar(&f.nats, "nats", "", "NATS URL (default: the stack's)")
	fs.StringVar(&f.natsMon, "nats-mon", "", "NATS monitoring URL (default: the stack's)")
	fs.StringVar(&f.pprof, "pprof", "", "Dispatch pprof base URL (default: the stack's)")
	f.pgGiven.register(fs, "pg", &f.pg, pgValueRefusal, "direct Postgres URL of the dispatch database, with no password in it (default: the stack's)")
	fs.StringVar(&f.pgFile, "pg-file", "", "file holding the -pg value, so no password reaches the command line")
	fs.StringVar(&f.tokenFile, "token-file", "", "file holding the Dispatch agent bearer (default: the stack's)")
	fs.StringVar(&f.listenerTokenFile, "listener-token-file", "", "file holding the listener bearer (default: the stack's)")
	f.tokenGiven.register(fs, "token", "-token-file")
	f.listenerTokenGiven.register(fs, "listener-token", "-listener-token-file")
	fs.StringVar(&f.natsServerName, "nats-server-name", "", "with -allow-foreign: the NATS server_name to expect (empty skips the check)")
	fs.BoolVar(&f.allowForeign, "allow-foreign", false, "allow endpoint flags that differ from stack.json (another stack, such as a cluster copy)")
}

// removedFlag stands where a flag that took a secret's value was: -token, -listener-token and
// stub-embed's -key. Every argument of a process is readable by every user of its machine, in
// /proc/<pid>/cmdline and `ps -eo args`, for as long as the process runs, so that secret comes
// from the file the flag's -…-file form names, and the flag given at all is refused, by
// parseArgs. Set keeps nothing of the value and refuses nothing itself: the flag package's own
// refusal of a value quotes the value, so it would print the secret.
type removedFlag struct {
	flag, fileFlag string
	given          bool
}

func (r *removedFlag) String() string   { return "" }
func (r *removedFlag) Set(string) error { r.given = true; return nil }

// register registers r as flag name of fs, whose value now comes only from fileFlag.
func (r *removedFlag) register(fs *flag.FlagSet, name, fileFlag string) {
	r.flag, r.fileFlag = "-"+name, fileFlag
	fs.Var(r, name, "removed, and refused: give "+fileFlag+" instead, since a value on the command line is readable by every user of the machine")
}

func (r *removedFlag) refusal() error {
	if !r.given {
		return nil
	}
	return r.refusalOf("")
}

// refusalOf refuses r given any value where the flag package did not parse it (parseArgs).
func (r *removedFlag) refusalOf(string) error {
	return &removedFlagError{Flag: r.flag, FileFlag: r.fileFlag}
}

// argvFlag is -pg as the command line gave it. Its value is the last one given, as a StringVar's
// is, and refused is check's refusal of the first one given that sets a password (pgValueRefusal):
// every value given is in the command line, not the last alone. Set refuses nothing itself, for
// the reason removedFlag gives, and check reads no file, so the parse never waits on one;
// parseArgs refuses.
type argvFlag struct {
	value   *string
	check   func(string) error
	refused error
}

func (a *argvFlag) String() string {
	if a.value == nil {
		return ""
	}
	return *a.value
}

func (a *argvFlag) Set(v string) error {
	*a.value = v
	if a.refused == nil {
		a.refused = a.check(v)
	}
	return nil
}

// register registers a as flag name of fs, its value landing in value, whose current content is
// the default, and each value given judged by check.
func (a *argvFlag) register(fs *flag.FlagSet, name string, value *string, check func(string) error, usage string) {
	a.value, a.check = value, check
	fs.Var(a, name, usage)
}

func (a *argvFlag) refusal() error { return a.refused }

// refusalOf is check's refusal of v given where the flag package did not parse it (parseArgs).
func (a *argvFlag) refusalOf(v string) error { return a.check(v) }

// commandLineForm is a flag as the command line gave it (removedFlag, argvFlag): refusal is the
// refusal of a secret it was given there, or nil, and refusalOf the refusal of a value given to it
// in an argument the flag package did not parse.
type commandLineForm interface {
	refusal() error
	refusalOf(value string) error
}

// parseArgs parses args into fs, then refuses the first secret the command line gave any flag of
// fs that records one (commandLineForm): a removed flag given at all, or a -pg that sets a
// password. It is the one place a secret on the command line is refused. Every command whose
// flags take a credential parses with it, so the refusal comes before the command reads, opens or
// checks anything else, and it names the flag's file form.
//
// The flag package stops at the first argument that is not a flag, and at "--", so the arguments
// it left are read for those flags too, as it would have read them (flagIn): a removed flag there
// is refused, and a -pg there is judged with the value after its = or in the next argument. A
// parse error is the flag package's own, and an ExitOnError set exits there; it holds nothing of
// a value of these flags, since their Set never fails.
func parseArgs(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	var refused error
	fs.VisitAll(func(f *flag.Flag) {
		if form, ok := f.Value.(commandLineForm); ok && refused == nil {
			refused = form.refusal()
		}
	})
	if refused != nil {
		return refused
	}
	rest := fs.Args()
	for i, arg := range rest {
		name, value, hasValue, ok := flagIn(arg)
		if !ok {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		form, ok := f.Value.(commandLineForm)
		if !ok {
			continue
		}
		if !hasValue && i+1 < len(rest) {
			value = rest[i+1]
		}
		if err := form.refusalOf(value); err != nil {
			return err
		}
	}
	return nil
}

// flagIn reads arg as the flag package reads a flag (flag.FlagSet.parseOne): one or two dashes, a
// name that starts with neither - nor =, and the value after the first = when there is one. ok is
// false for an argument that is no flag: "-", "--", "---x", "-=x", "x".
func flagIn(arg string) (name, value string, hasValue, ok bool) {
	s, found := strings.CutPrefix(arg, "-")
	if !found {
		return "", "", false, false
	}
	s = strings.TrimPrefix(s, "-")
	if s == "" || s[0] == '-' || s[0] == '=' {
		return "", "", false, false
	}
	name, value, hasValue = strings.Cut(s, "=")
	return name, value, hasValue, true
}

// credentialFile is one credential a command takes from the file path names, under fileFlag;
// value is where the credential lands, and flag is the flag that takes it on the command line
// (a removed one for -token, -listener-token and -key; -pg, whose value form stays for a
// connection string that sets no password).
type credentialFile struct {
	flag, fileFlag, path string
	value                *string
}

// readCredentials replaces each credential's value with its file's content, trailing white space
// trimmed, so that a file written with a final newline holds what the credential is. A credential
// is a value that carries a secret: a Dispatch agent token, the listener token, a Postgres URL
// with its password, the stub embedder's api-key. Every argument of every process is readable by
// every user of the machine (/proc/<pid>/cmdline, `ps -eo args`), here and on any machine an ssh
// carries the command to, so a credential comes only from a file: vm/vm.sh writes one of mode 600
// on the machine the command runs on, as it writes every container's settings, and
// cluster/cluster.sh writes them in the runner pod for the one job that reads them. A secret on
// the command line was refused before this, by parseArgs.
//
// A credential given in both forms is refused: which was meant cannot be known, and the two may
// name different things. A file that cannot be read, or that holds nothing, is refused by its flag
// and its path. Each refusal comes before the first connection, as every other input of this
// package is refused, and none holds anything of the value.
//
// Every command of this package that takes a credential reads it here, and no second reader of a
// credential file exists: the refusals are one behaviour, and a command that spelled them again
// would drift from it (stubembed.go, loopseed_check.go, loopseed_export.go, pgstats.go,
// seedfinish.go).
func readCredentials(creds ...credentialFile) error {
	for _, c := range creds {
		if c.path == "" {
			continue
		}
		if *c.value != "" {
			return &bothCredentialFormsError{Flag: c.flag, FileFlag: c.fileFlag}
		}
		b, err := os.ReadFile(c.path)
		if err != nil {
			// The error names the path alone; nothing of a file that was read is in it.
			return fmt.Errorf("%s: %w", c.fileFlag, err)
		}
		v := strings.TrimRightFunc(string(b), unicode.IsSpace)
		if v == "" {
			return fmt.Errorf("%s %s holds no credential: the file is empty", c.fileFlag, c.path)
		}
		*c.value = v
	}
	return nil
}

// readCredentialFiles reads the three endpoint credentials: the Dispatch agent token, the
// listener token and the Postgres URL with its password.
func (f *endpointFlags) readCredentialFiles() error {
	return readCredentials(
		credentialFile{"-token", "-token-file", f.tokenFile, &f.token},
		credentialFile{"-listener-token", "-listener-token-file", f.listenerTokenFile, &f.listenerToken},
		credentialFile{"-pg", "-pg-file", f.pgFile, &f.pg},
	)
}

// removedFlagError refuses a flag whose value form is removed (removedFlag).
type removedFlagError struct {
	Flag, FileFlag string
}

func (e *removedFlagError) Error() string {
	return fmt.Sprintf("%s is removed: a value on the command line is readable by every user of the machine (/proc/<pid>/cmdline), so give the credential in a file of mode 600 with %s <path>", e.Flag, e.FileFlag)
}

// pgPasswordError refuses a -pg value that sets a password (pgValueRefusal).
type pgPasswordError struct {
	Flag, FileFlag string
}

func (e *pgPasswordError) Error() string {
	return fmt.Sprintf("%s sets a password, and a value on the command line is readable by every user of the machine (/proc/<pid>/cmdline): give the connection string in a file of mode 600 with %s <path>, or give %s one with no password in it", e.Flag, e.FileFlag, e.Flag)
}

// pgPasswordKey matches, in a -pg value percent-decoded, a password or an sslpassword key
// wherever one could start: pgx reads a keyword=value string's keys as they are written, and a
// URL's query keys percent-decoded, each with any of pgx's white space around its = (a vertical
// tab among it, which Go's \s leaves out). It is read without regard to case and at every place
// such a key could start, so a value that only looks like one is refused too, and a key pgx would
// read is never missed.
var pgPasswordKey = regexp.MustCompile(`(?i)(^|[^a-z0-9_])(ssl)?password[\t\n\v\f\r ]*=`)

// pgValueRefusal is the refusal of raw, a -pg value given on the command line, or nil when it sets
// no password. It reads raw as text alone, never as pgx does: pgx's parser reads the files a value
// names (sslrootcert, sslcert, sslkey, passfile, a service file) before it says anything of the
// value's keys, and a file that blocks its reader would keep the command, and the secret in its
// argv, waiting at the parse. So it finds a password where pgx would read one (pgx/v5 pgconn's
// parse_url.go and config.go): the userinfo of a postgres:// or postgresql:// URL, which pgx ends
// at the first @ before any / (so a password that starts with # is one pgx sends and one net/url
// reads as a fragment), and a password or sslpassword key of a keyword=value string or of a URL's
// query (pgPasswordKey).
func pgValueRefusal(raw string) error {
	if pgUserinfoPassword(raw) || pgPasswordKey.MatchString(percentDecoded(raw)) {
		return &pgPasswordError{Flag: "-pg", FileFlag: "-pg-file"}
	}
	return nil
}

// pgUserinfoPassword reports whether raw is a URL as pgx tells one (its postgres:// or
// postgresql:// prefix, case-sensitive) whose userinfo, which pgx ends at the first @ before any
// /, holds a password after its first :.
func pgUserinfoPassword(raw string) bool {
	p, ok := strings.CutPrefix(raw, "postgresql://")
	if !ok {
		if p, ok = strings.CutPrefix(raw, "postgres://"); !ok {
			return false
		}
	}
	i := strings.IndexAny(p, "@/")
	if i < 0 || p[i] != '@' {
		return false
	}
	_, password, _ := strings.Cut(p[:i], ":")
	return password != ""
}

// percentDecoded is s with each % and two hex digits replaced by the byte they name and every
// other byte as it is: what pgx's decode of a URL's query key gives (uriDecode), refusing nothing.
func percentDecoded(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// bothCredentialFormsError refuses a credential given as a value and as a file at once.
type bothCredentialFormsError struct {
	Flag, FileFlag string
}

func (e *bothCredentialFormsError) Error() string {
	return fmt.Sprintf("%s and %s both give the same credential; pass one of them", e.Flag, e.FileFlag)
}

// urlFlags are the endpoint flags whose values are URLs. meta.json records the run's flags with
// each URL without its credentials (recordedArgs, redactEndpoint); no flag holds a token.
var urlFlags = []string{"-url", "-listener", "-nats", "-nats-mon", "-pprof", "-pg"}

// redactURL is raw as the URL rule names a URL a request asked (redact.AskedURL): its scheme, host
// and path, with its user information, its query and its fragment removed, for every place that
// records or prints an HTTP endpoint (redactEndpoint does each -nats server the same way). The
// whole userinfo goes, the user name too: nats.go takes a user name with no password as the auth
// token; and the whole query goes, since a value under any name can be a credential (an
// access_token, a signature). A ws or wss URL, a socket the load test dials, is read as the http
// or https URL it upgrades from and keeps its own scheme. raw is read whole by url.Parse, as
// net/http reads it. A value that does not parse, or names no scheme, is redacted whole, since no
// credential in it can be found for sure, and one the rule cannot read as clean is
// redact.NotPrinted. A -pg value is never read here (redactPG).
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return redacted
	}
	socket := strings.ToLower(u.Scheme)
	switch socket {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return redact.AskedURL(u)
	}
	shown := redact.AskedURL(u)
	if shown == redact.NotPrinted {
		return shown
	}
	return socket + strings.TrimPrefix(shown, u.Scheme)
}

// redactEndpoint is the value of endpoint flag flag as it is recorded or printed: a -nats value is
// a server list that nats.go splits at commas (processUrlString), each server with userinfo of its
// own, so each loses its own; a -pg value is redactPG's; any other flag's value is one URL
// (redactURL).
func redactEndpoint(flag, raw string) string {
	if flag == "-pg" {
		return redactPG(raw)
	}
	if flag != "-nats" {
		return redactURL(raw)
	}
	servers := strings.Split(raw, ",")
	for i, server := range servers {
		u, scheme, err := parseNATSServer(server)
		if err != nil {
			servers[i] = redacted
			continue
		}
		servers[i] = withoutCredentials(u).String()
		if !scheme {
			servers[i] = strings.TrimPrefix(servers[i], natsScheme)
		}
	}
	return strings.Join(servers, ",")
}

// withoutCredentials is u, a NATS server's URL, with no userinfo, query or fragment: nats.go reads
// a user, a password or a token from the userinfo, and a query or a fragment can carry a
// credential under any name.
func withoutCredentials(u *url.URL) *url.URL {
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u
}

// parsePG is pgx's own parse of a -pg value, and ok is false when pgx would not read raw as a
// URL: raw must start with postgres:// or postgresql:// (the prefixes pgconn.ParseConfig checks,
// case-sensitive; any other value goes to its keyword/value parser, whose error can quote the
// value) and pgconn.ParseConfig, the parser pgx.Connect uses, must accept it. ParseConfig also
// reads the PG* environment variables and may read the passfile, as pgx.Connect does.
func parsePG(raw string) (config *pgconn.Config, ok bool) {
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return nil, false
	}
	config, err := pgconn.ParseConfig(raw)
	return config, err == nil
}

// redactPG is a -pg value as it is recorded or printed, built from parsePG's result alone:
// postgres://, each host:port pgx would try (Host and Port, then each of Fallbacks, a repeat of
// the one before it dropped: sslmode prefer and allow give each host twice), comma-separated,
// then / and the database. It holds no user, password, sslpassword or other parameter. A value
// parsePG refuses is redacted whole.
func redactPG(raw string) string {
	config, ok := parsePG(raw)
	if !ok {
		return redacted
	}
	addrs := []string{net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))}
	for _, fb := range config.Fallbacks {
		addrs = append(addrs, net.JoinHostPort(fb.Host, strconv.Itoa(int(fb.Port))))
	}
	return "postgres://" + strings.Join(slices.Compact(addrs), ",") + "/" + url.PathEscape(config.Database)
}

// natsScheme stands in for the scheme of a NATS server that names none ("host:4222", which
// nats.go reads as nats://host:4222), so that url.Parse reads its host.
const natsScheme = "nats://"

// parseNATSServer parses one server of a -nats list; scheme reports whether it named its own.
func parseNATSServer(server string) (u *url.URL, scheme bool, err error) {
	server = strings.TrimSpace(server)
	scheme = strings.Contains(server, "://")
	if !scheme {
		server = natsScheme + server
	}
	u, err = url.Parse(server)
	return u, scheme, err
}

// isEndpointURL reports whether v is a valid value of endpoint flag flag: for -pg, a value
// parsePG accepts; for -nats, a comma-separated server list whose every server names a host; for
// any other flag, one URL with a scheme and a host, as url.Parse reads it.
func isEndpointURL(flag, v string) bool {
	switch flag {
	case "-pg":
		_, ok := parsePG(v)
		return ok
	case "-nats":
		for _, server := range strings.Split(v, ",") {
			if u, _, err := parseNATSServer(server); err != nil || u.Host == "" {
				return false
			}
		}
		return true
	default:
		u, err := url.Parse(strings.TrimSpace(v))
		return err == nil && u.Scheme != "" && u.Host != ""
	}
}

// joinTrimPath is raw, an endpoint flag's URL, without the slashes that end it when it has no
// query and no fragment, so a value that differs from the stack's by a trailing slash alone is the
// stack's own. A value with a query or a fragment is as it came: the slashes that end its text are
// the query's or the fragment's, which a trim would change (http://host/p?next=/ would ask with
// next empty), and urljoin.Join, which joins a route onto an endpoint's URL, reads a path that ends
// in a slash as one that does not.
func joinTrimPath(raw string) string {
	if strings.ContainsAny(raw, "?#") {
		return raw
	}
	return strings.TrimRight(raw, "/")
}

// foreignFlagError refuses an endpoint flag that names something other than the stack's own.
type foreignFlagError struct {
	Flag string
}

func (e *foreignFlagError) Error() string {
	return fmt.Sprintf("%s differs from stack.json; it would target something other than the local stack, so it is refused (pass -allow-foreign to target another stack on purpose)", e.Flag)
}

// missingForeignFlagError refuses a run under -allow-foreign that leaves an endpoint flag empty:
// it would fall back to the local stack's value, so the run would write to one stack and count,
// sample or profile another. All lists every endpoint and token flag.
type missingForeignFlagError struct {
	Flag string
	All  []string
}

func (e *missingForeignFlagError) Error() string {
	return fmt.Sprintf("%s is required with -allow-foreign: a foreign run takes no endpoint or token from stack.json, so give every one of %s", e.Flag, strings.Join(e.All, ", "))
}

// resolve reads stack.json and resolves the endpoints. It opens no connection. A secret on the
// command line was refused before this, by parseArgs, so stack.json is never read for a command
// line that gave one.
func (f endpointFlags) resolve() (endpoints, error) {
	path := f.stack
	if path == "" {
		path = filepath.Join(loadtestRoot(), "stack.json")
	}
	st, err := loadStack(path)
	if err != nil {
		if !f.allowForeign || f.stack != "" || !errors.Is(err, fs.ErrNotExist) {
			return endpoints{}, fmt.Errorf("read stack: %w", err)
		}
	}
	e, err := resolveEndpoints(f, st)
	e.stack = path
	return e, err
}

// resolveEndpoints takes every endpoint from st, refusing a flag that differs from it, unless
// f.allowForeign: then every endpoint flag is required, none falls back to st, and pids stay
// empty. The credentials are read first (readCredentialFiles): each token from its file, and the
// Postgres URL from its file or from -pg.
func resolveEndpoints(f endpointFlags, st *stackFile) (endpoints, error) {
	if st == nil && !f.allowForeign {
		return endpoints{}, fmt.Errorf("no stack.json, and -allow-foreign is not set")
	}
	if err := f.readCredentialFiles(); err != nil {
		return endpoints{}, err
	}
	var e endpoints
	// Under -allow-foreign without stack.json, the stack values below are never used.
	var sv stackFile
	if st != nil {
		sv = *st
		e.pgDirect = st.PGDirect && !f.allowForeign
		if !f.allowForeign {
			e.envoyRTT = st.EnvoyRTT
		}
	}
	local := func(scheme string, port int) string { return fmt.Sprintf("%s://127.0.0.1:%d", scheme, port) }
	// The endpoint and token flags, in the order the guard checks them: each one's name, the value
	// given (a credential's, from its file when it was given as one), the stack's value and the
	// endpoint it sets.
	fields := []struct {
		flag, given, stack string
		dst                *string
	}{
		{"-url", joinTrimPath(f.url), local("http", sv.Ports.Dispatch), &e.dispatch},
		{"-listener", joinTrimPath(f.listener), local("http", sv.Ports.Listener), &e.listener},
		{"-nats", f.nats, local("nats", sv.Ports.NATS), &e.nats},
		{"-nats-mon", joinTrimPath(f.natsMon), local("http", sv.Ports.NATSMonitor), &e.natsMon},
		{"-pprof", joinTrimPath(f.pprof), local("http", sv.Ports.Pprof), &e.pprof},
		{"-pg", f.pg, fmt.Sprintf("postgres://postgres@127.0.0.1:%d/dispatch?sslmode=disable", sv.Ports.Postgres), &e.pg},
		{"-token-file", f.token, sv.DispatchToken, &e.token},
		{"-listener-token-file", f.listenerToken, sv.ListenerToken, &e.listenerToken},
	}
	all := make([]string, len(fields))
	for i, field := range fields {
		all[i] = field.flag
	}
	for _, field := range fields {
		switch {
		case f.allowForeign && field.given == "":
			return endpoints{}, &missingForeignFlagError{Flag: field.flag, All: all}
		case !f.allowForeign && field.given != "" && field.given != field.stack:
			return endpoints{}, &foreignFlagError{Flag: field.flag}
		case field.given != "":
			// A foreign URL that does not parse would reach an error that prints it whole,
			// credentials included; the refusal names the flag and the reason alone.
			if slices.Contains(urlFlags, field.flag) && !isEndpointURL(field.flag, field.given) {
				if field.flag == "-pg" {
					return endpoints{}, errors.New("-pg is not a postgres:// or postgresql:// URL that pgx accepts")
				}
				return endpoints{}, fmt.Errorf("%s is not a URL with a scheme and a host (for -nats, a comma-separated list of servers)", field.flag)
			}
			*field.dst = field.given
		default:
			*field.dst = field.stack
		}
	}
	if f.natsServerName != "" && !f.allowForeign {
		return endpoints{}, fmt.Errorf("-nats-server-name applies only with -allow-foreign; the stack's name is expected otherwise")
	}
	e.pids = map[string]int{}
	if f.allowForeign {
		e.natsServerName = f.natsServerName
		return e, nil
	}
	e.natsServerName = st.Name
	if st.Ports.ProxyStats > 0 {
		e.proxyStats = fmt.Sprintf("http://127.0.0.1:%d", st.Ports.ProxyStats)
	}
	for name, pid := range st.PIDs.byName() {
		if pid > 0 {
			e.pids[name] = pid
		}
	}
	return e, nil
}

// checkStack confirms, before the first write, that each endpoint is the stack's own: NATS
// /varz names the stack, Dispatch answers an authenticated read with the stack's agent token, and
// the listener answers /v1/sessions with the stack's API token and refuses any other token (a
// listener with no token configured would answer every caller). The pod's live listener and
// production Dispatch hold other tokens, so a mistaken target fails here.
func checkStack(ctx context.Context, e endpoints) error {
	client := redirects.Refused(shortClient)
	if err := checkNATSServerName(ctx, client, e.natsMon, e.natsServerName); err != nil {
		return err
	}
	joined, err := urljoin.Join(e.dispatch, "/api/v1/projects")
	if err != nil {
		return errors.New("guard: dispatch check failed: the Dispatch URL is not an absolute URL with a host (it is not printed)")
	}
	projects := joined.String()
	status, body, err := guardGet(ctx, client, projects, e.token)
	if err != nil {
		return fmt.Errorf("guard: dispatch check failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("guard: dispatch check failed: GET %s with the stack's agent token answered %d %s", redactURL(projects), status, answerWords(body))
	}
	status, body, err = (&listenerclient.Client{URL: e.listener, Token: e.listenerToken, HTTP: client}).Sessions(ctx)
	if err != nil {
		return fmt.Errorf("guard: listener check failed: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("guard: listener check failed: GET %s with the stack's listener token answered %d %s", joinRedacted(e.listener, listenerclient.SessionsRoute), status, answerWords(body))
	}
	other := make([]byte, 16)
	_, _ = rand.Read(other)
	status, _, err = (&listenerclient.Client{URL: e.listener, Token: hex.EncodeToString(other), HTTP: client}).Sessions(ctx)
	if err != nil {
		return fmt.Errorf("guard: listener check failed: %w", err)
	}
	if status/100 == 2 {
		return fmt.Errorf("guard: listener check failed: %s answers %d to a token that is not the stack's, so it does not hold the stack's token", joinRedacted(e.listener, listenerclient.SessionsRoute), status)
	}
	return nil
}

// joinRedacted is route joined onto base, an endpoint's URL (urljoin.Join), as a message names it
// (redactURL), and redacted for a base that does not join.
func joinRedacted(base, route string) string {
	joined, err := urljoin.Join(base, route)
	if err != nil {
		return redacted
	}
	return redactURL(joined.String())
}

// checkNATSServerName confirms that the NATS server monitored at monURL names itself name, so a
// command that publishes (a step, a backlog run) reaches the stack's own NATS and no other. An
// empty name skips the check.
func checkNATSServerName(ctx context.Context, client *http.Client, monURL, name string) error {
	if name == "" {
		return nil
	}
	joined, err := urljoin.Join(monURL, "/varz")
	if err != nil {
		return errors.New("guard: nats check failed: the NATS monitor URL is not an absolute URL with a host (it is not printed)")
	}
	varzURL := joined.String()
	var varz struct {
		ServerName redact.RemoteText `json:"server_name"`
	}
	status, body, err := guardGet(ctx, client, varzURL, "")
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("GET %s answered %d", redactURL(varzURL), status)
	}
	if err == nil {
		err = body.Decode(&varz)
	}
	if err != nil {
		return fmt.Errorf("guard: nats check failed: %w", err)
	}
	if !varz.ServerName.Equal(name) {
		return fmt.Errorf("guard: nats check failed: %s server_name is %q, want %q", redactURL(varzURL), varz.ServerName, name)
	}
	return nil
}

// guardGet is one GET of url, a route joined onto an endpoint's URL, with bearer when it is not
// empty. A url that does not parse is refused (errJoinedURLUnparsed).
func guardGet(ctx context.Context, client *http.Client, url, bearer string) (int, redact.Answer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, redact.Answer{}, errJoinedURLUnparsed
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, redact.Answer{}, redact.Cause(err, url)
	}
	defer resp.Body.Close()
	body, err := redact.ReadAnswer(resp, 1<<20)
	if errors.Is(err, redact.ErrAnswerTooLarge) {
		err = nil
	}
	return resp.StatusCode, body, err
}
