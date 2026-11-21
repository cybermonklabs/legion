package writingcheck

import "strings"

// The reasons a write is not checked, as Checks answers them.
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

// Checks reports whether the writing check reads a write by the actor of kind and id, where
// actSession is the session a person's act names (the actor's act.session, "" when it names
// none). An agent session is checked unless it is a daemon's; a person's write is checked only
// when an agent session that is not a daemon's made it for them. A write not checked answers why
// in exemption; a checked one answers "".
func Checks(kind, id, actSession string) (checked bool, exemption string) {
	switch kind {
	case "session":
		if strings.HasPrefix(id, DaemonSessionPrefix) {
			return false, ExemptDaemon
		}
		return true, ""
	case "user":
		switch {
		case actSession == "":
			return false, ExemptPerson
		case strings.HasPrefix(actSession, DaemonSessionPrefix):
			return false, ExemptDaemon
		}
		return true, ""
	}
	return false, ExemptSystem
}
