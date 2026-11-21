package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// wouldDeliverRoutingSQL reads what a batch reads of an event's owner to route it (readBatchSQL):
// the route of the event's issue, else of the issue whose document the event is about; the session
// that holds the event's issue's claim, "" for none or a person; and the document's slug and
// project, which the event's envelope names.
const wouldDeliverRoutingSQL = `
	select coalesce(i.route, ai.route),
	       coalesce(case when i.claimed_by ->> 'kind' = 'session' then i.claimed_by ->> 'id' end, ''),
	       coalesce(ar.slug, ''), coalesce(i.project_key, ar.project_key, '')
	from (select $1::text as issue_key, $2::uuid as artifact_id) e
	left join issues i on i.key = e.issue_key
	left join artifacts ar on ar.id = e.artifact_id
	left join issues ai on ai.key = ar.issue_key
`

// WouldDeliver reports whether event, were its actor to write it now, would reach an agent
// session: whether the destinations eventDestinations lists for it hold a session's own subject or
// a role's topic. It is answered by eventDestinations itself, over the routing a batch would read
// of the event's owner now and the recipients the batch's lookups would read, with the payload
// decoded from JSON as the batch decodes the stored event's, so the two cannot disagree. Notify is
// set as broker sets it at the event's append (events.Broker.Notify). It writes nothing:
// eventDestinations lists the fallback rows the batch would hold without writing them, and a new
// comment's mentions are left out of the payload, as their own sends reach the sessions they name
// and their settle writes, so no mention is read and no copy is left to such a row. An event that
// names no issue, document or project reaches nobody.
func WouldDeliver(ctx context.Context, database *store.Store, broker *events.Broker, event model.Event) (bool, error) {
	if event.IssueKey == nil && event.ArtifactID == nil && event.ProjectKey == nil && event.Project == "" {
		return false, nil
	}
	event.Notify = broker.Notify(event)
	encoded, err := json.Marshal(event.Payload)
	if err != nil {
		return false, fmt.Errorf("encode the payload of a %s event: %w", event.Type, err)
	}
	event.Payload = nil
	if err := json.Unmarshal(encoded, &event.Payload); err != nil {
		return false, fmt.Errorf("decode the payload of a %s event: %w", event.Type, err)
	}
	if members, ok := event.Payload.(map[string]any); ok {
		delete(members, "mentions")
	}
	item := &batchEvent{pendingEvent: pendingEvent{event: event}}
	var project string
	err = database.Pool.QueryRow(ctx, wouldDeliverRoutingSQL, event.IssueKey, event.ArtifactID).Scan(
		&item.routing.route, &item.routing.claimant, &item.slug, &project)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read the routing of a %s event: %w", event.Type, err)
	}
	if item.event.Project == "" && event.ProjectKey != nil {
		item.event.Project = *event.ProjectKey
	}
	if item.event.Project == "" {
		item.event.Project = project
	}
	if envelope, err := envelope(item.event, item.slug); err == nil {
		item.item = envelope
	}
	want := newWanted()
	_, _, _, _ = eventDestinations(ctx, Deps{Store: database}, item, want)
	found, err := loadLookups(ctx, database.Pool, want)
	if err != nil {
		return false, fmt.Errorf("read the recipients of a %s event: %w", event.Type, err)
	}
	list, _, _, err := eventDestinations(ctx, Deps{Store: database}, item, found)
	if err != nil {
		return false, fmt.Errorf("list the destinations of a %s event: %w", event.Type, err)
	}
	for _, destination := range list {
		if reachesSession(destination.subject) {
			return true, nil
		}
	}
	return false, nil
}

// reachesSession reports whether subject is a session's own subject or a role's topic, which the
// role's holder is forwarded: a destination an agent session receives.
func reachesSession(subject string) bool {
	return strings.HasPrefix(subject, contracts.RoleTopicPrefix) || strings.HasPrefix(subject, agentSubjectPrefix)
}

// agentSubjectPrefix begins every session's own subject (contracts.LegacyAgentSubject).
var agentSubjectPrefix = strings.TrimSuffix(contracts.LegacyAgentSubject("-"), "-")
