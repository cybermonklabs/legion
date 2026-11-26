package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The broker keeps the last answer Dispatch's project route gave it for each person and project in
// 0049's project_standing, so that while Dispatch cannot be asked the request machine's person rule
// answers from it (projectaccess.OrKept): a project Dispatch last answered public or visible to the
// person shows them the row, until an answer for anyone says the project is private since, and
// every other answer, or none, refuses as before. Every process writes the answers its own asks
// fetched, and the row keeps the one asked latest by the clock of the process that asked, since
// Dispatch's project answer names no time of its own.

// KeepProjectStanding writes Dispatch's answer for person and project, asked at askedAt, unless the
// row already holds an answer asked later: visibility as Dispatch answered it, "public" or
// "private", and whether person may see the project; or, for visibility "", that Dispatch holds no
// such project, which is written for no person (the empty person), since it is the same answer for
// everyone. person is a login as record.CanonicalLogin spells it, or empty for a question naming
// no person.
func (s *Store) KeepProjectStanding(ctx context.Context, person, project, visibility string, visible bool, askedAt time.Time) error {
	if visibility == "" {
		person, visible = "", false
	}
	_, err := s.Pool.Exec(ctx, `insert into project_standing (person, project, visibility, visible, asked_at) values ($1, $2, nullif($3, ''), $4, $5)
		on conflict (person, project) do update set visibility = excluded.visibility, visible = excluded.visible, asked_at = excluded.asked_at
		where project_standing.asked_at < excluded.asked_at`, person, project, visibility, visible, askedAt)
	return err
}

// ProjectStanding is one answer of Dispatch's project route as the broker keeps it.
type ProjectStanding struct {
	// Visibility is "public" or "private" as Dispatch answered it, or "" for an answer that
	// Dispatch holds no such project.
	Visibility string
	// Visible is whether the person the answer was asked for may see the project.
	Visible bool
	// AskedAt is when the broker asked, read before the question was sent.
	AskedAt time.Time
}

// KeptProjectStanding is the last answer kept for person and project: the later, by AskedAt, of
// the answer kept for that person and an answer that Dispatch holds no such project, which is
// kept for no person. Whether the project exists and whether it is public are facts of the
// project, the same for every person, so that last answer stands only while no answer kept for
// any person, no person included, and asked at or after it, says otherwise: a no-project answer
// until an answer that the project exists, and a public answer until an answer that it is
// private. Once one does, the project was made, or made private, since, and a person with no
// answer of their own asked after it is kept nothing, so a read while Dispatch cannot be asked
// refuses them as one never asked rather than show them the project's rows on the older fact. A
// private answer is not ended by a later public one: whether the person may see a private
// project rests on its owner and its access list, which a switch to public takes from nobody.
// found is false when no answer is kept, which is no error.
func (s *Store) KeptProjectStanding(ctx context.Context, person, project string) (standing ProjectStanding, found bool, err error) {
	err = s.Pool.QueryRow(ctx, `select coalesce(last.visibility, ''), last.visible, last.asked_at from (
			select visibility, visible, asked_at from project_standing
			where project = $2 and (person = $1 or (person = '' and visibility is null))
			order by asked_at desc limit 1) last
		where not exists (
			select 1 from project_standing later
			where later.project = $2 and later.asked_at >= last.asked_at
				and case when last.visibility is null then later.visibility is not null
					when last.visibility = 'public' then later.visibility = 'private'
					else false end)`, person, project).Scan(&standing.Visibility, &standing.Visible, &standing.AskedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectStanding{}, false, nil
	}
	if err != nil {
		return ProjectStanding{}, false, err
	}
	return standing, true, nil
}
