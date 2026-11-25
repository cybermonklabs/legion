package access

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Answer is the membership answer for one project and one person (K3), which the secrets broker
// reads with a token of its own, outside any transaction: the project's owner and visibility, the
// person's member kind as the answer spells it, whether the rule lets that person see the project,
// and the project's Slack channels with when that list last changed. It names no repositories. A
// person no people row holds reads as the broker reads them, a full member, since the broker signs
// in only people of its own sign-in domain; the answer still names their kind none. A question
// naming no person reads as the rule reads a viewer with no person: it sees the project only when
// the project is public, so visible says whether it is, as the broker reads it
// (projectaccess.Client.States). An unknown project is ErrProjectNotFound; a project the person
// cannot see still answers, with visible false.
func Answer(ctx context.Context, q Querier, key, person string) (model.AccessAnswer, error) {
	login := model.CanonicalLogin(person)
	answer := model.AccessAnswer{Person: login}
	err := q.QueryRow(ctx, `
		with person as (
			select (select member_kind from people where email = $2) as member_kind
		)
		select p.key, p.visibility, coalesce(p.owner, ''), coalesce(person.member_kind, 'none'),
			case when $2 = '' then p.visibility = 'public'
			else coalesce(`+store.PersonSeesProjectSQL("$2", "coalesce(person.member_kind, 'full')", "p")+`, false) end,
			p.slack_channels, p.slack_channels_changed_at
		from projects p, person
		where p.key = $1
	`, key, login).Scan(&answer.Project, &answer.Visibility, &answer.Owner, &answer.MemberKind, &answer.Visible,
		&answer.SlackChannels, &answer.SlackChannelsChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.AccessAnswer{}, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
	}
	if err != nil {
		return model.AccessAnswer{}, fmt.Errorf("read the membership answer of project %s: %w", key, err)
	}
	return answer, nil
}

// PersonAnswer is one person's member kind as the membership answer spells it, and AsOf, the
// database's now() read in the same statement: the time an answer whose hold the caller could not
// read (Standing nil or failing) rests on. Held is left for the caller to fill in. Its error names
// the read that failed and never the person, as MemberKind's does.
func PersonAnswer(ctx context.Context, q Querier, person string) (model.PersonAnswer, error) {
	login := model.CanonicalLogin(person)
	var kind string
	var asOf time.Time
	if err := q.QueryRow(ctx, `select coalesce((select member_kind from people where email = $1), ''), now()`, login).Scan(&kind, &asOf); err != nil {
		return model.PersonAnswer{}, fmt.Errorf("read a person's member kind: %w", err)
	}
	answer := model.PersonAnswer{Person: login, MemberKind: kind, AsOf: asOf}
	if model.MemberKind(kind) == model.MemberNone {
		answer.MemberKind = model.MemberAnswerNone
	}
	return answer, nil
}
