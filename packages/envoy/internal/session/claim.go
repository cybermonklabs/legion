package session

import "github.com/sjawhar/envoy/internal/contracts"

// ClaimStaleAfter is how long a session's claim may go unrefreshed before a reader treats the
// process that wrote it as gone: contracts.SessionClaimStale, which allows three missed
// heartbeats at contracts.SessionHeartbeat (generated from SESSION_HEARTBEAT_MS) before assuming
// it is. The Legion daemon reads the same declaration as listenerClaimStale
// (packages/daemon/internal/daemon/notice_exceptions.go). No role resolution weighs it: a holder
// is judged by how long ago its registration was written (StateOf, on LiveWindow and
// GoneHorizon), and a session route's own write weighs no claim's freshness either, since the
// route follows whichever process heartbeated last (SessionRegistry.PutWithin).
const ClaimStaleAfter = contracts.SessionClaimStale

// stampClaim is the registration a claim writes for sessionID: next, stamped with the moment of
// the write, so a reader weighing the route's freshness reads the latest heartbeat
// (ClaimStaleAfter). It keeps the fields of the stored registration cur, if there is one (found),
// that this build does not know (SessionEntry.unknown), and only where cur is bound to sessionID:
// it names sessionID as its key and a machine, which DecodeEntry checks of every record naming a
// key. A newer build's field is the session's, not the claiming process's, and survives its
// heartbeat, since every build that writes such a field names the key in its record. A record
// naming no key - an earlier build's, or a RePublish of another stream whose body names none,
// which the cache and a direct get read as a registration - carries none of its unknown keys into
// the write: nothing says they are this session's.
func stampClaim(sessionID string, cur SessionEntry, found bool, next SessionEntry, now int64) SessionEntry {
	next.UpdatedAt = now
	if found && cur.SessionID == sessionID && cur.MachineID != "" {
		next.unknown = cur.unknown
	}
	return next
}
