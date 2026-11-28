package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// CheckPersonWrites is the rule c13 holds every try's messages and comments to: each message
// and comment the person wrote during the try (each message.created and comment.created by the
// person) belongs to a task that asks for it (the task allows it, or a status task messages a
// session whose reply it relies on) and to a row of an approved plan of the person's run: a row
// the person or Auto-review approved, whose landed request (going_landings) made the item. It
// answers one line for each that breaks the rule, saying why, and none for a try that keeps it.
// q reaches the try's store, which before and after were read from.
//
// A landed request made an item when its recorded answer names the item's id. A route that does
// not hand its answer to the landing (going_landed.go: the create of a comment, and of a message
// on an issue, among them) leaves that answer empty, so such an item is made by a request of the
// run its author's act names (Actor.Act) that posted to the route making that kind of item on the
// item's own issue, document or ask. Each landed request makes one item at most, so two items
// one request could have made are not both counted to it.
func CheckPersonWrites(ctx context.Context, q Querier, seed *Seed, task Task, before, after *Store, person string) ([]string, error) {
	landings, err := approvedLandings(ctx, q, person)
	if err != nil {
		return nil, err
	}
	used := make([]bool, len(landings))
	var broken []string
	for _, c := range Diff(seed, before, after, person) {
		if c.ID == "" || (c.Field != FieldMessage && c.Field != FieldComment && c.Field != FieldSuggestion) {
			continue
		}
		if !allowed(task, c, nil) {
			broken = append(broken, fmt.Sprintf("%s: task %s does not ask for it", c.What, task.ID))
			continue
		}
		run, routes := itemRoutes(c, after)
		made := -1
		for i, l := range landings {
			if !used[i] && strings.Contains(l.answer, c.ID) {
				made = i
				break
			}
		}
		for i, l := range landings {
			if made >= 0 || run == "" {
				break
			}
			if !used[i] && l.run == run && l.method == "POST" && routes(l.path) {
				made = i
			}
		}
		if made < 0 {
			broken = append(broken, fmt.Sprintf("%s: no approved row of a plan of the task made it", c.What))
			continue
		}
		used[made] = true
	}
	return broken, nil
}

// approvedLanding is a request of a run of the person that landed for an approved row of a plan:
// its run, its method and path, and the answer recorded beside it.
type approvedLanding struct {
	run, method, path, answer string
}

// approvedLandings is every request of the person's runs that landed for an approved row of a
// plan, in the order the runs made them.
func approvedLandings(ctx context.Context, q Querier, person string) ([]approvedLanding, error) {
	rows, err := q.Query(ctx, `
		select l.run_id::text, l.method, l.path, coalesce(l.answer::text, '')
		from going_landings l
		join going_runs r on r.id = l.run_id and lower(r.author_login) = lower($1)
		join going_plan_rows p on p.run_id = l.run_id
		  and (p.result->>'step')::int = l.step and (p.result->>'call')::int = l.call
		where l.phase = 'do' and p.approval is not null
		order by l.landed_at, l.step, l.call, l.request
	`, person)
	if err != nil {
		return nil, fmt.Errorf("eval: read the approved rows' landed requests: %w", err)
	}
	landings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (approvedLanding, error) {
		var l approvedLanding
		err := row.Scan(&l.run, &l.method, &l.path, &l.answer)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("eval: read the approved rows' landed requests: %w", err)
	}
	return landings, nil
}

// itemRoutes is the run that made the item c names, as its author's act names it, and whether a
// request path is one of the routes that make such an item where it is: a comment or a suggestion
// posted on its issue (an issue's document's and an issue's ask's among them) or on its project
// document (that document's ask's among them), and a message posted on its issue.
func itemRoutes(c Change, after *Store) (string, func(path string) bool) {
	var run, kind string
	var owners []string
	if c.Field == FieldMessage {
		m := after.Messages[c.ID]
		run, kind = m.Author.Act.Run, "messages"
		if m.Issue != "" {
			owners = append(owners, "/issues/"+m.Issue+"/")
		}
	} else {
		comment := after.Comments[c.ID]
		run, kind = comment.Author.Act.Run, "comments"
		if comment.Issue != "" {
			owners = append(owners, "/issues/"+comment.Issue+"/")
		}
		if comment.Artifact != "" {
			owners = append(owners, "/artifacts/"+comment.Artifact+"/")
		}
		if ask, ok := after.Asks[comment.Ask]; ok {
			if ask.Issue != "" {
				owners = append(owners, "/issues/"+ask.Issue+"/")
			}
			if ask.Artifact != "" {
				owners = append(owners, "/artifacts/"+ask.Artifact+"/")
			}
		}
	}
	return run, func(path string) bool {
		if !strings.HasSuffix(path, "/"+kind) {
			return false
		}
		for _, owner := range owners {
			if strings.Contains(path+"/", owner) {
				return true
			}
		}
		return false
	}
}
