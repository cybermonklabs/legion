package model

// SessionProjects is one session's projects as the listener answers them: the projects the token
// that made the session's registration lists (K6, listenerwire.SessionEntry.Projects), sorted.
// The listener authenticated that token before it recorded the list, and Dispatch reads the answer
// over its own authenticated connection, so each project in it is one the session's own
// credential proved, as an event of the session proves the project it is of. Dispatch records the
// pairs in session_projects (events.SessionRegistrations), which places the session among each
// project's agents (access.SessionClause).
type SessionProjects struct {
	SessionID string
	Projects  []string
}
