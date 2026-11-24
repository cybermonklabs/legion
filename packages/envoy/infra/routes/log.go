package routes

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The messages of a nats-server (v2.15.1) log the check reads. A rule reads only a line in the
// logger's form that the server wrote (serversLines), at one level, and matches its message: whole
// for the start line, the hangup, "Reloaded: cluster routes" and "Reloaded: cluster"; whole or by
// the prefix that comes before a digest for "Reloaded server configuration"; and by prefix for a
// failed reload, by pattern for a route connection's lines, and by substring for a credentialed
// URL, each in the server's own
// lines, or a connection's lines with their prefix, alone. Never does a rule match the end of a
// line or text anywhere in it: a connection's lines carry text its peer chose, a client's -ERR
// whole at the end of "Client Error" (client.go:2268), so such a rule could be met or tripped by
// any client.
const (
	// startMessage is the first line of a log: Start logs it before anything else (server.go:2295).
	startMessage = "Starting nats-server"
	// hangupMessage is the line nats-server logs when it traps the hangup the chart's config
	// reloader sends it to reload (signal.go:58, `Trapped %q signal`; the reloader signals SIGHUP
	// unless told otherwise, nack v0.24.0 cmd/nats-server-config-reloader/main.go:67, and the chart
	// tells it nothing). It logs it before Reload reads the configuration (signal.go:80-82), so
	// before the reload publishes the new options (reload.go:1561), which a route the hub dials then
	// uses at once (route.go:1064, :1092-1093), and before it logs "Reloaded: cluster" (reload.go:470).
	hangupMessage = `Trapped "hangup" signal`
	// routesReloadedMessage is the line a reload logs when it changed the routes alone
	// (reload.go:553): the routes stage's reload, before the one that loads authorization.
	routesReloadedMessage = "Reloaded: cluster routes"
	// clusterReloadedMessage is the line a reload logs when it applies a change to the cluster
	// block's options, the authorization among them (reload.go:470).
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

// A line's logger time against the time the runtime read it. The runtime and the server read one
// host clock, and the server timestamps a line before it writes it, so the runtime reads a line it
// wrote no earlier than clockSlack (the two clocks' rounding) before its logger time; and on a node
// under load it reads the line late, but no later than readLag after it. A line outside that span
// is forged. The late side is a bound for a loaded node, not a side to drop: it refuses a line a
// peer writes through a route's name or an auth error, in the server's form, with the server's
// process id and a timestamp well in the past. Read as the server's, such a line could add
// findings or open authorizationWindow's window earlier, and it would clear the two Unknowns that
// function names when the forger can place it at their moments.
const (
	clockSlack = time.Second
	readLag    = 30 * time.Second
)

// parseLog splits a log into its lines, without the empty ones and each without the carriage
// return and spaces it ends with.
func parseLog(body []byte) []logLine {
	var lines []logLine
	for raw := range strings.SplitSeq(string(body), "\n") {
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

// agrees reports whether the runtime read l no earlier than clockSlack before its logger time and
// no later than readLag after it.
func (l logLine) agrees() bool {
	lag := l.runtime.Sub(l.at)
	return l.form && lag >= -clockSlack && lag <= readLag
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
// to the second, cut down, and nats-server logs its start line as it starts, so the runtime reads
// the line no earlier than startSlack before that time and, on a node under load, no later than
// startDelay after it. The rule stays two-sided, against the container's start rather than the
// line's own time: it is what keeps a start line a peer forges through a route's name, at the top
// of a log the kubelet rotated, from passing as the server's (serversLines).
const (
	startSlack = time.Second
	startDelay = 30 * time.Second
)

// podLoopback are the addresses of a pod's own network namespace: the port-forward of a check's
// route port reads connects from one, and so does any process of the pod, which the check cannot
// tell from the server's own (Check's limit).
var podLoopback = map[string]bool{"127.0.0.1": true, "::1": true}

// checkLog judges the pod's nats container log, read with the container runtime's times: it keeps
// the lines its server wrote (serversLines), judges the server's lines of itself (ownLines), opens
// the window of the reload that loaded authorization (authorizationWindow) and judges each route
// connection in it (routeConnections). When auth says authorization is not in force, it says so,
// with what the log shows of why, since only it knows whether it read the log and judged it whole:
// that no reload has loaded authorization since the server started, it says only of a log that
// begins at the server's start line. It returns the index in r.Findings of its start line
// finding, or -1 when it made none.
func checkLog(ctx context.Context, r *Report, src Source, s server, ips map[string]bool, split bool, auth authState) int {
	pod := s.pod.Name
	startFinding, why := -1, ""
	if body, err := src.Log(ctx, pod); err != nil {
		r.add(pod, Unknown, "its nats container's log could not be read: %v", err)
	} else {
		lines, start, whole := serversLines(r, s, parseLog(body))
		startFinding = start
		ownLines(r, pod, lines)
		window, loaded := authorizationWindow(r, s, lines, auth)
		routeConnections(r, pod, window, ips, split)
		switch {
		case loaded:
			why = ", and a reload that changed its cluster block left it without authorization"
		case whole:
			why = ", and no reload has loaded authorization since the server started"
		}
	}
	if auth == authOff {
		r.add(pod, Unknown, "authorization is not in force: its route port's INFO has no auth_required%s; apply authorization, then check again", why)
	}
	return startFinding
}

// serversLines returns, in order, the lines of a log that its server wrote: lines in the logger's
// form, each read by the runtime no earlier than clockSlack before its logger time and no later
// than readLag after it, and carrying the start line's process id. The start line is the server's
// when the runtime read it at the container's start, since a line a peer forges carries the time
// the runtime read the line it was written in; a log that does not begin with it may have lost its
// lines since the env roll, which is PasswordRead, and then it returns every line whose times
// agree, whatever its process id, since no start line names the server's.
// A log with lines that carry no time of the runtime's is judged by none of this, since which of
// its lines the server wrote cannot be told: it is Unknown, and serversLines returns no line.
// Every rule after this one reads only the lines it returns. It returns, too, the index in
// r.Findings of its start line finding, or -1, and whether the log was judged and begins at the
// server's start line, so its lines reach back to the server's start.
func serversLines(r *Report, s server, lines []logLine) ([]logLine, int, bool) {
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
		return nil, -1, false
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
			"not the start line's, or the container runtime read them more than %s before or more than %s after their logger time. A route "+
			"peer writes such lines through its server name, which nats-server prints unquoted when the route closes (client.go:2125), and "+
			"the auth service through its error text (auth_callout.go:460); the check reads none of them, and cannot clear the log",
			forged, clockSlack, readLag)
	}
	return own, startFinding, startPID != ""
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

// authorizationWindow returns the server's lines the route connection rule judges, and whether the
// log shows the reload that loaded authorization, by its first "Reloaded: cluster" line
// (reload.go:470). The window opens at the hangup that began that reload (hangupMessage): the
// reload publishes the new options between the two (reload.go:1561), at a time no line shows, and a
// route the hub dials from then on carries the password (route.go:1064, :1092-1093), so every line
// from the hangup on is judged, however long the reload took to log its marker. That hangup is the
// first one after the routes stage's reload, "Reloaded: cluster routes" (reload.go:553), which
// itself began with a hangup: a peer can write any of these lines (serversLines), and taking the
// first of each moves the window no later than the real hangup, only earlier. With no hangup
// between the routes stage's reload and the marker, as when authorization was loaded otherwise
// (events.go:3219-3227 reloads on a system request with no hangup) or the routes stage's reload is
// not in the log, no line can be placed after the password came into use: Unknown, and no window.
// With no marker among the server's lines, the window is empty: when auth says authorization is not
// in force, since no connection then carried the password beyond the pods; and otherwise as well,
// but Unknown. A container the record vouches for had no authorization when it was recorded (Take),
// so it loaded authorization by a reload, which logs that line; with the line lost, or the reload
// not yet on the pod, no line can be placed after it, and the env roll's lines name the old IPs of
// the pods it rolled. The reload must have finished, and the runtime must have read its marker more
// than startDelay after the container's start, or a start line a process wrote in that span through
// a route's name could pass the start line rule; each is Unknown otherwise. Forged lines clear two
// Unknowns here, since the check cannot tell them from the server's: a peer that writes "Reloaded
// server configuration" through a route's name after the marker silences the finding that the
// reload logged no end, and one that writes the hangup line between the routes stage's reload and
// the marker silences the finding that the log holds no hangup, and opens the window at its own
// line. Neither finding is the verdict on authorization: the route port's INFO read (authOff,
// checkLog) stays it.
func authorizationWindow(r *Report, s server, lines []logLine, auth authState) ([]logLine, bool) {
	pod := s.pod.Name
	at := slices.IndexFunc(lines, func(l logLine) bool { return l.is("INF", clusterReloadedMessage) })
	if at < 0 {
		if auth != authOff && len(lines) > 0 {
			r.add(pod, Unknown, "its log, in the lines its server wrote, holds no %q, the line of the reload that loaded authorization, though "+
				"its route port's INFO does not show authorization off; the record's container loaded authorization by a reload, so that line "+
				"was lost or the reload has not reached the pod, and no route connection is judged as after authorization", "[INF] "+clusterReloadedMessage)
		}
		return nil, false
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
	hangup := -1
	if routes := slices.IndexFunc(lines[:at], func(l logLine) bool { return l.is("INF", routesReloadedMessage) }); routes >= 0 {
		if i := slices.IndexFunc(lines[routes:at], func(l logLine) bool { return l.is("INF", hangupMessage) }); i >= 0 {
			hangup = routes + i
		}
	}
	if hangup < 0 {
		r.add(pod, Unknown, "its log, in the lines its server wrote, holds no %q after the routes stage's %q and before %q: the reload that loaded "+
			"authorization publishes the new options after its hangup and before that line, at a time no line shows (reload.go:1561), so no "+
			"route connection can be placed after the password came into use", "[INF] "+hangupMessage, "[INF] "+routesReloadedMessage,
			"[INF] "+clusterReloadedMessage)
		return nil, true
	}
	return lines[hangup:], true
}

// routeConnection counts one address's route connections in the window by what the log shows of
// each.
type routeConnection struct {
	// formed: its close line names a server.
	formed int
	// refused: its login the route credential refused, with no close line that names a server.
	refused int
	// unnamed: its close line names no server.
	unnamed int
	// open: none of those, so still open when the log was read.
	open int
}

// routeConnections judges each route connection the window names with an address that is neither
// a pod IP nor the pod's loopback. One that formed a route by the log, its close line naming a
// server, is PasswordRead: it came after the hangup that began the reload that loaded
// authorization, and so may have come after the reload published the password. Every other one is
// Unknown: one whose login the credential refused, or whose close line names no server, may have
// formed no route, though on one the hub opened it sent the password before the peer named itself
// (route.go:2087-2094), and a peer can name itself with an empty name, which the close line does
// not show (client.go:2124); and one with neither line is
// still open, and may be an accepted connection waiting for its login, which nats-server logs on
// accept (route.go:2104) and closes at the cluster's auth timeout (route.go:2070-2073), so any
// process that reaches the route port can hold one open while the log is read. When split, it
// names the failed route logins from pod IPs that split the mesh. A host is quoted, since a line
// can carry the address another process wrote.
func routeConnections(r *Report, pod string, window []logLine, ips map[string]bool, split bool) {
	refused, named, unnamed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	logins := map[string]int{}
	var loginOrder []string
	for _, l := range window {
		switch l.level {
		case "ERR":
			match := routeAuthError.FindStringSubmatch(l.message)
			if match == nil {
				continue
			}
			refused[match[2]] = true
			if host, _, err := net.SplitHostPort(match[1]); err == nil && ips[host] {
				if logins[host] == 0 {
					loginOrder = append(loginOrder, host)
				}
				logins[host]++
			}
		case "INF":
			match := routeClosed.FindStringSubmatch(l.message)
			if match == nil {
				continue
			}
			if strings.Contains(match[2], remoteTag) {
				named[match[1]] = true
			} else {
				unnamed[match[1]] = true
			}
		}
	}
	counts := map[string]*routeConnection{}
	var order []string
	for _, l := range window {
		if l.level != "INF" {
			continue
		}
		match := routeCreated.FindStringSubmatch(l.message)
		if match == nil {
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
