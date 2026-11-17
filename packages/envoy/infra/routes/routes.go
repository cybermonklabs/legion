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
	// Clean: authorization is in force on every pod, every route of every pod joins two of the
	// hub's own servers, by pod IP and by server id, every route connection each server has logged
	// since it loaded authorization was with a pod IP, and no pod has restarted since the record.
	Clean Verdict = 0
	// PasswordRead: a process that is none of the hub's servers held a route, or may have been
	// handed the password and the record of it is gone, or the password is in a log line. Treat
	// the route password as read: cut that process's reach and rotate the password
	// (docs/nats-hub.md, Route authorization).
	PasswordRead Verdict = 1
	// Unknown: something the check needs could not be read, or is not yet so (authorization not in
	// force, a mesh that is not whole, a pod that changed during the check), or a route login from
	// another address was refused, which it cannot tell from one the hub dialed; so it concludes
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
// container as the API server returns it, the current container's newest file; RouteExchange
// opens a connection to a pod's route port and returns the first line it sends, its INFO, and,
// when send is not empty, writes send and returns the first line after the INFO that answers it
// (Exchange). Close ends whatever the source holds open.
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
		info, _, err := src.RouteExchange(ctx, p.Name, nil)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s (%v)", p.Name, err))
			continue
		}
		required, err := parseAuthRequired(info)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s (%v)", p.Name, err))
			continue
		}
		if required {
			authorized = append(authorized, p.Name)
		}
	}
	if len(unread) > 0 {
		return Record{}, fmt.Errorf("the route port's INFO of %s could not be read, so whether authorization is in force is not known; record at env, "+
			"before routes and authorization", strings.Join(unread, ", "))
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

// The messages of a nats-server (v2.15.1) log the check reads. Each is matched whole, as the
// message of a line logLine parsed, at its level, never as a substring of a line: a connection's
// lines carry text its peer chose, a client's -ERR whole at the end of "Client Error" (client.go:
// 2268), so a rule that matched the end of a line, or anywhere in it, could be met or tripped by
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

// logLine is one line of a nats-server log, as its logger writes every line (logger/log.go,
// NewStdLogger: the process id, then Go's log date and time to the microsecond, then the level's
// label): its time, read as UTC, the zone of the hub's nats image, which sets none; its level; and
// its message. ok is false for a line that has no such prefix, the continuation of a message that
// held a newline, which no rule reads.
type logLine struct {
	ok      bool
	at      time.Time
	level   string
	message string
}

var logPrefix = regexp.MustCompile(`^\[\d+\] (\d{4}/\d\d/\d\d \d\d:\d\d:\d\d\.\d{6}) \[([A-Z]{3})\] (.*)$`)

// logTime is the layout of logPrefix's time (Go's log.LstdFlags with log.Lmicroseconds).
const logTime = "2006/01/02 15:04:05.000000"

// parseLog splits a log into its lines, without the empty ones and each without the carriage
// return and spaces it ends with.
func parseLog(body []byte) []logLine {
	var lines []logLine
	for _, raw := range strings.Split(string(body), "\n") {
		raw = strings.TrimRight(raw, "\r ")
		if raw == "" {
			continue
		}
		match := logPrefix.FindStringSubmatch(raw)
		if match == nil {
			lines = append(lines, logLine{})
			continue
		}
		at, err := time.ParseInLocation(logTime, match[1], time.UTC)
		if err != nil {
			lines = append(lines, logLine{})
			continue
		}
		lines = append(lines, logLine{ok: true, at: at, level: match[2], message: strings.TrimRight(match[3], " ")})
	}
	return lines
}

// is reports whether l is the server's line of level and exactly message.
func (l logLine) is(level, message string) bool {
	return l.ok && l.level == level && l.message == message
}

// connectionMessage finds a message a connection logged, which begins with its prefix: the peer's
// address and the connection's kind and id (client.go:808-835), or an internal client's name. Its
// text after the prefix can hold what the peer sent, so no rule that a peer could meet or trip
// reads it as the server's own.
var connectionMessage = regexp.MustCompile(`^(?:\S+ - (?:cid|wid|rid|gid|lid|mid)[a-z]*:\d+|SYSTEM|JETSTREAM|ACCOUNT) - `)

// serversOwn reports whether l is a line the server logged of itself, not of a connection.
func (l logLine) serversOwn() bool { return l.ok && !connectionMessage.MatchString(l.message) }

// routeCreated finds the message a server logs for each route connection it makes, accepted or
// solicited, with its peer's address and the connection's id: `<ip>:<port> - rid:<n> - Route
// connection created` (route.go:2104, the connection's prefix at client.go:821, the address from
// RemoteAddr at :785-790).
var routeCreated = regexp.MustCompile(`^(\S+) - rid:(\d+) - Route connection created$`)

// routeAuthError finds the message a server logs, at ERR, when a route connection's CONNECT fails
// the credential check (client.go:2448, 2586-2588), with the peer's address and the connection's id.
var routeAuthError = regexp.MustCompile(`^(\S+) - rid:(\d+) - authentication error(?: - .*)?$`)

// credentialedURL finds a URL that carries a user and a password: a route URL the server printed
// whole, as "Removed route %v" does on a rollback (reload.go:543).
var credentialedURL = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/@:]*:[^\s/@]*@`)

// The first line's time against its container's start: the API server gives a container's start
// to the second, cut down, and nats-server logs its start line as it starts, so the line is
// written no earlier than startSlack before that time and no later than startDelay after it.
const (
	startSlack = time.Second
	startDelay = 10 * time.Second
)

// podLoopback are the addresses of a pod's own network namespace: the port-forward of a check's
// route port reads connects from one, and so does any process of the pod, which the check cannot
// tell from the server's own (Check's limit).
var podLoopback = map[string]bool{"127.0.0.1": true, "::1": true}

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
//   - its log, by the lines the server's logger wrote (logLine): it begins with the server's start
//     line, written at its container's start, so it reaches back to the record; no reload failed,
//     and no line the server wrote of itself holds a URL with a user and password, since either
//     can hold the password; and from the reload that loaded authorization on, every route
//     connection names a pod IP, by IP alone, since the port of a connection a peer opened is
//     ephemeral. Lines before that reload are not judged: no connection then carried the password
//     to an address the configuration does not name, and an env roll's pods log the addresses of
//     the pods before them. A connection from another address whose login the credential refused
//     is Unknown, not PasswordRead: a refused login carries no password, but the log does not say
//     which side opened the connection, and a process the hub dialed with the password can send a
//     refused login of its own on it (client.go:2433 checks every CONNECT).
//
// A pod whose routes do not reach all its peers, while its log names failed route logins from pod
// IPs, holds another password than its peers: the mesh is split (docs/nats-hub.md, Route
// authorization). Last, it reads the pods again, and a pod whose identity or container changed
// during the pass is Unknown, since what the pass read of it may be of two containers.
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
	for _, s := range servers {
		split := checkRoutes(ctx, &r, src, s, ips, ids, len(servers)-1)
		auth, authKnown := checkRoutePort(ctx, &r, src, s.pod.Name)
		loaded := checkLog(ctx, &r, src, s, ips, split, auth || !authKnown)
		if authKnown && !auth {
			why := "no reload has loaded authorization since the server started"
			if loaded {
				why = "a reload that changed its cluster block left it without authorization"
			}
			r.add(s.pod.Name, Unknown, "authorization is not in force: its route port's INFO has no auth_required, and %s; apply authorization, then check again", why)
		}
	}
	checkUnchanged(ctx, &r, src, pods)
	return r
}

// checkUnchanged reads src's pods again, after the pass, and finds each pod whose uid, IP, restart
// count or start is not the one before: a container that started during the pass can have answered
// some reads and not others, its log among them, and the next check, against the record, would
// find its restart.
func checkUnchanged(ctx context.Context, r *Report, src Source, before []Pod) {
	after, err := src.Pods(ctx)
	if err != nil {
		r.add("", Unknown, "the pods could not be read again after the pass, so a restart during it cannot be ruled out: %v", err)
		return
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
		case q.UID != p.UID || q.IP != p.IP || q.RestartCount != p.RestartCount || !q.StartedAt.Equal(p.StartedAt):
			r.add(p.Name, Unknown, "changed during the check (at its start: uid %s, ip %s, restartCount %d, startedAt %s; at its end: uid %s, ip %s, "+
				"restartCount %d, startedAt %s), so what it read of the pod may be of two containers; run it again", p.UID, p.IP, p.RestartCount,
				p.StartedAt.UTC().Format(time.RFC3339), q.UID, q.IP, q.RestartCount, q.StartedAt.UTC().Format(time.RFC3339))
		}
		delete(now, p.Name)
	}
	for _, q := range after {
		if _, joined := now[q.Name]; joined {
			r.add(q.Name, Unknown, "joined the StatefulSet during the check; run it again")
		}
	}
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
		if !ips[route.IP] {
			r.add(s.pod.Name, PasswordRead, "has a route from an address that is no pod of the hub: ip %s, remote_id %s, remote_name %q",
				route.IP, route.RemoteID, route.RemoteName)
			continue
		}
		if ids == nil {
			continue
		}
		if owner, hub := ids[route.RemoteID]; !hub || owner == s.pod.Name {
			r.add(s.pod.Name, PasswordRead, "has a route to a server that is none of the hub's others: ip %s, remote_id %s, remote_name %q",
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

// checkLog judges the pod's nats container log, by the lines the server's logger wrote: it begins
// with the server's start line, written at the container's start; no reload failed and no line the
// server wrote of itself holds a URL with a user and password; from the reload that loaded
// authorization on, every route connection it names has a pod IP, or else the credential refused
// its login; and, when split, it names the failed route logins from pod IPs that split the mesh.
// With no reload that changed the cluster block in the log, the whole log is judged when
// sinceStart says authorization was in force from the server's start (or that is not known), and
// no route connection when it was never in force, since none then carried the password beyond the
// pods. It returns whether the log shows authorization loaded by a reload.
func checkLog(ctx context.Context, r *Report, src Source, s server, ips map[string]bool, split, sinceStart bool) bool {
	pod := s.pod.Name
	body, err := src.Log(ctx, pod)
	if err != nil {
		r.add(pod, Unknown, "its nats container's log could not be read: %v", err)
		return false
	}
	lines := parseLog(body)
	switch {
	case len(lines) == 0 || !lines[0].is("INF", startMessage):
		r.add(pod, PasswordRead, "its log does not begin with the server's start line, %q as nats-server's logger writes it, so the lines since "+
			"the env roll may be gone (the API server returns only the newest log file, which the kubelet rotates)", "[INF] "+startMessage)
	case lines[0].at.Before(s.pod.StartedAt.Add(-startSlack)) || lines[0].at.After(s.pod.StartedAt.Add(startDelay)):
		r.add(pod, PasswordRead, "its log's first line, the server's start line, was written at %s, and its container started at %s, so it is no "+
			"start line of that container and the lines since the env roll may be gone (the log's times are read as UTC)",
			lines[0].at.Format(time.RFC3339Nano), s.pod.StartedAt.UTC().Format(time.RFC3339))
	}
	failed, credentialed := 0, 0
	for _, l := range lines {
		if !l.serversOwn() {
			continue
		}
		if l.level == "ERR" && strings.HasPrefix(l.message, failedReloadMessage) {
			failed++
		}
		if credentialedURL.MatchString(l.message) {
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

	// The window opens at the hangup that began the first reload which changed the cluster block.
	start, loaded := 0, false
	if !sinceStart {
		start = len(lines)
	}
	for i, l := range lines {
		if !l.is("INF", clusterReloadedMessage) {
			continue
		}
		loaded = true
		start = i
		for j := i; j >= 0; j-- {
			if lines[j].is("INF", hangupMessage) {
				start = j
				break
			}
		}
		if !slices.ContainsFunc(lines[i:], func(l logLine) bool {
			return l.is("INF", reloadedMessage) || (l.ok && l.level == "INF" && strings.HasPrefix(l.message, reloadedMessage+" ("))
		}) {
			r.add(pod, Unknown, "the reload that changed its cluster block has logged no %q", "[INF] "+reloadedMessage)
		}
		break
	}
	window := lines[start:]

	// A connection whose login the credential refused, by its id.
	refused := map[string]bool{}
	logins := map[string]int{}
	var loginOrder []string
	for _, l := range window {
		match := routeAuthError.FindStringSubmatch(l.message)
		if !l.ok || l.level != "ERR" || match == nil {
			continue
		}
		refused[match[2]] = true
		if host, _, err := net.SplitHostPort(match[1]); err == nil && ips[host] {
			if logins[host] == 0 {
				loginOrder = append(loginOrder, host)
			}
			logins[host]++
		}
	}
	accepted, refusedFrom := map[string]int{}, map[string]int{}
	var order, refusedOrder []string
	for _, l := range window {
		match := routeCreated.FindStringSubmatch(l.message)
		if !l.ok || l.level != "INF" || match == nil {
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
		if refused[match[2]] {
			if refusedFrom[host] == 0 {
				refusedOrder = append(refusedOrder, host)
			}
			refusedFrom[host]++
			continue
		}
		if accepted[host] == 0 {
			order = append(order, host)
		}
		accepted[host]++
	}
	for _, host := range order {
		r.add(pod, PasswordRead, "its log names %d route connection(s) with %s since it loaded authorization, and %s is no pod of the hub",
			accepted[host], host, host)
	}
	for _, host := range refusedOrder {
		r.add(pod, Unknown, "its log names %d route connection(s) with %s since it loaded authorization whose login the route credential refused, "+
			"and %s is no pod of the hub: a refused login carries no password, but the log does not say which side opened a connection, and a "+
			"process the hub dialed with the password can send a refused login of its own, so the check cannot clear %s; find what ran there, "+
			"and treat the password as read unless it is accounted for", refusedFrom[host], host, host, host)
	}
	if split && len(loginOrder) > 0 {
		var counts []string
		for _, host := range loginOrder {
			counts = append(counts, fmt.Sprintf("%d from %s", logins[host], host))
		}
		r.add(pod, Unknown, "the hub's servers do not hold one route password: its routes do not reach all its peers, and its log names failed route "+
			"logins from pod IPs since it loaded authorization (%s); bring every pod to one password from the env stage (docs/nats-hub.md, Route authorization)",
			strings.Join(counts, ", "))
	}
	return loaded
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

// checkRoutePort reads the first line the pod's route port sends, and returns whether its INFO has
// auth_required, which a server sets exactly when its cluster authorization has a user
// (reload.go:423): whether authorization is in force; and whether that is known. When it is, the
// route port must refuse each of routeLogins, or authorization fences nothing.
func checkRoutePort(ctx context.Context, r *Report, src Source, pod string) (bool, bool) {
	info, _, err := src.RouteExchange(ctx, pod, nil)
	if err != nil {
		r.add(pod, Unknown, "its route port's INFO could not be read: %v", err)
		return false, false
	}
	required, err := parseAuthRequired(info)
	if err != nil {
		r.add(pod, Unknown, "%v", err)
		return false, false
	}
	if !required {
		return false, true
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
	return true, true
}

// parseAuthRequired reads a route port's first line as its INFO, and returns its auth_required.
func parseAuthRequired(line []byte) (bool, error) {
	text, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "INFO ")
	var info struct {
		AuthRequired bool `json:"auth_required"`
	}
	if !ok || json.Unmarshal([]byte(text), &info) != nil {
		return false, errors.New("its route port's first line is no INFO")
	}
	return info.AuthRequired, nil
}
