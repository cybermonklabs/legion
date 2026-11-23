package writingcheck

import "strings"

// The reasons a write is not checked, as Exemption answers them.
const (
	// ExemptPerson is a person writing for themselves: a user actor with no acting session.
	ExemptPerson = "exempt_person"
	// ExemptDaemon is a project's Legion daemon, or a person's act its session made.
	ExemptDaemon = "exempt_daemon"
	// ExemptSystem is Dispatch's own writer, or any actor kind that is neither a session nor a
	// user.
	ExemptSystem = "exempt_system"
)

// DaemonSessionPrefix opens the session id of every project's Legion daemon
// (legion-daemon:<PROJECT>).
const DaemonSessionPrefix = "legion-daemon:"

// Exemption is why the writing check does not read a write by the actor of kind and id, where
// actSession is the session a person's act names (the actor's act.session, "" when it names
// none), and "" for a write it reads: an agent session is checked unless it is a daemon's; a
// person's write is checked only when an agent session that is not a daemon's made it for them.
func Exemption(kind, id, actSession string) string {
	switch kind {
	case "session":
		if strings.HasPrefix(id, DaemonSessionPrefix) {
			return ExemptDaemon
		}
		return ""
	case "user":
		switch {
		case actSession == "":
			return ExemptPerson
		case strings.HasPrefix(actSession, DaemonSessionPrefix):
			return ExemptDaemon
		}
		return ""
	}
	return ExemptSystem
}
