package writerhold

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// connzLimit is the /connz page the report asks for: nats-server's own default and its largest
// page (DefaultConnListSize, 1,024).
const connzLimit = 1024

// connzTries bounds the readings of one server's /connz until one holds every connection.
const connzTries = 3

// reportRounds bounds the readings of the whole census again while a new connection is unresolved.
const reportRounds = 3

// holdAware ends the name of the auth service's own connection in a build that refuses every client
// but the operator during a hold (`envoy-auth <version> hold-aware`); an older replica's does not.
const holdAware = " hold-aware"

// Report is step 6, the report: only the operator remains. Over the declared servers it reads, at
// each reading, every server's /varz, /routez, /leafz, /gatewayz and
// /connz?auth=1&state=open&limit=1024&offset=0, and the hold holds when all of these are true:
//
//  1. every server's id differs from its id at step 1, and step 1's route, leafnode and gateway
//     checks hold again;
//  2. every connection of every server is the caller's own (its server id and client id, read
//     from the caller's connection after its last reconnect), or the auth service's: authorized as
//     AuthUser, named `… hold-aware`, in $G, which /connz shows as an empty account (nats-server
//     monitor.go:471-473), so an empty account is read as $G and never compared with "$G";
//  3. the caller's own connection is in the /connz of its server, with its user: a reading that
//     cannot see it is UNKNOWN, not clean.
//
// Any other connection is unresolved at first: a connection is listed before its CONNECT is
// authorized, showing its CONNECT user. The whole census is read again the declared servers'
// auth_timeout plus 1 s later. A connection blocks the hold, named by its server, cid, name and
// user, only when the same (server id, cid) is in two readings; new unresolved connections take
// another round, and after reportRounds rounds the step is UNKNOWN. The report only reads, so
// nothing mutates while a connection is unresolved.
type Report struct {
	Hub      KubeHub
	Declared Declared
	// AuthUser is the auth service's static user, as /connz?auth=1 shows its authorized_user.
	AuthUser string
	// Wait waits d before a reading again, ending early with ctx's error; a sleep when nil.
	Wait func(ctx context.Context, d time.Duration) error

	// The seams the report's rows flip, each a control that shows a check matters.
	predicate predicate
	// open opens a pod's monitoring port, Hub.Monitor when nil (a row's reading of the wrong
	// server).
	open opener
	// noAuth reads /connz without auth=1, which leaves every authorized_user empty.
	noAuth bool
	// onePage counts /connz's first page as the whole list (TestHoldReportPagesPastOneServersLimit's
	// control).
	onePage bool
	// caller replaces the caller's ids read at each reading (TestHoldReportTheCallerAfterItsReconnect's
	// control, the ids from before its restart).
	caller func() (ConnRef, error)
}

// predicate holds the controls of the connection predicate, each a weaker rule a row shows is
// unsafe or unsound; the zero predicate is the hold's.
type predicate struct {
	// atOnce stops the hold on every unresolved connection of the first reading
	// (TestHoldReportAFiniteTransient's control).
	atOnce bool
	// atOnceOnUser stops the hold at the first reading on an unresolved connection with a user
	// (TestHoldReportAPausedOperatorConnect's control).
	atOnceOnUser bool
	// accountByName reads a connection in $G by comparing its account with "$G"
	// (TestHoldReportOnlyTheCallerAndTheAuthService's control).
	accountByName bool
	// byCallerUser resolves every connection of the caller's user, not the caller's own (server
	// id, cid) alone (TestHoldReportASecondOperator's control).
	byCallerUser bool
}

// ConnRef names one connection: its server's id and its client id on that server.
type ConnRef struct {
	ServerID string
	CID      uint64
}

// ReportReading is the report's clean reading: the caller, and every declared server with its
// connections, each the caller's own or the auth service's.
type ReportReading struct {
	Caller ConnRef
	// Readings is how many readings of the whole census the report made.
	Readings int
	Servers  []ServerConnections
}

// ServerConnections is one declared server as the report read it.
type ServerConnections struct {
	Pod         string
	Varz        Varz
	Connections []ConnInfo
}

// listedConn is one connection a reading listed, with its server.
type listedConn struct {
	pod  string
	varz Varz
	conn ConnInfo
}

func (l listedConn) ref() ConnRef { return ConnRef{ServerID: l.varz.ServerID, CID: l.conn.CID} }

func (l listedConn) String() string {
	return fmt.Sprintf("%s cid %d name %q user %q account %q", describeServer(l.pod, l.varz), l.conn.CID, l.conn.Name, l.conn.AuthorizedUser, l.conn.Account)
}

// Run is the report, on caller, the operator's connection the predicate allows.
func (r Report) Run(ctx context.Context, caller *nats.Conn) (*ReportReading, error) {
	switch {
	case len(r.Declared) == 0:
		return nil, unknownf(StepReport, nil, "the report names no declared server")
	case r.AuthUser == "":
		return nil, unknownf(StepReport, nil, "the report names no auth service user, so no connection of the auth service can be read as its")
	case caller == nil && r.caller == nil:
		return nil, unknownf(StepReport, nil, "the report has no caller connection")
	}
	wait := r.Wait
	if wait == nil {
		wait = sleep
	}
	again := r.Declared.AuthTimeout() + time.Second
	reading, unresolved, err := r.read(ctx, caller)
	if err != nil {
		return nil, err
	}
	reading.Readings = 1
	if blocked := r.predicate.atFirstReading(unresolved); len(blocked) > 0 {
		return nil, refusef(StepReport, "a connection other than the caller's and the auth service's is listed: %s", names(blocked))
	}
	seen := map[ConnRef]bool{}
	for round := 1; len(unresolved) > 0; round++ {
		if round > reportRounds {
			return nil, unknownf(StepReport, nil, "new unresolved connections were listed at each of %d readings again, the last %s: a client that keeps retrying a refused connect is still connecting",
				reportRounds, names(unresolved))
		}
		for _, conn := range unresolved {
			seen[conn.ref()] = true
		}
		if err := wait(ctx, again); err != nil {
			return nil, unknownf(StepReport, err, "the wait before reading the census again")
		}
		reading, unresolved, err = r.read(ctx, caller)
		if err != nil {
			return nil, err
		}
		reading.Readings = round + 1
		var blocked []listedConn
		for _, conn := range unresolved {
			if seen[conn.ref()] {
				blocked = append(blocked, conn)
			}
		}
		if len(blocked) > 0 {
			return nil, refusef(StepReport, "a connection other than the caller's and the auth service's stayed listed across two readings: %s", names(blocked))
		}
	}
	return reading, nil
}

// atFirstReading is what the predicate's controls stop on at the first reading; the hold's own
// predicate stops on nothing there.
func (p predicate) atFirstReading(unresolved []listedConn) []listedConn {
	var blocked []listedConn
	for _, conn := range unresolved {
		if p.atOnce || (p.atOnceOnUser && conn.conn.AuthorizedUser != "") {
			blocked = append(blocked, conn)
		}
	}
	return blocked
}

// resolved reports whether conn, listed on the server whose id is serverID, is the caller's own or
// the auth service's.
func (p predicate) resolved(conn ConnInfo, serverID string, caller ConnRef, callerUser, authUser string) bool {
	if serverID == caller.ServerID && conn.CID == caller.CID {
		return true
	}
	if p.byCallerUser && callerUser != "" && conn.AuthorizedUser == callerUser {
		return true
	}
	inG := conn.Account == ""
	if p.accountByName {
		inG = conn.Account == "$G"
	}
	return conn.AuthorizedUser == authUser && strings.HasSuffix(conn.Name, holdAware) && inG
}

// names lists conns, each by its server, cid, name, user and account, in a stable order.
func names(conns []listedConn) string {
	listed := make([]string, len(conns))
	for i, conn := range conns {
		listed[i] = conn.String()
	}
	slices.Sort(listed)
	return strings.Join(listed, "; ")
}

// callerOf is the caller's own connection now: its server's id and its client id there.
func callerOf(nc *nats.Conn) (ConnRef, error) {
	server := nc.ConnectedServerId()
	cid, err := nc.GetClientID()
	switch {
	case err != nil:
		return ConnRef{}, err
	case server == "":
		return ConnRef{}, fmt.Errorf("the caller is not connected (%s)", nc.Status())
	}
	return ConnRef{ServerID: server, CID: cid}, nil
}

// read is one reading of the whole census: point 1's checks, then every server's connections. It
// returns the reading and its unresolved connections. A reading that cannot see the caller, or
// during which the caller reconnected, is UNKNOWN.
func (r Report) read(ctx context.Context, nc *nats.Conn) (*ReportReading, []listedConn, error) {
	callerIDs := r.caller
	if callerIDs == nil {
		callerIDs = func() (ConnRef, error) { return callerOf(nc) }
	}
	caller, err := callerIDs()
	if err != nil {
		return nil, nil, unknownf(StepReport, err, "the caller's own connection")
	}
	open := r.open
	if open == nil {
		open = r.Hub.Monitor
	}
	reading, err := readServers(ctx, StepReport, open, r.Declared.pods())
	if err != nil {
		return nil, nil, err
	}
	defer reading.close()
	before := make(map[string]string, len(r.Declared))
	for _, server := range r.Declared {
		before[server.Varz.ServerID] = server.Pod
		if now := reading.varz[server.Pod]; now.ServerID == server.Varz.ServerID {
			return nil, nil, refusef(StepReport, "%s still has its step-1 id: it has not restarted since the hold began", describeServer(server.Pod, now))
		}
	}
	if err := reading.checkTopology(ctx, StepReport, r.Declared.names(), before); err != nil {
		return nil, nil, err
	}
	result := &ReportReading{Caller: caller}
	var callerUser string
	callerSeen := false
	for _, pod := range reading.pods {
		varz := reading.varz[pod]
		conns, err := r.connz(ctx, reading.monitors[pod], pod, varz)
		if err != nil {
			return nil, nil, err
		}
		result.Servers = append(result.Servers, ServerConnections{Pod: pod, Varz: varz, Connections: conns})
		for _, conn := range conns {
			if varz.ServerID == caller.ServerID && conn.CID == caller.CID {
				if conn.AuthorizedUser == "" {
					return nil, nil, unknownf(StepReport, nil, "the caller's own connection, %s cid %d, shows no authorized_user: the reading was made without auth=1, and shows no connection's user",
						describeServer(pod, varz), conn.CID)
				}
				callerSeen, callerUser = true, conn.AuthorizedUser
			}
		}
	}
	if !callerSeen {
		return nil, nil, unknownf(StepReport, nil, "the reading does not see the caller's own connection, server %s cid %d", caller.ServerID, caller.CID)
	}
	if after, err := callerIDs(); err != nil || after != caller {
		return nil, nil, unknownf(StepReport, err, "the caller's connection moved during the reading, from server %s cid %d to server %s cid %d",
			caller.ServerID, caller.CID, after.ServerID, after.CID)
	}
	var unresolved []listedConn
	for _, server := range result.Servers {
		for _, conn := range server.Connections {
			if !r.predicate.resolved(conn, server.Varz.ServerID, caller, callerUser, r.AuthUser) {
				unresolved = append(unresolved, listedConn{pod: server.Pod, varz: server.Varz, conn: conn})
			}
		}
	}
	return result, unresolved, nil
}

// connz is every open connection of pod's server. An answer counts only when its total is at most
// its limit, so it lists every connection at once. Otherwise the report pages by offset, and a
// paging counts only when every page answers the same total and the pages hold that many distinct
// connections, and the paging read again holds the same ones: a connection that closes between two
// pages moves a later one onto a page already read. After connzTries readings with no such answer,
// it is UNKNOWN. An answer from another server than the reading's is UNKNOWN at once.
func (r Report) connz(ctx context.Context, monitor Monitor, pod string, varz Varz) ([]ConnInfo, error) {
	var previous []ConnInfo
	var problem error
	for range connzTries {
		conns, whole, err := r.paging(ctx, monitor, pod, varz)
		switch {
		case err != nil:
			return nil, err
		case whole:
			return conns, nil
		case conns == nil:
			previous = nil
			problem = fmt.Errorf("the pages did not hold one list")
		case previous != nil && sameConnections(previous, conns):
			return conns, nil
		default:
			previous = conns
			problem = fmt.Errorf("%d connections, past one page of %d, read twice", len(conns), connzLimit)
		}
	}
	return nil, unknownf(StepReport, problem, "%s's /connz held no one list of every connection in %d readings", describeServer(pod, varz), connzTries)
}

// paging is one reading of pod's /connz: whole when its first answer lists every connection, else
// every page's connections, or nil when the pages do not hold one list.
func (r Report) paging(ctx context.Context, monitor Monitor, pod string, varz Varz) ([]ConnInfo, bool, error) {
	var all []ConnInfo
	seen := map[uint64]bool{}
	total := -1
	for {
		offset := len(all)
		query := "/connz?auth=1&state=open&limit=%d&offset=%d"
		if r.noAuth {
			query = "/connz?state=open&limit=%d&offset=%d"
		}
		var page Connz
		if err := monitor.Get(ctx, fmt.Sprintf(query, connzLimit, offset), &page); err != nil {
			return nil, false, unknownf(StepReport, err, "%s's /connz at offset %d", describeServer(pod, varz), offset)
		}
		if page.ServerID != varz.ServerID {
			return nil, false, unknownf(StepReport, nil, "%s's /connz answered as server %s", describeServer(pod, varz), page.ServerID)
		}
		if offset == 0 && page.Total <= page.Limit && len(page.Connections) == page.Total {
			return page.Connections, true, nil
		}
		if offset == 0 && r.onePage {
			return page.Connections, true, nil
		}
		if total >= 0 && page.Total != total || len(page.Connections) == 0 {
			return nil, false, nil
		}
		total = page.Total
		for _, conn := range page.Connections {
			if seen[conn.CID] {
				return nil, false, nil
			}
			seen[conn.CID] = true
			all = append(all, conn)
		}
		if len(all) >= total {
			if len(all) > total {
				return nil, false, nil
			}
			return all, false, nil
		}
	}
}

// sameConnections reports whether a and b list the same connections, by client id.
func sameConnections(a, b []ConnInfo) bool {
	if len(a) != len(b) {
		return false
	}
	ids := func(conns []ConnInfo) []uint64 {
		out := make([]uint64, len(conns))
		for i, conn := range conns {
			out[i] = conn.CID
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(ids(a), ids(b))
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
