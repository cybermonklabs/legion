package access

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Answer is the membership answer for one project and one person (K3), which the secrets broker
// reads with a token of its own, outside any transaction: the project's owner and visibility, the
// person's member kind as the answer spells it, whether the rule lets that person see the project,
// and the project's Slack channels with when that list last changed. It names no repositories. A
// person no people row holds reads as the broker reads them, a full member, since the broker signs
// in only people of its own sign-in domain; the answer still names their kind none. An unknown
// project is ErrProjectNotFound; a project the person cannot see still answers, with visible
// false.
func Answer(ctx context.Context, q Querier, key, person string) (model.AccessAnswer, error) {
	login := model.CanonicalLogin(person)
	answer := model.AccessAnswer{Person: login}
	err := q.QueryRow(ctx, `
		with person as (
			select (select member_kind from people where email = $2) as member_kind
		)
		select p.key, p.visibility, coalesce(p.owner, ''), coalesce(person.member_kind, 'none'),
			$2 <> '' and coalesce(`+PersonSeesProjectSQL("$2", "coalesce(person.member_kind, 'full')", "p")+`, false),
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

// PersonAnswer is one person's member kind as the membership answer spells it.
func PersonAnswer(ctx context.Context, q Querier, person string) (model.PersonAnswer, error) {
	login := model.CanonicalLogin(person)
	kind, err := MemberKind(ctx, q, login)
	if err != nil {
		return model.PersonAnswer{}, err
	}
	answer := model.PersonAnswer{Person: login, MemberKind: string(kind)}
	if kind == model.MemberNone {
		answer.MemberKind = model.MemberAnswerNone
	}
	return answer, nil
}
