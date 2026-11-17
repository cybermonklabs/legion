package store

// TopicMatcher indexes a key's subscribed topics and reports every key whose topics match a
// published topic. It is the interest rows' own subject index (subjectIndex), exported for the
// known-session records (internal/session), which are keyed by session id and have no machine: a
// publish must count up to ~100,000 of them per request, which a linear scan over the records
// cannot answer, while the index follows the published topic's tokens and the patterns that share
// them. Its wildcard rules are routing.Match's, as subjectIndex's are. Every key is indexed under
// the empty machine id, so a match walks that one tree.
//
// A TopicMatcher is not safe for concurrent use: its one caller, session.KnownSessions, keeps it
// in step with its record map under that store's own lock.
type TopicMatcher struct {
	index *subjectIndex
}

// NewTopicMatcher returns an empty matcher.
func NewTopicMatcher() *TopicMatcher {
	return &TopicMatcher{index: newSubjectIndex()}
}

// Set indexes key under topics, replacing whatever topics key was indexed under. An empty or nil
// topics list leaves key matching nothing, as a record with no topic does.
func (m *TopicMatcher) Set(key string, topics []string) {
	m.index.set(key, Interest{Topics: topics})
}

// Remove drops key from the index, and every node it alone kept.
func (m *TopicMatcher) Remove(key string) {
	m.index.remove(key)
}

// Match calls visit with each key whose topics match topic, once per key (oncePerKey): the index
// underneath visits a key once per matching topic, so a caller that counts keys needs no set of
// its own.
func (m *TopicMatcher) Match(topic string, visit func(key string)) {
	m.index.match("", topic, oncePerKey(visit))
}
