// Package routes is `envoy-infra routes`, the route check of a clustered hub's
// routeAuthorization (docs/nats-hub.md, Route authorization). Take, which `routes record` runs
// once the env stage's roll has finished on every pod, saves each pod's identity and its nats
// container's start. Check, which `routes check` runs once authorization is on every pod, then
// asks whether authorization is in force, and whether any process other than the hub's own
// servers could have read the route password since it was. Both read the hub through a Source;
// OpenKubectl's, the one the command uses (kubectl.go), reads the API server's view of the
// StatefulSet's pods, each pod's monitoring port and route port through a port-forward of its
// own, and its nats container's log with the container runtime's times (kubectl logs
// --timestamps).
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
// and its Ready condition; whether it is on the update revision of a finished roll of the
// StatefulSet, its controller-revision-hash the update revision once the StatefulSet's status shows
// every replica updated and the current revision the update revision; whether its nats container
// names the route variables routeAuthorization's env stage gives it; and its nats container's
// restart count and the time the running container started.
type Pod struct {
	Name         string
	UID          string
	IP           string
	Ready        bool
	Updated      bool
	RouteEnv     bool
	RestartCount int32
	StartedAt    time.Time
}

// Source reads the hub's pods. Cluster names the kubeconfig context the source reads through and
// that context's cluster, by its API server; Pods lists every pod the StatefulSet has, by its
// ordinal names; Monitor answers a GET of path on a pod's monitoring port; Log is the whole log of
// a pod's nats container as the API server returns it with the container runtime's times
// (`kubectl logs --timestamps`), the current container's newest file, each line after the RFC 3339
// time the runtime read it at and a space; RouteExchange opens a connection to a pod's route port
// and returns the first line it sends, its INFO, and, when send is not empty, writes send and
// returns the first line after the INFO that answers it (Exchange). Close ends whatever the source
// holds open.
type Source interface {
	Cluster(ctx context.Context) (Cluster, error)
	Pods(ctx context.Context) ([]Pod, error)
	Monitor(ctx context.Context, pod, path string) ([]byte, error)
	Log(ctx context.Context, pod string) ([]byte, error)
	RouteExchange(ctx context.Context, pod string, send []byte) (info, answer []byte, err error)
	Close() error
}

// Cluster is the kubeconfig context a source reads through, the API server of its cluster, and the
// SHA-256, in hex, of each CA certificate the client holds for that cluster, sorted, none when it
// holds none. The record is taken through the deploying person's context, and the check runs as the
// operator's Job in a pod, whose context is in-cluster and whose server is the `kubernetes`
// Service's address, which clusters of a kind can share; so a cluster is told by its CA
// certificates (sameCluster).
type Cluster struct {
	Context string   `json:"context"`
	Server  string   `json:"server"`
	CA      []string `json:"caCertificates"`
}

// sameCluster reports whether a and b are one cluster: when both hold a CA certificate, whether
// they share one, since a bundle in rotation holds the old and the new and two clusters share none;
// and when either holds none, whether they name one context and one server.
func sameCluster(a, b Cluster) bool {
	if len(a.CA) > 0 && len(b.CA) > 0 {
		return slices.ContainsFunc(a.CA, func(c string) bool { return slices.Contains(b.CA, c) })
	}
	return a.Context == b.Context && a.Server == b.Server
}

// Record is what `routes record` saves: the hub it read, through which context of which cluster,
// when, and each pod's identity and its nats container's start at that time. A hub of the same
// namespace and StatefulSet on another cluster has other pods, which `routes check` would read as a
// new container on every pod, so it refuses a record of another cluster (sameCluster).
type Record struct {
	Namespace   string      `json:"namespace"`
	StatefulSet string      `json:"statefulSet"`
	Cluster     Cluster     `json:"cluster"`
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

// Take reads src's pods and returns their record, timed now. It refuses until the env stage's roll
// has finished: while any pod is not Ready or its nats container is not running, is not on the
// update revision of a finished roll of the StatefulSet, or names no route variables in its nats
// container, since a pod the roll replaces after the record has a log the record does not cover.
// Between two pods of a roll every pod can be Ready while one still runs the revision before. It
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
	var notReady, notUpdated, noRouteEnv []string
	rec := Record{Namespace: namespace, StatefulSet: statefulSet, RecordedAt: now.UTC()}
	for _, p := range pods {
		if !p.Ready || p.UID == "" || p.StartedAt.IsZero() {
			notReady = append(notReady, p.Name)
			continue
		}
		if !p.Updated {
			notUpdated = append(notUpdated, p.Name)
		}
		if !p.RouteEnv {
			noRouteEnv = append(noRouteEnv, p.Name)
		}
		rec.Pods = append(rec.Pods, PodRecord{Name: p.Name, UID: p.UID, RestartCount: p.RestartCount, StartedAt: p.StartedAt.UTC()})
	}
	if len(notReady) > 0 {
		return Record{}, fmt.Errorf("%s not Ready with a running nats container; record once the env stage's roll has finished on every pod",
			strings.Join(notReady, ", "))
	}
	if len(notUpdated) > 0 {
		return Record{}, fmt.Errorf("%s not on the update revision of a finished roll of the StatefulSet; record once the env stage's roll has "+
			"finished on every pod", strings.Join(notUpdated, ", "))
	}
	if len(noRouteEnv) > 0 {
		return Record{}, fmt.Errorf("%s has no route variables in its nats container: apply routeAuthorization's env stage, then record once its "+
			"roll has finished on every pod", strings.Join(noRouteEnv, ", "))
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
//   - its /routez: every route's ip a pod IP and its remote_id the server id of another pod, from
//     that pod's own IP, and a route to each of its peers. A route's remote_id is the id its peer
//     sends (route.go:820), which every route port's INFO shows of its server, so it counts only
//     from the IP of the pod whose server answers it. It never trusts a route's remote_name, which
//     the peer chooses: nats-server refuses an accepted route's name only when it equals the
//     accepting server's own (route.go:745-751).
//   - its route port: its INFO has auth_required, and it refuses a route login with no user and
//     one with the route user and no password, so authorization is in force with a password.
//   - its log, by the lines the server wrote (serversLines: in the logger's form, with the start
//     line's process id and a time that agrees with the runtime's): it begins with the server's
//     start line, which the runtime read at the container's start, so it reaches back to the
//     record; no reload failed, and no line the server wrote of itself holds a URL with a user and
//     password, since either can hold the password; and in the window that opens at the hangup
//     that began the reload that loaded authorization (authorizationWindow), which the server
//     logs before it publishes the new options, every route connection names a pod IP, by IP
//     alone, since the port of a connection a peer opened is ephemeral (routeConnections). Lines
//     before the window are not judged: no connection then carried the password to an address the
//     configuration does not name, and an env roll's pods log the addresses of the pods before
//     them. A log with no such hangup after the routes stage's reload is Unknown. A connection
//     from another address is PasswordRead when it formed a route by the log, its close line
//     naming a server; one whose login the credential refused, or whose close line names no
//     server, may have formed no route, though one the hub dialed carried the password
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
// reloader or its exporter, has that pod's IP, so a route it holds from that IP reads in /routez as
// the pod's, and remote_id tells it apart only while it holds the route under an id other than its
// own pod's server's, which it can send as its own; and it has the pod's loopback, from which the
// log rule passes a route connection, since the check's own port-forward connects from it.
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
	ids := map[string]server{}
	for _, s := range servers {
		ips[s.pod.IP] = true
		if s.id == "" {
			continue
		}
		if other, taken := ids[s.id]; taken {
			r.add(s.pod.Name, Unknown, "answered the server id %s that %s answered too", s.id, other.pod.Name)
			idsKnown = false
		}
		ids[s.id] = s
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
// id, its remote_id another pod's, from that pod's own IP, and a route to each of s's peers. It
// returns whether s's routes are known not to reach all its peers.
func checkRoutes(ctx context.Context, r *Report, src Source, s server, ips map[string]bool, ids map[string]server, peerCount int) bool {
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
		// quoted: a peer can put a newline or a terminal escape in either. A route's ip is the
		// TCP peer's address, which the peer does not choose, so a remote_id counts only from the
		// IP of the pod whose server answers it: every route port's INFO shows its server's id,
		// which a process at another pod's address could send as its own.
		if !ips[route.IP] {
			r.add(s.pod.Name, PasswordRead, "has a route from an address that is no pod of the hub: ip %s, remote_id %q, remote_name %q",
				route.IP, route.RemoteID, route.RemoteName)
			continue
		}
		if ids == nil {
			continue
		}
		owner, hub := ids[route.RemoteID]
		if !hub || owner.pod.Name == s.pod.Name {
			r.add(s.pod.Name, PasswordRead, "has a route to a server that is none of the hub's others: ip %s, remote_id %q, remote_name %q",
				route.IP, route.RemoteID, route.RemoteName)
			continue
		}
		if route.IP != owner.pod.IP {
			r.add(s.pod.Name, PasswordRead, "has a route from another pod's address than the server it names: ip %s, remote_id %q of %s, whose pod IP is %s, "+
				"remote_name %q", route.IP, route.RemoteID, owner.pod.Name, owner.pod.IP, route.RemoteName)
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
