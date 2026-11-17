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
	// force, a mesh that is not whole), so it concludes nothing, and in particular not Clean.
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
// container as the API server returns it, the current container's newest file; RouteInfo is the
// first line a pod's route port sends a connection, its INFO. Close ends whatever the source holds
// open.
type Source interface {
	Pods(ctx context.Context) ([]Pod, error)
	Monitor(ctx context.Context, pod, path string) ([]byte, error)
	Log(ctx context.Context, pod string) ([]byte, error)
	RouteInfo(ctx context.Context, pod string) ([]byte, error)
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
// roll has finished: a pod that starts after it would have a log the record does not cover.
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

// The lines of a nats-server log (v2.15.1) the check reads.
const (
	// startLine ends the first line of a log: Start logs it before anything else (server.go:2295).
	startLine = "[INF] Starting nats-server"
	// hangupLine begins a reload by SIGHUP, which the chart's config reloader sends (signal.go:58).
	hangupLine = `[INF] Trapped "hangup" signal`
	// clusterReloadedLine ends the line a reload logs when it applies a change to the cluster
	// block's options, the authorization among them (reload.go:470). A change of the routes alone
	// logs "Reloaded: cluster routes" instead (reload.go:553).
	clusterReloadedLine = "[INF] Reloaded: cluster"
	// reloadedLine begins the line a reload logs when it has finished (reload.go:2132).
	reloadedLine = "[INF] Reloaded server configuration"
	// failedReloadLine is in the line a reload that failed logs (signal.go:83). Its error can hold
	// the route password whole: a password outside the minter's rule is printed in it ("integer …
	// is out of the range", or "error parsing route url" with the URL).
	failedReloadLine = "Failed to reload server configuration"
)

// routeLine finds the line a server logs for each route connection it makes, accepted or
// solicited, and its peer's address: `<ip>:<port> - rid:<n> - Route connection created`
// (route.go:2104, the connection's prefix at client.go:821, the address from RemoteAddr at
// :785-790).
var routeLine = regexp.MustCompile(`\[INF\] (\S+) - rid:\d+ - Route connection created$`)

// authErrorLine finds the line a server logs when a route connection fails its credential check,
// and the peer's address.
var authErrorLine = regexp.MustCompile(`\[ERR\] (\S+) - rid:\d+ - authentication error`)

// podLoopback are the addresses of a pod's own network namespace: the port-forward of a check's
// INFO read connects from one, and so does any process of the pod, which the check cannot tell
// from the server's own (Check's limit).
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
//   - its log: it begins with the server's start line, so it reaches back to the record; no reload
//     failed, since a failed reload's line can hold the password; and from the reload that loaded
//     authorization on, every route connection names a pod IP, by IP alone, since the port of a
//     connection a peer opened is ephemeral. Lines before that reload are not judged: no
//     connection then carried the password to an address the configuration does not name, and an
//     env roll's pods log the addresses of the pods before them.
//   - its route port's INFO: auth_required, so authorization is in force.
//
// A pod whose routes do not reach all its peers, while its log names failed route logins from pod
// IPs, holds another password than its peers: the mesh is split (docs/nats-hub.md, Route
// authorization).
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
		auth, authKnown := routeAuthRequired(ctx, &r, src, s.pod.Name)
		loaded := checkLog(ctx, &r, src, s.pod.Name, ips, split, auth || !authKnown)
		if authKnown && !auth {
			why := "no reload has loaded authorization since the server started"
			if loaded {
				why = "a reload that changed its cluster block left it without authorization"
			}
			r.add(s.pod.Name, Unknown, "authorization is not in force: its route port's INFO has no auth_required, and %s; apply authorization, then check again", why)
		}
	}
	return r
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

// checkLog judges the pod's nats container log: it begins with the server's start line, no reload
// failed, and from the reload that loaded authorization on, every route connection it names has a
// pod IP; and, when split, it names the failed route logins from pod IPs that split the mesh. With
// no reload that changed the cluster block in the log, the whole log is judged when sinceStart
// says authorization was in force from the server's start (or that is not known), and no route
// connection when it was never in force, since none then carried the password beyond the pods. It
// returns whether the log shows authorization loaded by a reload.
func checkLog(ctx context.Context, r *Report, src Source, pod string, ips map[string]bool, split, sinceStart bool) bool {
	body, err := src.Log(ctx, pod)
	if err != nil {
		r.add(pod, Unknown, "its nats container's log could not be read: %v", err)
		return false
	}
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimRight(line, "\r "); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 || !strings.HasSuffix(lines[0], startLine) {
		r.add(pod, PasswordRead, "its log does not begin with the server's start line, %q, so the lines since the env roll may be gone "+
			"(the API server returns only the newest log file, which the kubelet rotates)", startLine)
	}
	failed := 0
	for _, line := range lines {
		if strings.Contains(line, failedReloadLine) {
			failed++
		}
	}
	if failed > 0 {
		r.add(pod, PasswordRead, "its log holds %d failed reload(s) (%q), and a failed load's error line can hold the route password whole, "+
			"as it does for a password outside the minter's rule", failed, failedReloadLine)
	}

	// The window opens at the hangup that began the first reload which changed the cluster block.
	start, loaded := 0, false
	if !sinceStart {
		start = len(lines)
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, clusterReloadedLine) {
			continue
		}
		loaded = true
		start = i
		for j := i; j >= 0; j-- {
			if strings.HasSuffix(lines[j], hangupLine) {
				start = j
				break
			}
		}
		if !slices.ContainsFunc(lines[i:], func(l string) bool { return strings.Contains(l, reloadedLine) }) {
			r.add(pod, Unknown, "the reload that changed its cluster block has logged no %q", reloadedLine)
		}
		break
	}

	others, logins := map[string]int{}, map[string]int{}
	var order, loginOrder []string
	for _, line := range lines[start:] {
		if match := routeLine.FindStringSubmatch(line); match != nil {
			host, _, err := net.SplitHostPort(match[1])
			if err != nil {
				r.add(pod, Unknown, "its log names a route connection with %q, which is no host and port", match[1])
				continue
			}
			if ips[host] || podLoopback[host] {
				continue
			}
			if others[host] == 0 {
				order = append(order, host)
			}
			others[host]++
		}
		if match := authErrorLine.FindStringSubmatch(line); match != nil {
			if host, _, err := net.SplitHostPort(match[1]); err == nil && ips[host] {
				if logins[host] == 0 {
					loginOrder = append(loginOrder, host)
				}
				logins[host]++
			}
		}
	}
	for _, host := range order {
		r.add(pod, PasswordRead, "its log names %d route connection(s) with %s since it loaded authorization, and %s is no pod of the hub",
			others[host], host, host)
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

// routeAuthRequired reads the first line the pod's route port sends, and returns whether its INFO
// has auth_required, which a server sets exactly when its cluster authorization has a user
// (reload.go:423): whether authorization is in force; and whether that is known.
func routeAuthRequired(ctx context.Context, r *Report, src Source, pod string) (bool, bool) {
	line, err := src.RouteInfo(ctx, pod)
	if err != nil {
		r.add(pod, Unknown, "its route port's INFO could not be read: %v", err)
		return false, false
	}
	text, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "INFO ")
	var info struct {
		AuthRequired bool `json:"auth_required"`
	}
	if !ok || json.Unmarshal([]byte(text), &info) != nil {
		r.add(pod, Unknown, "its route port's first line is no INFO")
		return false, false
	}
	return info.AuthRequired, true
}
