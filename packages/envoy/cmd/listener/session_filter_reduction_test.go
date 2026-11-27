package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// A session's consumer filter reaches no other session's private subjects even after the server's
// filter reduction merges its topics: bus.SessionConsumerFilters replaces two overlapping topics
// that neither contains with their longest common prefix of whole tokens followed by `>`, so two
// wildcard topics that each pass the private rule alone can reduce to one that reaches every
// session's inbox. The filter holds the row's topics whatever the deployed stream captures, so it
// must hold this line itself.
//
// The rows show the ways it happens. A wildcard first token takes the reduction out of the
// notification namespace altogether: *.team.foo, which an earlier build's registration stored
// (it does not begin with notifications., so the publish verdict sends it through JetStream and
// ReachesNoSession is false for it), beside notifications.team.*, which the stream captures,
// reduce to `>`, the whole stream. The filter carries only topics that begin with the literal
// notifications., so such a topic is left out. Inside the namespace, two wildcards an earlier
// build's row may hold, notifications.envoy.exceptions.foo.* and notifications.envoy.exceptions.*.bar,
// reduce to notifications.envoy.exceptions.>, which reaches every session's failed deliveries;
// both are left out, and the filter keeps everything else, a wildcard that merged with neither of
// them among it. Another session's agent subject as an earlier build left it is a literal topic
// that stays, whose new form (notifications.agent.*.<that session>) is a reduced subject that
// reaches another session and is that literal's own, no merge; it sorts before the exception
// lanes' merge, which is left out all the same.
//
// The last two rows are the reduction's own merges, which the filter follows by provenance: the
// topics left out are exactly those whose forms fed the merges that made a reduced subject
// reaching another session. In a chain, two wildcards merge into a subject that reaches nothing
// private, and that merge then merges with a third wildcard into one that does: all three fed it,
// and all three go, though no two of them overlap as the row spells them but the first pair. And a
// pair that merges only through one topic's new form (contracts.TopicForms) goes, while a third
// wildcard the merged subject contains, which fed no merge, stays.
func TestAFilterWhoseTopicsReduceToAnotherSessionsSubjectsLeavesThemOut(t *testing.T) {
	t.Parallel()
	const (
		session = "ses_reduction"
		// The chain: chainA and chainB merge into the GitHub exception lanes, which reach no
		// session's private subjects, and that merge with chainC into every exception lane.
		chainA = "notifications.envoy.exceptions.notifications.github.*.pr"
		chainB = "notifications.envoy.exceptions.notifications.github.repo.*"
		chainC = "notifications.envoy.exceptions.*.github.foo"
		// The forms: issueOld is an issue's exception lane in the old form, whose new form
		// names the key's project; formMate overlaps that new form and not the old one.
		issueOld = "notifications.envoy.exceptions.notifications.dispatch.issue.KEY-1.>"
		issueNew = "notifications.envoy.exceptions.notifications.dispatch.issue.KEY.KEY-1.>"
		formMate = "notifications.envoy.exceptions.*.dispatch.issue.KEY.KEY-1.foo"
		github   = "notifications.envoy.exceptions.notifications.github.>"
		every    = "notifications.envoy.exceptions.>"
	)
	for _, tc := range []struct {
		name   string
		topics []string
		kept   []string
		left   []string
		// premise checks what makes the row the case it names, so a change to the reduction or
		// to the forms that unmakes the case fails here rather than passing on a row that no
		// longer shows it.
		premise func(t *testing.T)
	}{
		{
			name:   "a wildcard first token beside a team wildcard",
			topics: []string{"*.team.foo", "notifications.team.*"},
			kept:   []string{"notifications.team.*"},
			left:   []string{"*.team.foo"},
		},
		{
			name:   "two exception-lane wildcards an earlier build's row holds",
			topics: []string{"notifications.envoy.exceptions.foo.*", "notifications.envoy.exceptions.*.bar", "notifications.team.deploy"},
			kept:   []string{"notifications.team.deploy"},
			left:   []string{"notifications.envoy.exceptions.foo.*", "notifications.envoy.exceptions.*.bar"},
		},
		{
			name: "the same pair beside an exception-lane wildcard that merges with neither",
			topics: []string{
				"notifications.envoy.exceptions.foo.*", "notifications.envoy.exceptions.*.bar",
				github, "notifications.team.deploy",
			},
			kept: []string{github, "notifications.team.deploy"},
			left: []string{"notifications.envoy.exceptions.foo.*", "notifications.envoy.exceptions.*.bar"},
		},
		{
			name: "the pair beside another session's agent subject an earlier build left",
			topics: []string{
				contracts.LegacyAgentSubject("ses_other"),
				"notifications.envoy.exceptions.foo.*", "notifications.envoy.exceptions.*.bar", "notifications.team.deploy",
			},
			kept: []string{contracts.LegacyAgentSubject("ses_other"), "notifications.team.deploy"},
			left: []string{"notifications.envoy.exceptions.foo.*", "notifications.envoy.exceptions.*.bar"},
		},
		{
			name:   "a chain of merges whose last reaches every exception lane",
			topics: []string{chainA, chainB, chainC, "notifications.team.deploy"},
			kept:   []string{"notifications.team.deploy"},
			left:   []string{chainA, chainB, chainC},
			premise: func(t *testing.T) {
				if got := bus.SessionConsumerFilters([]string{chainA, chainB}); !slices.Equal(got, []string{github}) || bus.ReachesPrivateStreamSubject(session, github, nil) {
					t.Fatalf("%s and %s reduce to %v, want %s alone, which reaches no private subject", chainA, chainB, got, github)
				}
				for _, other := range []string{chainA, chainB} {
					if bus.SubjectsOverlap(chainC, other) {
						t.Fatalf("%s overlaps %s, so it would merge with it as the row spells them", chainC, other)
					}
				}
				if got := bus.SessionConsumerFilters([]string{chainA, chainB, chainC}); !slices.Equal(got, []string{every}) || !bus.ReachesPrivateStreamSubject(session, every, nil) {
					t.Fatalf("the chain reduces to %v, want %s, which reaches another session's failed deliveries", got, every)
				}
			},
		},
		{
			name:   "a pair that merges only through one topic's new form beside a wildcard that fed no merge",
			topics: []string{issueOld, formMate, github, "notifications.team.deploy"},
			kept:   []string{github, "notifications.team.deploy"},
			left:   []string{issueOld, formMate},
			premise: func(t *testing.T) {
				if got := contracts.TopicForms(issueOld); !slices.Equal(got, []string{issueOld, issueNew}) {
					t.Fatalf("the forms of %s are %v, want it and %s", issueOld, got, issueNew)
				}
				if bus.SubjectsOverlap(formMate, issueOld) || !bus.SubjectsOverlap(formMate, issueNew) {
					t.Fatalf("%s must overlap the new form %s and not the old %s", formMate, issueNew, issueOld)
				}
				for _, other := range []string{issueOld, issueNew, formMate} {
					if bus.SubjectsOverlap(github, other) {
						t.Fatalf("%s overlaps %s, so it would feed a merge", github, other)
					}
				}
				if got := bus.SessionConsumerFilters([]string{issueOld, formMate, github}); !slices.Equal(got, []string{every}) {
					t.Fatalf("the row reduces to %v, want %s, which contains %s", got, every, github)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.premise != nil {
				tc.premise(t)
			}
			for _, topic := range tc.topics {
				if hasWildcardToken(topic) && bus.ReachesPrivateStreamSubject(session, topic, nil) {
					t.Fatalf("%s reaches a private subject by itself; the row needs wildcards that pass the rule alone", topic)
				}
			}
			filter := sessionConsumerFilter(session, tc.topics)
			// A literal topic of the row stays, and so do its own forms (contracts.TopicForms): they
			// name the subjects that topic names, and no reduction of the filter's wildcards.
			var literalForms []string
			for _, topic := range filter {
				if !hasWildcardToken(topic) {
					literalForms = append(literalForms, contracts.TopicForms(topic)...)
				}
			}
			for _, reduced := range bus.SessionConsumerFilters(filter) {
				if strings.ContainsAny(reduced, "*>") && !slices.Contains(literalForms, reduced) && bus.ReachesPrivateStreamSubject(session, reduced, nil) {
					t.Errorf("the filter %v reduces to %s, which reaches another session's private subjects", filter, reduced)
				}
			}
			for _, topic := range tc.kept {
				if !slices.Contains(filter, topic) {
					t.Errorf("the filter %v lacks %s, which fed no merge reaching another session", filter, topic)
				}
			}
			for _, topic := range tc.left {
				if slices.Contains(filter, topic) {
					t.Errorf("the filter %v holds %s", filter, topic)
				}
			}
			if !slices.Contains(filter, contracts.LegacyAgentSubject(session)) {
				t.Errorf("the filter %v lacks the session's own agent subject", filter)
			}
		})
	}
}
