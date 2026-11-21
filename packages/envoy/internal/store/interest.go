package store

import (
	"context"
	"slices"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// Interest is one session's persisted interest record, the value the interest bucket stores and
// the listener's interest routes answer, which the listener's wire package declares
// (listenerwire.Interest).
type Interest = listenerwire.Interest

func Merge(item Interest, topics []string) Interest {
	seen := map[string]bool{}
	out := make([]string, 0, len(item.Topics)+len(topics))
	for _, topic := range item.Topics {
		if seen[topic] {
			continue
		}
		seen[topic] = true
		out = append(out, topic)
	}
	for _, topic := range topics {
		if seen[topic] {
			continue
		}
		seen[topic] = true
		out = append(out, topic)
	}
	item.Topics = out
	return item
}

// oneInterest is what makes two spellings of a name one interest while every reader takes both
// forms: the old form of a topic that has one (contracts.LegacyTwin), and the topic itself
// otherwise. It is read on both sides of a removal, because the new form of an agent's name is a
// wildcard over every project (notifications.agent.*.s1), which no stored topic equals: a reading
// that compared the forms as strings would leave a record's notifications.agent._.s1 and
// notifications.role.PRIV.<role> standing after their own unsubscribe. A topic with no old form,
// one over every issue of a project among them, is its own interest.
func oneInterest(topic string) string {
	if twin, ok := contracts.LegacyTwin(topic); ok {
		return twin
	}
	return topic
}

// Remove takes topics out of item, each in every form a reader takes it in (oneInterest): the old
// and new forms of an issue's, an agent's or a role's name are one interest, and a form left
// behind would keep the session's consumer filtered on it. No topics at all removes every topic.
func Remove(item Interest, topics []string) Interest {
	if len(topics) == 0 {
		item.Topics = nil
		return item
	}
	cut := map[string]bool{}
	for _, topic := range topics {
		cut[oneInterest(topic)] = true
	}
	out := make([]string, 0, len(item.Topics))
	for _, topic := range item.Topics {
		if cut[oneInterest(topic)] {
			continue
		}
		out = append(out, topic)
	}
	item.Topics = out
	return item
}

// dropPrivateStreamTopics removes from topics every one reaching a private stream subject that is
// not sessionID's own (bus.ReachesPrivateStreamSubject): another session's inbox, a
// failed-delivery lane, a controller notice lane of a project whose controller role roles does
// not name, or a wildcard overlapping one. Registry.Match fans a stored topic out to the listener
// the row names, which delivers it to that row's session, so such a topic would hand that session
// messages meant for another session or for another project's controller. The listener refuses
// one at /v1/interests/subscribe; this drops one a row stored before that guard held, and one
// whose role the session has since lost, so every write of a row (Upsert, Remove) cleans it.
// roles is what the session holds now, as each controller role's claim record says
// (Registry.controllerLaneRoles). sessionID is the row's key, not the session_id its stored value
// names. It filters topics in place, so callers pass a slice they own (Merge's and Remove's
// results are).
func dropPrivateStreamTopics(sessionID string, roles []string, topics []string) []string {
	return slices.DeleteFunc(topics, func(topic string) bool {
		return bus.ReachesPrivateStreamSubject(sessionID, topic, roles)
	})
}

// MergedTopics is the topic set an upsert of topics into a row of sessionID holding cur stores:
// their union (Merge), without a topic reaching a private stream subject that is not the session's
// own (dropPrivateStreamTopics), sorted. mergeForUpsert stores exactly this, and the listener's
// registration route filters the session's consumer on it before the write (cmd/listener's
// subscribeHandler, through Registry.PlanInterest), so the filter and the row it stores name one
// set: a filter naming a topic the row does not would carry that topic into a session no interest
// of its own follows, and a row holding one the filter does not would say a session follows a
// topic its consumer never delivers. roles is the controller roles the session holds, each read
// from its claim record and not from the role claim copy (Registry.controllerLaneRoles), so the
// set keeps a controller notice lane exactly while the store gives the session that project's
// controller role.
func MergedTopics(sessionID string, roles []string, cur []string, topics []string) []string {
	merged := dropPrivateStreamTopics(sessionID, roles, Merge(Interest{Topics: cur}, topics).Topics)
	slices.Sort(merged)
	return merged
}

// FollowableTopics is topics, read from sessionID's stored interest row, without each one reaching a
// private stream subject that is not the session's own (dropPrivateStreamTopics): the rule every
// write of a row applies (MergedTopics), applied again to what a read finds. A row is a record any
// writer of the bucket can store, so the one stored under sessionID's own key, naming its key, a
// machine and a time, can still name another session's inbox or another project's controller
// notice lane, and a consumer filtered on such a row would hand the session that other session's
// messages. The controller roles the session holds are read from their claim records
// (controllerLaneRoles), as a registration reads them, and only when topics name a controller
// notice lane, waiting no longer than ctx; a read that did not answer or failed is returned, and
// the caller builds no filter from the row. topics is not changed.
func (r *Registry) FollowableTopics(ctx context.Context, sessionID string, topics []string) ([]string, error) {
	roles, err := r.controllerLaneRoles(ctx, sessionID, topics)
	if err != nil {
		return nil, err
	}
	return dropPrivateStreamTopics(sessionID, roles, slices.Clone(topics)), nil
}

// InterestPlan is one registration's whole decision about a session's topics: the topics its
// interest row will hold, which are its session consumer's filter too, and the roles read to
// decide them, which name the one private stream subject a session registers beside its own agent
// subject (dropPrivateStreamTopics). Registry.PlanInterest reads the roles once and merges once;
// the listener's registration route builds the filter from the plan and hands the same plan back
// to Registry.UpsertPlanned, so no second read can decide the row differently from the filter.
// Withheld is the topics of the stored row a caller's rule of its own takes out of the row
// (cmd/listener's edge hold): they are out of Topics, and the write takes them out of the row it
// reads before it merges Topics in (mergeForUpsert).
//
// Revision is the bucket revision of the row the plan merged its topics into, 0 for a session
// with no row: Registry.UpsertPlanned writes at that revision and refuses the write when another
// writer has stored the row since (ErrInterestPlanConflict), and the route then plans again from
// the topics it asked for (Registry.PlanInterestAfterConflict), filters the consumer on the new
// plan and writes that, so an unsubscribe that landed in the seconds between a plan and its
// write is not undone and the filter is never left standing for a plan the row does not hold.
type InterestPlan struct {
	Roles    []string
	Topics   []string
	Withheld []string
	Revision uint64
	Dropped  []string
	// afterConflict is set on a plan PlanInterestAfterConflict made, whose row was read from the
	// stream's leader; UpsertPlanned reads the row it fences the write on the same way.
	afterConflict bool
}

// Without is the plan with topics left out of it: out of Topics, which the session's consumer is
// filtered on, and out of the row its write stores (Dropped), the stored row's own topics among
// them. It is how a caller holding a session to a rule of its own (cmd/listener's edge listener,
// which holds a session to its token's projects) drops what that rule refuses from a plan whose
// topics the store merged in from the row another listener wrote. The plan's revision stands, so
// its write is still refused when another writer stored the row since.
func (p InterestPlan) Without(topics []string) InterestPlan {
	if len(topics) == 0 {
		return p
	}
	p.Topics = slices.DeleteFunc(slices.Clone(p.Topics), func(topic string) bool { return slices.Contains(topics, topic) })
	p.Dropped = append(slices.Clone(p.Dropped), topics...)
	return p
}
