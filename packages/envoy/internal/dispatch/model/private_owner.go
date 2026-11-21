package model

import "fmt"

// PrivateOwnerSQL is whether the events row alias names belongs to an owner whose project the
// database holds private, by the project that owner resolves to: a project event's own, an issue's,
// a document's. alias names a row with the events table's three owner columns, project_key,
// issue_key and artifact_id. It is true or false, never null, and the one test the private-delivery
// hold makes of an event: the outbox's reads of what it holds (internal/dispatch/outbox) and the
// event log's refusal of a private project's event once the outbox holds as many as it may
// (events.Broker.HoldPrivate). Every event of one owner resolves to one project, so the hold takes
// an owner whole and never one event of it alone.
func PrivateOwnerSQL(alias string) string {
	return fmt.Sprintf(`exists (
		select 1 from projects held
		where held.visibility = 'private'
		  and held.key = coalesce(%[1]s.project_key,
		      (select i.project_key from issues i where i.key = %[1]s.issue_key),
		      (select a.project_key from artifacts a where a.id = %[1]s.artifact_id)))`, alias)
}
