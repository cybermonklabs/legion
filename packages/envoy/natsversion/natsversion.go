// Package natsversion holds the nats-server versions Envoy and the Legion daemon refuse to run
// against, for a caller in either module: the Envoy listener asks it at start, before it opens its
// session consumers (internal/bus ConnectSessionConsumers), and the daemon before it opens its
// durable consumers (packages/daemon/internal/intake OpenConsumers). It is one rule in one place,
// so the two cannot require different servers.
//
// The requirement is a server whose scan of a consumer's stored messages reads every filter. A
// session consumer filters on a session's agent subject in both forms of its name,
// notifications.agent.<session> beside notifications.agent.*.<session> (dispatch://INFRA-3862),
// which puts a `*` and a literal at the same token of the server's filter tree. From v2.10.26, and
// in 2.11.0 to 2.11.11 and 2.12.0 to 2.12.3, nats-server reads a file block by subject through an
// intersection that can miss a literal filter beside a `*`: at v2.10.29 and v2.11.0
// intersectStree (server/sublist.go 1750-1784 at v2.10.29) follows only the `*` branch at such a
// level, and at v2.11.11 and v2.12.3 gsl.intersectStree visits a terminal literal but skips one
// that has descendants. With other subjects' messages between a consumer's position and a message
// on the literal, the consumer is moved past that message (consumer.go getNextMsg, 3700-3708 at
// v2.10.29) and never delivers it, with no error. v2.11.12, v2.12.4 and every release from v2.14.0
// read the block through stree.IntersectGSL, which checks every filter; their first candidates did
// not (v2.11.12-RC.1 to RC.3 and v2.12.4-RC.1 and RC.2 still call gsl.IntersectStree), so a
// prerelease counts as before its release. There is no v2.13.
//
// The floor requires one of those fixed releases, so every 2.10 is refused: the last 2.10, v2.10.29,
// has the bug, and no 2.10 release has the fix, although the releases before v2.10.26 do not have
// the bug. A connection is read for its server's version only once it reports one
// (ConnectedRefusal), so a reconnect or a drain is waited out rather than read as an unknown server.
//
// Like subjectforms and sessiontiming, it is a door for the daemon's module, which Go's internal
// rule keeps out of the envoy module's internal packages.
package natsversion

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// FixedReleases names the first releases of each line whose scan reads every filter, which every
// refusal names.
const FixedReleases = "v2.11.12, v2.12.4, or v2.14.0 and later"

// Refusal answers nil for a nats-server version, as a connection's server reports it
// ((*nats.Conn).ConnectedServerVersion(): "2.15.1", with or without a leading v), whose scan of a
// consumer's stored messages reads every filter, and a *RefusedRelease naming the version and the
// fixed releases for any other: every 2.10, 2.11.0 to 2.11.11, 2.12.0 to 2.12.3, every release
// before them, a prerelease of a line's first fixed release (2.12.4-RC.1), which comes before that
// release, and a version it cannot read, an empty one among them, since a server it cannot place is
// not known to be fixed.
func Refusal(serverVersion string) error {
	major, minor, patch, prerelease, ok := parse(serverVersion)
	if !ok {
		return &RefusedRelease{Release: serverVersion, text: fmt.Sprintf("the NATS server reports version %q, which cannot be read; Envoy needs nats-server %s, whose scan of a consumer's stored messages reads every filter", serverVersion, FixedReleases)}
	}
	if fixed(major, minor, patch, prerelease) {
		return nil
	}
	return &RefusedRelease{Release: serverVersion, text: fmt.Sprintf("the NATS server is nats-server %s, which is not a release whose scan of a consumer's stored messages reads every filter: from v2.10.26, and in 2.11 before v2.11.12 and 2.12 before v2.12.4, a consumer filtering on a wildcard and a literal at the same token can skip a message; Envoy needs nats-server %s", serverVersion, FixedReleases)}
}

// RefusedRelease is the error Refusal answers for a server it refuses, so a caller that names the
// release apart from the text can read it (errors.As): the Envoy listener's start logs it beside
// FixedReleases at each try while it waits for a server it takes.
type RefusedRelease struct {
	// Release is the version the server reported, as Refusal was given it.
	Release string
	text    string
}

func (r *RefusedRelease) Error() string { return r.text }

// fixed is whether a version is at or after the first fixed release of its line, a prerelease
// counting as before the release it leads to: 2.11.12 on 2.11, 2.12.4 on 2.12, 2.14.0 on 2.14,
// every release of a later line, and 2.13, which has no release. Every 2.10 and every line before
// it is not.
func fixed(major, minor, patch int, prerelease bool) bool {
	if major != 2 {
		return major > 2
	}
	at := func(first int) bool { return patch > first || (patch == first && !prerelease) }
	switch {
	case minor == 11:
		return at(12)
	case minor == 12:
		return at(4)
	case minor == 13:
		return true
	case minor == 14:
		return at(0)
	default:
		return minor > 14
	}
}

// parse reads major.minor.patch from version, a leading v dropped, and whether it is a prerelease:
// a `-` after the patch's digits (-RC.1). Build metadata after a `+` is not a prerelease.
func parse(version string) (major, minor, patch int, prerelease, ok bool) {
	fields := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(fields) != 3 {
		return 0, 0, 0, false, false
	}
	if end := strings.IndexFunc(fields[2], func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		prerelease = fields[2][end] == '-'
		fields[2] = fields[2][:end]
	}
	var err error
	if major, err = strconv.Atoi(fields[0]); err != nil {
		return 0, 0, 0, false, false
	}
	if minor, err = strconv.Atoi(fields[1]); err != nil {
		return 0, 0, 0, false, false
	}
	if patch, err = strconv.Atoi(fields[2]); err != nil {
		return 0, 0, 0, false, false
	}
	return major, minor, patch, prerelease, true
}

// VersionWait bounds how long ConnectedRefusal waits for a connection to report its server's
// version, for a caller whose context sets no nearer deadline: the listener's connection
// reconnects once a second (internal/bus Connect) and nats.go's default waits two seconds, so a
// minute covers a hub restart without holding a start for ever. The Legion daemon's boot attempt
// sets a nearer one, its 30 s bootTimeout, the same bound its stream lookup after this has.
const VersionWait = time.Minute

// reconnectPoll is how often ConnectedVersion looks again at a connection that reports no version.
const reconnectPoll = 20 * time.Millisecond

// ConnectedVersion is the version conn's server reports, read only once the connection reports
// one: (*nats.Conn).ConnectedServerVersion answers "" whenever a connection is not CONNECTED,
// while it reconnects and while it drains among them, which is no reading of the server at all.
// So an empty answer is never returned as a version: ConnectedVersion looks again until the
// connection reports one, until it is closed, or until ctx is done or VersionWait passes, and in
// the last two answers an error that the connection is not connected. It asks nothing else of the
// connection before it answers (IsConnected counts a draining connection, and a second read could
// see a reconnect the first did not), so the version it returns is the one it read. A server that
// is connected and reports no version is answered as not connected at the deadline, which is all
// that can be said of it. Every decision on a connection's server version is made on this read:
// ConnectedRefusal's, and the Envoy stream's consumer cap (internal/bus
// connectedDefaultsAnUnsetCap).
func ConnectedVersion(ctx context.Context, conn *nats.Conn) (string, error) {
	return connectedVersion(ctx, conn)
}

// ConnectedRefusal is Refusal of the version conn's server reports (ConnectedVersion), decided on
// that version alone. A connection that reports none within the wait is refused as not connected,
// naming no version.
func ConnectedRefusal(ctx context.Context, conn *nats.Conn) error {
	return connectedRefusal(ctx, conn)
}

// serverConn is what ConnectedVersion reads of a connection, the seam its tests reach a reconnect
// between two reads through.
type serverConn interface {
	ConnectedServerVersion() string
	IsClosed() bool
}

func connectedRefusal(ctx context.Context, conn serverConn) error {
	version, err := connectedVersion(ctx, conn)
	if err != nil {
		return fmt.Errorf("%w, so its server's version cannot be checked against nats-server %s", err, FixedReleases)
	}
	return Refusal(version)
}

func connectedVersion(ctx context.Context, conn serverConn) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, VersionWait)
	defer cancel()
	for {
		if version := conn.ConnectedServerVersion(); version != "" {
			return version, nil
		}
		if conn.IsClosed() {
			return "", errors.New("the NATS connection is closed and not connected")
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the NATS connection is not connected: it reported no server version within the wait (%w)", ctx.Err())
		case <-time.After(reconnectPoll):
		}
	}
}
