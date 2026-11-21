package daemon

import "strings"

// issueSubject is the subject Dispatch publishes an event of issue key on, in the form whose
// fourth token is the issue's project (notifications.dispatch.issue.<project>.<key>.<event>), the
// only form a daemon's Dispatch consumer hears. The project is the key up to its last `-`, as an
// issue key is <project>-<number>.
func issueSubject(key, event string) string {
	return "notifications.dispatch.issue." + key[:strings.LastIndex(key, "-")] + "." + key + "." + event
}
