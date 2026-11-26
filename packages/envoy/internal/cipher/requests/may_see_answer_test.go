// packages/envoy/internal/cipher/requests/may_see_answer_test.go
// The deep review's round 3 on cybermonklabs/legion#4779: once a list's clock has ended, an answer
// the list or the client already holds is still answered (R3-1), and a project no read answered
// never shows a row.
package requests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
)

// TestAnAnswerTheListOrTheClientHoldsIsStillAnsweredOnceTheClockHasEnded: maySee asks every
// project of a row whatever its context's state, so once a list's clock has ended
// (listOwnRowsBound) an answer the list's memo or the client's cache already holds is still
// answered, and only a question Dispatch would have to be asked now fails. Each case warms its
// source with one maySee on a live context, then asks again on a context whose deadline has
// passed: the reason naming TEAM-1 is shown, and Dispatch is asked nothing. On the same ended
// context a reason naming PRIV-1, which neither source holds, is ErrProjectAccessUnavailable, so
// the context has ended for every question Dispatch would have to answer.
func TestAnAnswerTheListOrTheClientHoldsIsStillAnsweredOnceTheClockHasEnded(t *testing.T) {
	const held, unheld = teamProject + "-1: rotate the deploy key", privProject + "-1: rotate the private key"
	for _, c := range []struct {
		name string
		// warm is the context the warm-up reads on, made from a live one.
		warm func(live context.Context) context.Context
		// ending is the context the ended one is made from: the warm-up's, or another.
		ending func(live, warm context.Context) context.Context
		// fresh points the broker at a client that holds no answer before the ended reads, so
		// that the list's memo alone can answer them.
		fresh bool
	}{
		{
			name:   "the list's memo",
			warm:   withAccessMemo,
			ending: func(_, warm context.Context) context.Context { return warm },
			fresh:  true,
		},
		{
			name:   "the client's cache, read by a later list",
			warm:   withAccessMemo,
			ending: func(live, _ context.Context) context.Context { return withAccessMemo(live) },
		},
		{
			name:   "the client's cache, read outside a list",
			warm:   func(live context.Context) context.Context { return live },
			ending: func(live, _ context.Context) context.Context { return live },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch := projectaccesstest.NewServer(t, dispatchRows(publicTeam), nil)
			m := &Machine{Projects: projectaccess.New(dispatch.URL, dispatch.Account, nil)}
			warm := c.warm(t.Context())
			if shown, _, err := m.maySee(warm, outsider, nil, held); !shown || err != nil {
				t.Fatalf("maySee(%s, %q) on a live context = %v, %v; want true, nil", outsider, held, shown, err)
			}
			if dispatch.Calls() == 0 {
				t.Fatal("the warm-up asked Dispatch nothing, so nothing holds TEAM's answer: the test is blind")
			}
			if c.fresh {
				dispatch = projectaccesstest.NewServer(t, dispatchRows(publicTeam), nil)
				m.Projects = projectaccess.New(dispatch.URL, dispatch.Account, nil)
			}
			before := dispatch.Calls()
			ended, cancel := context.WithDeadline(c.ending(t.Context(), warm), time.Now())
			defer cancel()
			if ended.Err() == nil {
				t.Fatal("the context's deadline has passed and it has not ended: the test is blind")
			}

			if shown, _, err := m.maySee(ended, outsider, nil, held); !shown || err != nil {
				t.Errorf("maySee(%s, %q) once the clock has ended = %v, %v; want true, nil: %s holds TEAM's answer", outsider, held, shown, err, c.name)
			}
			if shown, _, err := m.maySee(ended, outsider, nil, unheld); shown || !errors.Is(err, ErrProjectAccessUnavailable) {
				t.Errorf("maySee(%s, %q) once the clock has ended = %v, %v; want ErrProjectAccessUnavailable: nothing holds PRIV's answer, and Dispatch can no longer be asked", outsider, unheld, shown, err)
			}
			if calls := dispatch.Calls() - before; calls != 0 {
				t.Errorf("Dispatch was asked %d times once the clock had ended; want none", calls)
			}
		})
	}
}

// noSuchProjectBare is a projectaccess.Access for a source that keeps no clock: it answers every
// access question about project with projectaccess.ErrProjectNotFound alone, no ask time beside
// it, and passes every other question to inner.
type noSuchProjectBare struct {
	inner   projectaccess.Access
	project string
}

func (n noSuchProjectBare) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	if project != n.project {
		return n.inner.Access(ctx, project, person)
	}
	return projectaccess.Answer{}, projectaccess.ErrProjectNotFound
}

func (n noSuchProjectBare) MemberKind(ctx context.Context, person string) (string, error) {
	return n.inner.MemberKind(ctx, person)
}

func (n noSuchProjectBare) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return n.inner.States(ctx, projects)
}

// TestARowIsShownOnlyOnAnAnswerForEachOfItsProjects: maySee shows a row only when a read
// answered for each project the row names (maySeeAnswer). Its loop stops asking only once a
// project hides the row. A project no read answered, as a loop that stopped early for any other
// reason would leave it, is ErrProjectAccessUnavailable and never reads as shown; a project that
// hides the row hides it however many were left unasked. A project Dispatch does not hold still
// hides nothing, whether the client answers it with the time it asked
// (projectaccess.ProjectNotFoundError) or a source that keeps no clock answers
// projectaccess.ErrProjectNotFound alone.
func TestARowIsShownOnlyOnAnAnswerForEachOfItsProjects(t *testing.T) {
	asked := time.Now()
	shown := projectSeen{answered: true, asked: asked}
	for _, c := range []struct {
		name  string
		seens []projectSeen
		want  bool
		err   error
	}{
		{"a loop that stopped after the first of three projects", []projectSeen{shown, {}, {}}, false, ErrProjectAccessUnavailable},
		{"a loop that stopped before the last of three projects", []projectSeen{shown, {answered: true}, {}}, false, ErrProjectAccessUnavailable},
		{"a project that hides the row, the loop stopping there", []projectSeen{shown, {answered: true, hidden: true}, {}}, false, nil},
		{"every project answered, one of them no such project", []projectSeen{shown, {answered: true}, {answered: true, asked: asked.Add(time.Second)}}, true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, at, err := maySeeAnswer(c.seens)
			if got != c.want || !errors.Is(err, c.err) {
				t.Errorf("maySeeAnswer(%+v) = %v, %v; want %v, %v", c.seens, got, err, c.want, c.err)
			}
			if c.want && !at.Equal(asked) {
				t.Errorf("maySeeAnswer(%+v) answers the time %s; want the oldest answer's, %s", c.seens, at, asked)
			}
		})
	}

	const reason = goneProject + "-1: retire the old key, as " + teamProject + "-1 asks"
	for _, c := range []struct {
		name string
		// source is the broker's client of Dispatch, given one that serves dispatchRows.
		source func(client projectaccess.Access) projectaccess.Access
	}{
		{"Dispatch holds no such project", func(client projectaccess.Access) projectaccess.Access { return client }},
		{"a source that keeps no clock holds no such project", func(client projectaccess.Access) projectaccess.Access {
			return noSuchProjectBare{inner: client, project: goneProject}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatch := projectaccesstest.NewServer(t, dispatchRows(publicTeam), nil)
			log := &fanOutLog{inner: c.source(projectaccess.New(dispatch.URL, dispatch.Account, nil))}
			m := &Machine{Projects: log}
			if got, _, err := m.maySee(t.Context(), outsider, nil, reason); !got || err != nil {
				t.Errorf("maySee(%s, %q) = %v, %v; want true, nil: a project Dispatch does not hold hides nothing", outsider, reason, got, err)
			}
			for _, project := range []string{goneProject, teamProject} {
				if n := log.asked[project+" for "+outsider]; n != 1 {
					t.Errorf("maySee asked about %s %d times; want once", project, n)
				}
			}
		})
	}
}
