// Package routes is `envoy-infra routes`, the route check of a clustered hub's
// routeAuthorization (docs/nats-hub.md, Route authorization). Take, which `routes record` runs
// once the env stage's roll has finished on every pod, saves each pod's identity and its nats
// container's start. Check, which `routes check` runs once authorization is on every pod, then
// asks whether authorization is in force, and whether any process other than the hub's own
// servers could have read the route password since it was.
//
// The password reaches a peer the configuration does not name only once authorization applies:
// an implicit route, which a server dials at an address another route's INFO names, carries the
// cluster authorization's user and password (nats-server 2.15.1 server/route.go:1092-1094), which
// are empty before. A reload closes only the routes a server accepted that fail the credential
// check, never one it opened itself, so a process that joined as a route during env or routes
// keeps the routes the hub's servers opened to it, and once authorization applies it can name an
// address, which they then dial with the password (route.go:702-713). Check reads two records of
// that: each server's routes now, and each server's log since the reload that loaded
// authorization, which names the peer of every route connection the server made; a process that
// read the password and left is in the second alone.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Verdict is what Check concludes, and the exit code `routes check` returns for it.
type Verdict int

const (
	// Clean: every rule Check applies holds. Check's doc comment gives the rules, and usageText, the
	// usage `routes check` prints, what each verdict means to the operator.
	Clean Verdict = 0
	// PasswordRead: a rule shows the route password may have been read, by a process the findings
	// name or from a log line. Treat the password as read: cut that process's reach and rotate the
	// password (docs/nats-hub.md, Route authorization).
	PasswordRead Verdict = 1
	// Unknown: the check could not read all it needs, or the hub is not yet fit to judge, or a
	// finding neither condemns nor clears a process (usageText lists each cause); it concludes
	// nothing, and in particular not Clean.
	Unknown Verdict = 2
)

func (v Verdict) String() string {
	switch v {
	case Clean:
		return "clean"
	case PasswordRead:
		return "password read"
	default:
		return "unknown"
	}
}

// Pod is one pod of the hub's StatefulSet, as the API server reports it: its name, its uid, its IP
// and its Ready condition, and its nats container's restart count and the time the running
// container started.
type Pod struct {
	Name         string
	UID          string
	IP           string
	Ready        bool
	RestartCount int32
	StartedAt    time.Time
}

// Source reads the hub's pods. Pods lists every pod the StatefulSet has, by its ordinal names;
// Monitor answers a GET of path on a pod's monitoring port; Log is the whole log of a pod's nats
// container as the API server returns it with the container runtime's times (`kubectl logs
// --timestamps`), the current container's newest file, each line after the RFC 3339 time the
// runtime read it at and a space; RouteExchange opens a connection to a pod's route port and
// returns the first line it sends, its INFO, and, when send is not empty, writes send and returns
// the first line after the INFO that answers it (Exchange). Close ends whatever the source holds
// open.
type Source interface {
	Pods(ctx context.Context) ([]Pod, error)
	Monitor(ctx context.Context, pod, path string) ([]byte, error)
	Log(ctx context.Context, pod string) ([]byte, error)
	RouteExchange(ctx context.Context, pod string, send []byte) (info, answer []byte, err error)
	Close() error
}

// Record is what `routes record` saves: the hub it read, when, and each pod's identity and its nats
// container's start at that time.
type Record struct {
	Namespace   string      `json:"namespace"`
	StatefulSet string      `json:"statefulSet"`
	RecordedAt  time.Time   `json:"recordedAt"`
	Pods        []PodRecord `json:"pods"`
}

// PodRecord is one pod as Record saves it. A pod whose uid, restart count or start time differs
// at the check has run a container since the record, whose log no longer reaches back to it.
type PodRecord struct {
	Name         string    `json:"name"`
	UID          string    `json:"uid"`
	RestartCount int32     `json:"restartCount"`
	StartedAt    time.Time `json:"startedAt"`
}

// Take reads src's pods and returns their record, timed now. It refuses while any pod is not
// Ready or its nats container is not running, since a record must be taken when the env stage's
// roll has finished: a pod that starts after it would have a log the record does not cover. It
// refuses as well while any pod's route port requires authorization, or its INFO cannot be read:
// the record vouches that each log reaches back to env, and one taken once authorization is in
// force can follow a restart whose new log holds none of the routes a process joined by at env or
// routes. Every rollout and every rotation records at env, which drops authorization.
func Take(ctx context.Context, src Source, namespace, statefulSet string, now time.Time) (Record, error) {
	pods, err := src.Pods(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("read the pods: %w", err)
	}
	if len(pods) == 0 {
		return Record{}, fmt.Errorf("the StatefulSet %s/%s has no pods", namespace, statefulSet)
	}
	var notReady []string
	rec := Record{Namespace: namespace, StatefulSet: statefulSet, RecordedAt: now.UTC()}
	for _, p := range pods {
		if !p.Ready || p.UID == "" || p.StartedAt.IsZero() {
			notReady = append(notReady, p.Name)
			continue
		}
		rec.Pods = append(rec.Pods, PodRecord{Name: p.Name, UID: p.UID, RestartCount: p.RestartCount, StartedAt: p.StartedAt.UTC()})
	}
	if len(notReady) > 0 {
		return Record{}, fmt.Errorf("%s not Ready with a running nats container; record once the env stage's roll has finished on every pod",
			strings.Join(notReady, ", "))
	}
	var authorized, unread []string
	for _, p := range pods {
		required, err := routePortAuthRequired(ctx, src, p.Name)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s (%v)", p.Name, err))
			continue
		}
		if required {
			authorized = append(authorized, p.Name)
		}
	}
	if len(unread) > 0 {
		return Record{}, fmt.Errorf("whether authorization is in force is not known on %s; record at env, before routes and authorization",
			strings.Join(unread, ", "))
	}
	if len(authorized) > 0 {
		return Record{}, fmt.Errorf("authorization is already in force on %s (its route port's INFO has auth_required): a record taken now can follow a "+
			"restart whose log holds none of the routes opened at env or routes; record at env, before routes and authorization, as a rotation does",
			strings.Join(authorized, ", "))
	}
	slices.SortFunc(rec.Pods, func(a, b PodRecord) int { return strings.Compare(a.Name, b.Name) })
	return rec, nil
}

// Finding is one thing Check concluded: about a pod, or about the hub when Pod is empty.
type Finding struct {
	Pod     string
	Verdict Verdict
	Detail  string
}

func (f Finding) String() string {
	who := f.Pod
	if who == "" {
		who = "the hub"
	}
	return fmt.Sprintf("%s: %s: %s", who, f.Verdict, f.Detail)
}

// Report is what Check found: every finding that is not Clean, in the order it found them.
type Report struct {
	Findings []Finding
}

func (r *Report) add(pod string, verdict Verdict, format string, args ...any) {
	r.Findings = append(r.Findings, Finding{Pod: pod, Verdict: verdict, Detail: fmt.Sprintf(format, args...)})
}

// Verdict is the report's conclusion: PasswordRead when any finding is, since one is enough; else
// Unknown when any finding is; else Clean.
func (r Report) Verdict() Verdict {
	verdict := Clean
	for _, f := range r.Findings {
		if f.Verdict == PasswordRead {
			return PasswordRead
		}
		if f.Verdict == Unknown {
			verdict = Unknown
		}
	}
	return verdict
}

// Route is one route a server's /routez lists: the server at its other end, by id and by the name
// that server chose, and the TCP peer's address.
type Route struct {
	RemoteID   string `json:"remote_id"`
	RemoteName string `json:"remote_name"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
}

// Routez is a server's /routez: its own id, and its routes.
type Routez struct {
	ServerID string  `json:"server_id"`
	Routes   []Route `json:"routes"`
}

// ParseRoutez decodes a /routez answer.
func ParseRoutez(body []byte) (Routez, error) {
	var routez Routez
	if err := json.Unmarshal(body, &routez); err != nil {
		return Routez{}, fmt.Errorf("decode /routez: %w", err)
	}
	return routez, nil
}

// serverID is the server id the pod's /varz answers, which nats-server makes at each start and a
// peer cannot choose, unlike its name.
func serverID(ctx context.Context, src Source, pod string) (string, error) {
	body, err := src.Monitor(ctx, pod, "/varz")
	if err != nil {
		return "", err
	}
	var varz struct {
		ID string `json:"server_id"`
	}
	if err := json.Unmarshal(body, &varz); err != nil {
		return "", fmt.Errorf("decode /varz: %w", err)
	}
	if varz.ID == "" {
		return "", fmt.Errorf("/varz names no server_id")
	}
	return varz.ID, nil
}

// The messages of a nats-server (v2.15.1) log the check reads. A rule reads only a line in the
// logger's form that the server wrote (serversLines), at one level, and matches its message: whole
// for the start line, the hangup and "Reloaded: cluster"; whole or by the prefix that comes before a
// digest for "Reloaded server configuration"; and by prefix for a failed reload, by pattern for a
// route connection's lines, and by substring for a credentialed URL, each in the server's own
// lines, or a connection's lines with their prefix, alone. Never does a rule match the end of a
// line or text anywhere in it: a connection's lines carry text its peer chose, a client's -ERR
// whole at the end of "Client Error" (client.go:2268), so such a rule could be met or tripped by
// any client.
const (
	// startMessage is the first line of a log: Start logs it before anything else (server.go:2295).
	startMessage = "Starting nats-server"
	// hangupMessage begins a reload by SIGHUP, which the chart's config reloader sends
	// (signal.go:58).
	hangupMessage = `Trapped "hangup" signal`
	// clusterReloadedMessage is the line a reload logs when it applies a change to the cluster
	// block's options, the authorization among them (reload.go:470). A change of the routes alone
	// logs "Reloaded: cluster routes" instead (reload.go:553).
	clusterReloadedMessage = "Reloaded: cluster"
	// reloadedMessage is the line a reload logs when it has finished, followed by the new
	// configuration's digest in parentheses when it has one (reload.go:2128-2132).
	reloadedMessage = "Reloaded server configuration"
	// failedReloadMessage begins the line a reload that failed logs, at ERR (signal.go:83). Its
	// error can hold the route password whole: a password outside the minter's rule is printed in
	// it ("integer … is out of the range", or "error parsing route url" with the URL).
	failedReloadMessage = "Failed to reload server configuration: "
)

// logLine is one line of a nats-server log as `kubectl logs --timestamps` gives it: the time the
// container runtime read it at, which no process in the pod sets, then the line as the server's
// logger writes every line (logger/log.go, NewStdLogger: the process id, then Go's log date and
// time to the microsecond, then the level's label), its time read as UTC, the zone of the hub's
// nats image, which sets none. stamped is false for a line without the runtime's time, and form
// for a line that has no logger prefix, the continuation of a message that held a newline. A line
// in the logger's form can still be another process's (serversLines), so form alone does not make
// it the server's.
type logLine struct {
	stamped bool
	runtime time.Time
	form    bool
	pid     string
	at      time.Time
	level   string
	message string
}

var logPrefix = regexp.MustCompile(`^\[(\d+)\] (\d{4}/\d\d/\d\d \d\d:\d\d:\d\d\.\d{6}) \[([A-Z]{3})\] (.*)$`)

// logTime is the layout of logPrefix's time (Go's log.LstdFlags with log.Lmicroseconds).
const logTime = "2006/01/02 15:04:05.000000"

// stampSkew is how far a line's logger time may be from the time the runtime read it: the runtime
// reads each line from the container's output as the server writes it, and the rest allows for a
// node under load.
const stampSkew = 2 * time.Second

// parseLog splits a log into its lines, without the empty ones and each without the carriage
// return and spaces it ends with.
func parseLog(body []byte) []logLine {
	var lines []logLine
	for _, raw := range strings.Split(string(body), "\n") {
		raw = strings.TrimRight(raw, "\r ")
		if raw == "" {
			continue
		}
		var l logLine
		stamp, rest, _ := strings.Cut(raw, " ")
		if runtime, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			l.stamped, l.runtime = true, runtime.UTC()
			if match := logPrefix.FindStringSubmatch(rest); match != nil {
				if at, err := time.ParseInLocation(logTime, match[2], time.UTC); err == nil {
					l.form, l.pid, l.at, l.level, l.message = true, match[1], at, match[3], strings.TrimRight(match[4], " ")
				}
			}
		}
		lines = append(lines, l)
	}
	return lines
}

// agrees reports whether l's logger time is within stampSkew of the time the runtime read it.
func (l logLine) agrees() bool {
	skew := l.runtime.Sub(l.at)
	return l.form && skew < stampSkew && skew > -stampSkew
}

// is reports whether l is a line in the logger's form of level and exactly message.
func (l logLine) is(level, message string) bool {
	return l.form && l.level == level && l.message == message
}

// connectionMessage finds a message a connection logged, which begins with its prefix: the peer's
// address and the connection's kind and id (client.go:808-835), or an internal client's name. Its
// text after the prefix can hold what the peer sent, so no rule that a peer could meet or trip
// reads it as the server's own.
var connectionMessage = regexp.MustCompile(`^(?:\S+ - (?:cid|wid|rid|gid|lid|mid)[a-z]*:\d+|SYSTEM|JETSTREAM|ACCOUNT) - `)

// serversOwn reports whether l is a line the server logged of itself, not of a connection.
func (l logLine) serversOwn() bool { return l.form && !connectionMessage.MatchString(l.message) }

// routeCreated finds the message a server logs for each route connection it makes, accepted or
// solicited, with its peer's address and the connection's id: `<ip>:<port> - rid:<n> - Route
// connection created` (route.go:2104, the connection's prefix at client.go:821, the address from
// RemoteAddr at :785-790).
var routeCreated = regexp.MustCompile(`^(\S+) - rid:(\d+) - Route connection created$`)

// routeAuthError finds the message a server logs, at ERR, when a route connection's CONNECT fails
// the credential check (client.go:2448, 2586-2588), with the peer's address and the connection's id.
var routeAuthError = regexp.MustCompile(`^(\S+) - rid:(\d+) - authentication error(?: - .*)?$`)

// routeClosed finds the message a server logs, at INF, when a route connection closes, with the
// connection's id and its reason (client.go:2128-2130). The reason ends with "- Remote: <name>"
// once the peer's INFO named a server with a name that is not empty; a login the credential
// refused, a peer that waited out the auth timeout or closed at once, and a peer that named itself
// with an empty name end without it.
var routeClosed = regexp.MustCompile(`^\S+ - rid:(\d+) - Router connection closed: (.+)$`)

// remoteTag is in a route's close line once its peer named a server.
const remoteTag = " - Remote: "

// credentialedURL finds a URL that carries a user and a password: a route URL the server printed
// whole, as "Removed route %v" does on a rollback (reload.go:543). It is read in INF lines alone:
// the auth callout's WRN lines carry its service's error text (auth_callout.go:460).
var credentialedURL = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/@:]*:[^\s/@]*@`)

// The first line's time against its container's start: the API server gives a container's start
// to the second, cut down, and nats-server logs its start line as it starts, so the line is
// written no earlier than startSlack before that time and no later than startDelay after it.
const (
	startSlack = time.Second
	startDelay = 10 * time.Second
)

// reloadSpan is how long before the "Reloaded: cluster" line of the reload that loaded
// authorization the log rule's window opens, by the time the container runtime read each line,
// which no process in the pod sets: a reload logs its hangup and that line within milliseconds,
// and the rest allows for a node under load. A marker line cannot open the window instead, since a
// route peer can write one (serversLines).
const reloadSpan = 5 * time.Second

// podLoopback are the addresses of a pod's own network namespace: the port-forward of a check's
// route port reads connects from one, and so does any process of the pod, which the check cannot
// tell from the server's own (Check's limit).
var podLoopback = map[string]bool{"127.0.0.1": true, "::1": true}

// authState is what a pod's route port says of authorization (checkRoutePort).
type authState int

const (
	// authUnread: its INFO could not be read.
	authUnread authState = iota
	// authOff: its INFO has no auth_required.
	authOff
	// authOn: its INFO has auth_required.
	authOn
)

// server is a pod Check could read, with the server id its /varz answered, empty when it could
// not be read.
type server struct {
	pod Pod
	id  string
}

// Check reads src's pods and judges them against rec, in one pass over the pods (Verdict). For
// each pod it requires that the uid, the nats container's restart count and its start are the ones
// rec saved, so the container's log reaches back to the record, and it reads the pod's IP and its
// server id (/varz). Then, for each pod:
//
//   - its /routez: every route's ip a pod IP and its remote_id the server id of another pod, and a
//     route to each of its peers. It never trusts a route's remote_name, which the peer chooses:
//     nats-server refuses an accepted route's name only when it equals the accepting server's own
//     (route.go:745-751).
//   - its route port: its INFO has auth_required, and it refuses a route login with no user and
//     one with the route user and no password, so authorization is in force with a password.
//   - its log, by the lines the server wrote (serversLines: in the logger's form, with the start
//     line's process id and a time that agrees with the runtime's): it begins with the server's
//     start line, which the runtime read at the container's start, so it reaches back to the
//     record; no reload failed, and no line the server wrote of itself holds a URL with a user and
//     password, since either can hold the password; and in the window that opens reloadSpan before
//     the reload that loaded authorization, by the runtime's time (authorizationWindow), every route
//     connection names a pod IP, by IP alone, since the port of a connection a peer opened is
//     ephemeral (routeConnections). Lines before the window are not judged: no connection then
//     carried the password to an address the configuration does not name, and an env roll's pods
//     log the addresses of the pods before them. A connection from another address is PasswordRead
//     only when it was logged after that reload's "Reloaded: cluster" line and formed a route by
//     the log, its close line naming a server. One logged before that line may predate the
//     reload's new configuration; one whose login the credential refused, or whose close line names
//     no server, may have formed no route, though one the hub dialed carried the password
//     (client.go:2433 checks every CONNECT); and one still open when the log was read may be an
//     accepted connection still waiting for its login, which any process that reaches the route
//     port can hold open. Each of these is Unknown.
//
// A pod whose routes do not reach all its peers, while its log names failed route logins from pod
// IPs, holds another password than its peers: the mesh is split (docs/nats-hub.md, Route
// authorization). Last, it reads the pods again, and a pod whose identity or container changed
// during the pass is Unknown, since what the pass read of it may be of two containers, and so is
// that pod's start line finding, since its log may be of a container that started during the pass.
//
// The IP checks need every pod's IP, and the remote_id checks every pod's server id, so a pod whose
// IP is unknown leaves no route and no log judged, and a pod whose /varz cannot be read leaves the
// IP checks standing and no remote_id judged; each is Unknown, never Clean.
//
// What it cannot see: a process in a hub pod's own network namespace, such as the chart's config
// reloader or its exporter, has that pod's IP or its loopback, so its route reads as the pod's, and
// remote_id tells it apart only while it holds the route.
func Check(ctx context.Context, src Source, rec Record) Report {
	var r Report
	pods, err := src.Pods(ctx)
	if err != nil {
		r.add("", Unknown, "the pods could not be read: %v", err)
		return r
	}
	recorded := make(map[string]PodRecord, len(rec.Pods))
	for _, p := range rec.Pods {
		recorded[p.Name] = p
	}
	ipsKnown, idsKnown := len(pods) > 0, len(pods) > 0
	present := map[string]bool{}
	var servers []server
	for _, p := range pods {
		present[p.Name] = true
		was, ok := recorded[p.Name]
		if !ok {
			r.add(p.Name, Unknown, "is not in the record of %s", rec.RecordedAt.Format(time.RFC3339))
			ipsKnown, idsKnown = false, false
			continue
		}
		if p.UID != was.UID || p.RestartCount != was.RestartCount || !p.StartedAt.Equal(was.StartedAt) {
			r.add(p.Name, PasswordRead, "has run a new container since the record of %s (uid %s, restartCount %d, startedAt %s; recorded %s, %d, %s), "+
				"so its log no longer reaches back to the env roll", rec.RecordedAt.Format(time.RFC3339),
				p.UID, p.RestartCount, p.StartedAt.UTC().Format(time.RFC3339), was.UID, was.RestartCount, was.StartedAt.UTC().Format(time.RFC3339))
		}
		if p.IP == "" {
			r.add(p.Name, Unknown, "has no pod IP")
			ipsKnown, idsKnown = false, false
			continue
		}
		id, err := serverID(ctx, src, p.Name)
		if err != nil {
			r.add(p.Name, Unknown, "its /varz could not be read: %v", err)
			idsKnown = false
		}
		servers = append(servers, server{pod: p, id: id})
	}
	for _, p := range rec.Pods {
		if !present[p.Name] {
			r.add(p.Name, Unknown, "is in the record and not in the StatefulSet now")
			ipsKnown, idsKnown = false, false
		}
	}
	if !ipsKnown {
		r.add("", Unknown, "the hub's pods are not all known, so no route and no log is judged")
		return r
	}
	ips := map[string]bool{}
	ids := map[string]string{}
	for _, s := range servers {
		ips[s.pod.IP] = true
		if s.id == "" {
			continue
		}
		if other, taken := ids[s.id]; taken {
			r.add(s.pod.Name, Unknown, "answered the server id %s that %s answered too", s.id, other)
			idsKnown = false
		}
		ids[s.id] = s.pod.Name
	}
	if !idsKnown {
		r.add("", Unknown, "the hub's server ids are not all known, so routes are judged by IP alone")
		ids = nil
	}
	startFindings := map[string]int{}
	for _, s := range servers {
		split := checkRoutes(ctx, &r, src, s, ips, ids, len(servers)-1)
		auth := checkRoutePort(ctx, &r, src, s.pod.Name)
		startFindings[s.pod.Name] = checkLog(ctx, &r, src, s, ips, split, auth)
	}
	for pod := range checkUnchanged(ctx, &r, src, pods) {
		if i, found := startFindings[pod]; found && i >= 0 {
			r.Findings[i].Verdict = Unknown
			r.Findings[i].Detail += "; the pod changed during the check, so this log may be of a container that started during it, which the next " +
				"check judges against the record"
		}
	}
	return r
}

// checkUnchanged reads src's pods again, after the pass, and finds each pod whose uid, IP, restart
// count or start is not the one before, or that left: a container that started during the pass can
// have answered some reads and not others, its log among them, and the next check, against the
// record, would find its restart. It returns the pods it found changed or gone.
func checkUnchanged(ctx context.Context, r *Report, src Source, before []Pod) map[string]bool {
	changed := map[string]bool{}
	after, err := src.Pods(ctx)
	if err != nil {
		r.add("", Unknown, "the pods could not be read again after the pass, so a restart during it cannot be ruled out: %v", err)
		return changed
	}
	now := make(map[string]Pod, len(after))
	for _, p := range after {
		now[p.Name] = p
	}
	for _, p := range before {
		q, ok := now[p.Name]
		switch {
		case !ok:
			r.add(p.Name, Unknown, "left the StatefulSet during the check; run it again")
			changed[p.Name] = true
		case q.UID != p.UID || q.IP != p.IP || q.RestartCount != p.RestartCount || !q.StartedAt.Equal(p.StartedAt):
			r.add(p.Name, Unknown, "changed during the check (at its start: uid %s, ip %s, restartCount %d, startedAt %s; at its end: uid %s, ip %s, "+
				"restartCount %d, startedAt %s), so what it read of the pod may be of two containers; run it again", p.UID, p.IP, p.RestartCount,
				p.StartedAt.UTC().Format(time.RFC3339), q.UID, q.IP, q.RestartCount, q.StartedAt.UTC().Format(time.RFC3339))
			changed[p.Name] = true
		}
		delete(now, p.Name)
	}
	for _, q := range after {
		if _, joined := now[q.Name]; joined {
			r.add(q.Name, Unknown, "joined the StatefulSet during the check; run it again")
		}
	}
	return changed
}

// checkRoutes judges s's /routez: every route's ip a pod IP and, when ids holds every pod's server
// id, its remote_id another pod's, and a route to each of s's peers. It returns whether s's routes
// are known not to reach all its peers.
func checkRoutes(ctx context.Context, r *Report, src Source, s server, ips map[string]bool, ids map[string]string, peerCount int) bool {
	body, err := src.Monitor(ctx, s.pod.Name, "/routez")
	if err != nil {
		r.add(s.pod.Name, Unknown, "its /routez could not be read: %v", err)
		return false
	}
	routez, err := ParseRoutez(body)
	if err != nil {
		r.add(s.pod.Name, Unknown, "%v", err)
		return false
	}
	if s.id != "" && routez.ServerID != s.id {
		r.add(s.pod.Name, Unknown, "its /routez answered for the server %s, and its /varz for %s", routez.ServerID, s.id)
		return false
	}
	peers := map[string]bool{}
	for _, route := range routez.Routes {
		// A route's remote_id and remote_name are what its peer sent (route.go:820), so each is
		// quoted: a peer can put a newline or a terminal escape in either.
		if !ips[route.IP] {
			r.add(s.pod.Name, PasswordRead, "has a route from an address that is no pod of the hub: ip %s, remote_id %q, remote_name %q",
				route.IP, route.RemoteID, route.RemoteName)
			continue
		}
		if ids == nil {
			continue
		}
		if owner, hub := ids[route.RemoteID]; !hub || owner == s.pod.Name {
			r.add(s.pod.Name, PasswordRead, "has a route to a server that is none of the hub's others: ip %s, remote_id %q, remote_name %q",
				route.IP, route.RemoteID, route.RemoteName)
			continue
		}
		peers[route.RemoteID] = true
	}
	if ids != nil && len(peers) < peerCount {
		r.add(s.pod.Name, Unknown, "has routes to %d of its %d peers, so the hub's mesh is not whole", len(peers), peerCount)
		return true
	}
	return false
}

// checkLog judges the pod's nats container log, read with the container runtime's times: it keeps
// the lines its server wrote (serversLines), judges the server's lines of itself (ownLines), opens
// the window of the reload that loaded authorization (authorizationWindow) and judges each route
// connection in it (routeConnections). When auth says authorization is not in force, it says so,
// with what the log shows of why, since only it knows whether it read the log. It returns the index
// in r.Findings of its start line finding, or -1 when it made none.
func checkLog(ctx context.Context, r *Report, src Source, s server, ips map[string]bool, split bool, auth authState) int {
	pod := s.pod.Name
	body, err := src.Log(ctx, pod)
	if err != nil {
		r.add(pod, Unknown, "its nats container's log could not be read: %v", err)
		if auth == authOff {
			r.add(pod, Unknown, "authorization is not in force: its route port's INFO has no auth_required; apply authorization, then check again")
		}
		return -1
	}
	lines, startFinding := serversLines(r, s, parseLog(body))
	ownLines(r, pod, lines)
	window, from, loaded := authorizationWindow(r, s, lines, auth)
	routeConnections(r, pod, window, from, ips, split)
	if auth == authOff {
		why := "no reload has loaded authorization since the server started"
		if loaded {
			why = "a reload that changed its cluster block left it without authorization"
		}
		r.add(pod, Unknown, "authorization is not in force: its route port's INFO has no auth_required, and %s; apply authorization, then check again", why)
	}
	return startFinding
}

// serversLines returns, in order, the lines of a log that its server wrote: lines in the logger's
// form, each read by the runtime within stampSkew of its logger time and carrying the start line's
// process id. The start line is the server's when the runtime read it at the container's start,
// since a line a peer forges carries the time the runtime read the line it was written in; a log
// that does not begin with it may have lost its lines since the env roll, which is PasswordRead.
// A log with lines that carry no time of the runtime's is judged by none of this, since which of
// its lines the server wrote cannot be told: it is Unknown, and serversLines returns no line.
// Every rule after this one reads only the lines it returns. It returns, too, the index in
// r.Findings of its start line finding, or -1.
func serversLines(r *Report, s server, lines []logLine) ([]logLine, int) {
	pod := s.pod.Name
	unstamped := 0
	for _, l := range lines {
		if !l.stamped {
			unstamped++
		}
	}
	if unstamped > 0 {
		r.add(pod, Unknown, "%d of its log's %d line(s) carry no time of the container runtime's, so which lines its server wrote cannot be told; "+
			"read the log as kubectl logs --timestamps gives it", unstamped, len(lines))
		return nil, -1
	}
	startPID, startFinding := "", -1
	switch {
	case len(lines) == 0 || !lines[0].is("INF", startMessage) || !lines[0].agrees():
		startFinding = len(r.Findings)
		r.add(pod, PasswordRead, "its log does not begin with the server's start line, %q as nats-server's logger writes it at the time the "+
			"container runtime read it, so the lines since the env roll may be gone (the API server returns only the newest log file, which the "+
			"kubelet rotates)", "[INF] "+startMessage)
	case lines[0].runtime.Before(s.pod.StartedAt.Add(-startSlack)) || lines[0].runtime.After(s.pod.StartedAt.Add(startDelay)):
		startFinding = len(r.Findings)
		r.add(pod, PasswordRead, "its log's first line, the server's start line, was read by the container runtime at %s, and its container "+
			"started at %s, so it is no start line of that container and the lines since the env roll may be gone",
			lines[0].runtime.Format(time.RFC3339Nano), s.pod.StartedAt.UTC().Format(time.RFC3339))
	default:
		startPID = lines[0].pid
	}
	var own []logLine
	forged := 0
	for i, l := range lines {
		switch {
		case !l.form:
		case l.agrees() && (startPID == "" || l.pid == startPID):
			own = append(own, l)
		case i > 0:
			forged++
		}
	}
	if forged > 0 {
		r.add(pod, Unknown, "its log holds %d line(s) in nats-server's form that its server did not write when they say: their process id is "+
			"not the start line's, or their time is %s or more from the time the container runtime read them. A route peer writes such lines "+
			"through its server name, which nats-server prints unquoted when the route closes (client.go:2125), and the auth service through "+
			"its error text (auth_callout.go:460); the check reads none of them, and cannot clear the log", forged, stampSkew)
	}
	return own, startFinding
}

// ownLines judges the lines the server wrote of itself, not of a connection: no failed reload, and
// no INF line with a URL that carries a user and a password, since either can hold the password.
func ownLines(r *Report, pod string, lines []logLine) {
	failed, credentialed := 0, 0
	for _, l := range lines {
		if !l.serversOwn() {
			continue
		}
		if l.level == "ERR" && strings.HasPrefix(l.message, failedReloadMessage) {
			failed++
		}
		if l.level == "INF" && credentialedURL.MatchString(l.message) {
			credentialed++
		}
	}
	if failed > 0 {
		r.add(pod, PasswordRead, "its log holds %d failed reload(s) (%q), and a failed load's error line can hold the route password whole, "+
			"as it does for a password outside the minter's rule", failed, "[ERR] "+strings.TrimSuffix(failedReloadMessage, ": "))
	}
	if credentialed > 0 {
		r.add(pod, PasswordRead, "its log holds %d line(s) the server wrote with a URL that carries a user and a password, as a rollback's "+
			"\"Removed route\" lines carry the route password", credentialed)
	}
}

// authorizationWindow returns the server's lines the route connection rule judges, the index among
// them of the first "Reloaded: cluster" line (reload.go:470), of the reload that loaded
// authorization, and whether the log shows that reload. The window opens at the first line the
// container runtime read at most reloadSpan before that line, by the runtime's time, so no line a
// peer writes can move it. With no such reload in the log, it is the whole log, every line judged
// as after the reload, when auth says authorization is in force or that is not known, since the
// server then held it from its start; and empty when auth says it is not, since no connection then
// carried the password beyond the pods. The reload must have finished, and the runtime must have
// read its line more than startDelay after the container's start, or a start line a process wrote
// in that span through a route's name could pass the start line rule; each is Unknown otherwise.
func authorizationWindow(r *Report, s server, lines []logLine, auth authState) ([]logLine, int, bool) {
	pod := s.pod.Name
	at := slices.IndexFunc(lines, func(l logLine) bool { return l.is("INF", clusterReloadedMessage) })
	if at < 0 {
		if auth == authOff {
			return nil, 0, false
		}
		return lines, 0, false
	}
	if !slices.ContainsFunc(lines[at:], func(l logLine) bool {
		return l.is("INF", reloadedMessage) || (l.level == "INF" && strings.HasPrefix(l.message, reloadedMessage+" ("))
	}) {
		r.add(pod, Unknown, "the reload that changed its cluster block has logged no %q", "[INF] "+reloadedMessage)
	}
	loadedAt := lines[at].runtime
	if loadedAt.Before(s.pod.StartedAt.Add(startDelay)) {
		r.add(pod, Unknown, "the container runtime read the line of the reload that loaded authorization, %q, at %s, within %s of its container's "+
			"start at %s, and a start line a process writes in that span through a route's name passes the start line rule, so the log cannot "+
			"be cleared; apply authorization more than %s after a pod starts", "[INF] "+clusterReloadedMessage, loadedAt.Format(time.RFC3339Nano),
			startDelay, s.pod.StartedAt.UTC().Format(time.RFC3339), startDelay)
	}
	from := slices.IndexFunc(lines, func(l logLine) bool { return !l.runtime.Before(loadedAt.Add(-reloadSpan)) })
	return lines[from:], at - from, true
}

// routeConnection counts one address's route connections in the window by what the log shows of
// each.
type routeConnection struct {
	// before: logged before the reload's "Reloaded: cluster" line.
	before int
	// formed: logged at or after it, and its close line names a server.
	formed int
	// refused: its login the route credential refused, with no close line that names a server.
	refused int
	// unnamed: its close line names no server.
	unnamed int
	// open: none of those, so still open when the log was read.
	open int
}

// routeConnections judges each route connection the window names with an address that is neither
// a pod IP nor the pod's loopback. One logged at or after the reload's line, window[from], that
// formed a route by the log, its close line naming a server, is PasswordRead. Every other one is
// Unknown: one logged before that line may predate the reload's new configuration, which the
// reload takes before it logs the line (reload.go:1561); one whose login the credential refused, or
// whose close line names no server, may have formed no route, though on one the hub opened it sent
// the password before the peer named itself (route.go:2087-2094), and a peer can name itself with
// an empty name, which the close line does not show (client.go:2124); and one with neither line is
// still open, and may be an accepted connection waiting for its login, which nats-server logs on
// accept (route.go:2104) and closes at the cluster's auth timeout (route.go:2070-2073), so any
// process that reaches the route port can hold one open while the log is read. When split, it
// names the failed route logins from pod IPs that split the mesh. A host is quoted, since a line
// can carry the address another process wrote.
func routeConnections(r *Report, pod string, window []logLine, from int, ips map[string]bool, split bool) {
	refused, named, unnamed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	logins := map[string]int{}
	var loginOrder []string
	for _, l := range window {
		if match := routeAuthError.FindStringSubmatch(l.message); l.level == "ERR" && match != nil {
			refused[match[2]] = true
			if host, _, err := net.SplitHostPort(match[1]); err == nil && ips[host] {
				if logins[host] == 0 {
					loginOrder = append(loginOrder, host)
				}
				logins[host]++
			}
		}
		if match := routeClosed.FindStringSubmatch(l.message); l.level == "INF" && match != nil {
			if strings.Contains(match[2], remoteTag) {
				named[match[1]] = true
			} else {
				unnamed[match[1]] = true
			}
		}
	}
	counts := map[string]*routeConnection{}
	var order []string
	for i, l := range window {
		match := routeCreated.FindStringSubmatch(l.message)
		if l.level != "INF" || match == nil {
			continue
		}
		host, _, err := net.SplitHostPort(match[1])
		if err != nil {
			r.add(pod, Unknown, "its log names a route connection with %q, which is no host and port", match[1])
			continue
		}
		if ips[host] || podLoopback[host] {
			continue
		}
		c := counts[host]
		if c == nil {
			c = &routeConnection{}
			counts[host] = c
			order = append(order, host)
		}
		switch rid := match[2]; {
		case i < from:
			c.before++
		case named[rid]:
			c.formed++
		case refused[rid]:
			c.refused++
		case unnamed[rid]:
			c.unnamed++
		default:
			c.open++
		}
	}
	for _, host := range order {
		if c := counts[host]; c.formed > 0 {
			r.add(pod, PasswordRead, "its log names %d route connection(s) with %q since it loaded authorization, and %q is no pod of the hub: "+
				"each formed a route, its close line naming the server its peer named", c.formed, host, host)
		}
	}
	for _, host := range order {
		if c := counts[host]; c.refused+c.unnamed > 0 {
			r.add(pod, Unknown, "its log names %d route connection(s) with %q since it loaded authorization whose peer named no server (%d whose login "+
				"the route credential refused, %d whose close line names no server), and %q is no pod of the hub: such a connection can still have "+
				"carried the password, since on one the hub opened it sent the password before the peer named itself (route.go:2087-2094), and a "+
				"peer can name itself with an empty name, which the close line does not show; and the log does not say which side opened it, so the "+
				"check neither condemns nor clears %q. Find what ran there, and treat the password as read unless it is accounted for",
				c.refused+c.unnamed, host, c.refused, c.unnamed, host, host)
		}
	}
	for _, host := range order {
		if c := counts[host]; c.open > 0 {
			r.add(pod, Unknown, "its log names %d route connection(s) with %q since it loaded authorization that were still open when the log was "+
				"read, with no login refused and no close line, and %q is no pod of the hub: nats-server logs a route connection as it accepts it, "+
				"before any login (route.go:2104), so any process that reaches the route port can hold one open while the log is read, and one "+
				"that held a route when /routez was read is named there. The server closes an accepted connection that logs in to no route at the "+
				"cluster's auth timeout (route.go:2070-2073): check again", c.open, host, host)
		}
	}
	for _, host := range order {
		if c := counts[host]; c.before > 0 {
			r.add(pod, Unknown, "its log names %d route connection(s) with %q in the %s before the reload that loaded authorization logged %q, and "+
				"%q is no pod of the hub: the reload takes its new configuration before it logs that line (reload.go:1561), so such a connection "+
				"carried no password if it came before, and, if the hub dialed it after, the password (route.go:1055-1093); the log cannot tell "+
				"which. Find what ran there, and treat the password as read unless it is accounted for", c.before, host, reloadSpan,
				"[INF] "+clusterReloadedMessage, host)
		}
	}
	if split && len(loginOrder) > 0 {
		var from []string
		for _, host := range loginOrder {
			from = append(from, fmt.Sprintf("%d from %s", logins[host], host))
		}
		r.add(pod, Unknown, "the hub's servers do not hold one route password: its routes do not reach all its peers, and its log names failed route "+
			"logins from pod IPs since it loaded authorization (%s); bring every pod to one password from the env stage (docs/nats-hub.md, Route authorization)",
			strings.Join(from, ", "))
	}
}

// routeUser is the user every route of the hub presents, gcp:ClusteredNatsHub's
// clusteredHubRouteUser.
const routeUser = "route"

// probeGateway names the route logins the check tries as a gateway's, so a login the credential
// takes is refused next, before it can join, for a gateway on the route port
// (route.go:3065-3073), and changes nothing on the server: processConnect checks the credential
// first (client.go:2433) and only then hands the CONNECT to the route.
const probeGateway = "envoy-infra-routes-probe"

// routeLogins are the logins checkRoutePort tries on a route port that requires authorization:
// none must be taken. The route user with no password is taken by a server whose password is
// empty, which nats-server allows, and which every peer then has.
var routeLogins = []struct {
	what    string
	connect []byte
}{
	{"no user", []byte(`CONNECT {"verbose":false,"pedantic":false,"tls_required":false,"gateway":"` + probeGateway + `"}` + "\r\nPING\r\n")},
	{"the user " + routeUser + " and no password", []byte(`CONNECT {"verbose":false,"pedantic":false,"tls_required":false,"user":"` + routeUser +
		`","pass":"","gateway":"` + probeGateway + `"}` + "\r\nPING\r\n")},
}

// refusedLogin is what a route port answers a login the credential refuses (client.go:2595).
const refusedLogin = "-ERR 'Authorization Violation'"

// checkRoutePort reads what the pod's route port says of authorization (routePortAuthRequired).
// When it requires authorization, the route port must refuse each of routeLogins, or
// authorization fences nothing.
func checkRoutePort(ctx context.Context, r *Report, src Source, pod string) authState {
	required, err := routePortAuthRequired(ctx, src, pod)
	if err != nil {
		r.add(pod, Unknown, "%v", err)
		return authUnread
	}
	if !required {
		return authOff
	}
	for _, login := range routeLogins {
		_, answer, err := src.RouteExchange(ctx, pod, login.connect)
		if err != nil {
			r.add(pod, Unknown, "its route port's answer to a route login with %s could not be read, so authorization may fence nothing: %v", login.what, err)
			continue
		}
		if got := strings.TrimSpace(string(answer)); got != refusedLogin {
			r.add(pod, Unknown, "authorization is not in force: its route port took a route login with %s (it answered %q, not %q), so any process "+
				"can join as a route; roll out a password that keeps the minter's rule from env (docs/nats-hub.md, Route authorization)",
				login.what, got, refusedLogin)
		}
	}
	return authOn
}

// routePortAuthRequired reads the first line the pod's route port sends as its INFO, and returns
// its auth_required, which a server sets exactly when its cluster authorization has a user
// (reload.go:423). Its error says what it could not read.
func routePortAuthRequired(ctx context.Context, src Source, pod string) (bool, error) {
	line, _, err := src.RouteExchange(ctx, pod, nil)
	if err != nil {
		return false, fmt.Errorf("its route port's INFO could not be read: %w", err)
	}
	text, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "INFO ")
	var info struct {
		AuthRequired bool `json:"auth_required"`
	}
	if !ok || json.Unmarshal([]byte(text), &info) != nil {
		return false, errors.New("its route port's first line is no INFO")
	}
	return info.AuthRequired, nil
}
