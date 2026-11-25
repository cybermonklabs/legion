package api

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// m19ControlFunctions are functions of the control's own. The first reads an issue under no
// clause: the census reads the body of every function a statement calls as it reads the
// statement, so a read that hides its unbound read in a function is named all the same. The
// second reads an issue by a local its argument is assigned to: the body is bound where the
// call's argument is, and not where it is not.
var m19ControlFunctions = []string{`create function m19_unbound_issue_titles(p_key text) returns text
language sql stable as $$
	select string_agg(child.title, ', ') from issues child where child.parent_key = p_key
$$`, `create function m19_issue_title_of(p_owner text) returns text
language plpgsql stable as $$
declare
	v_key text := split_part(p_owner, '#', 1);
begin
	return (select owned.title from issues owned where owned.key = v_key);
end
$$`}

// TestM19TheCensusNamesEveryUnboundProjectReadOfAStatement is the control of
// TestEveryProjectReadBindsTheFragmentPerStatement: over statements written here, each in the
// shape a project read sends, the census must name every project-owned table the statement reads
// that no clause on that table's own alias binds, and must name none of those a clause or the
// table's own owner column does bind. The rows that read a table through a comma join, through a
// join on no key, through a reference that is not the row's owner (issue_links.target_key) and
// through a function's body are the reads a census that looks for a marker anywhere passes.
func TestM19TheCensusNamesEveryUnboundProjectReadOfAStatement(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	for _, function := range m19ControlFunctions {
		if _, err := database.Pool.Exec(context.Background(), function); err != nil {
			t.Fatalf("create the control's function: %v", err)
		}
	}
	schema := m19ProjectSchema(t, database)
	viewer := "$2"
	for _, row := range []struct {
		name      string
		statement string
		want      []string
		// checked are the parameters the statement's transaction binds (readsKeyedByTheirTransaction).
		checked []int
	}{{
		name: "a comma join reads a comment's mentions and binds them on nothing",
		statement: `select m.target from comments c, comment_mentions m
			where c.id = $1 and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: []string{"comment_mentions as m"},
	}, {
		name: "a comma join bound through the mention's own comment",
		statement: `select m.target from comments c, comment_mentions m
			where c.id = $1 and m.comment_id = c.id and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: nil,
	}, {
		name:      "the followers of an ask, under no clause at all",
		statement: `select f.session_id from ask_followers f join asks a on a.id = f.ask_id where a.issue_key = $1`,
		want:      []string{"ask_followers as f", "asks as a"},
	}, {
		name: "the issue a link waits on is not the link's own",
		statement: `select l.target_key, t.title from issue_links l join issues t on t.key = l.target_key
			where ` + access.IssueProjectClause("l.issue_key", viewer),
		want: []string{"issues as t"},
	}, {
		name: "a message's deliveries, bound through the message they are of",
		statement: `select d.session_id from messages m join message_deliveries d on d.message_id = m.id
			where m.id = $1 and ` + access.MessageClause("m", viewer),
		want: nil,
	}, {
		name: "a message's deliveries, joined by no key",
		statement: `select d.session_id from messages m, message_deliveries d
			where m.id = $1 and ` + access.MessageClause("m", viewer),
		want: []string{"message_deliveries as d"},
	}, {
		name:      "a function's body is read as the statement that calls it",
		statement: `select m19_unbound_issue_titles($1)`,
		want:      []string{"issues as child in m19_unbound_issue_titles"},
	}, {
		name: "a function the rule itself calls reads for the rule",
		statement: `select c.id from comments c where c.id = $1
			and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer) + `
			and ` + access.SessionClause("c.author->>'id'", viewer),
		want: nil,
	}, {
		name: "a left join binds the side its condition can drop",
		statement: `select i.key, c.id from issues i left join comments c on c.issue_key = i.key
			where ` + access.ProjectClause("i.project_key", viewer),
		want: nil,
	}, {
		name: "a left join on no key binds nothing it joins",
		statement: `select c.id, m.target from comments c left join comment_mentions m on true
			where ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: []string{"comment_mentions as m"},
	}, {
		name: "a key = any of an array binds the row by the array, never the array by the row",
		statement: `select m.target from comments c, comment_mentions m
			where c.id = any($1::uuid[]) and m.comment_id = any($1::uuid[])
			and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: []string{"comment_mentions as m"},
	}, {
		name:      "a row keyed by a parameter nothing binds",
		statement: `select m.target from comment_mentions m where m.comment_id = $1`,
		want:      []string{"comment_mentions as m"},
	}, {
		name:      "a row keyed by a parameter its transaction binds",
		statement: `select m.target from comment_mentions m where m.comment_id = $1`,
		checked:   []int{1},
		want:      nil,
	}, {
		name: "a row keyed by a parameter an exists of its own binds",
		statement: `select m.target from comment_mentions m where m.comment_id = $1
			and exists (select 1 from comments c where c.id = $1
				and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer) + `)`,
		want: nil,
	}, {
		name: "an or binds a row only where every arm does",
		statement: `select m.target from comments c, comment_mentions m
			where c.id = $1 and (m.comment_id = c.id or m.target = 'x')
			and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: []string{"comment_mentions as m"},
	}, {
		name: "an or whose every arm binds the row",
		statement: `select m.target from comments c, comment_mentions m
			where c.id = $1 and (m.comment_id = c.id and m.target = 'a' or m.comment_id = c.id and m.target = 'b')
			and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: nil,
	}, {
		name: "an ask held by the coalesce of its owners' projects to a project the reader sees",
		statement: `select k.id from asks k left join issues i on i.key = k.issue_key left join artifacts a on a.id = k.artifact_id
			where coalesce(i.project_key, a.project_key) = $1 and ` + access.ProjectClause("$1", viewer),
		want: nil,
	}, {
		name: "an ask held by the coalesce of its owners' projects to a project nothing binds",
		statement: `select k.id from asks k left join issues i on i.key = k.issue_key left join artifacts a on a.id = k.artifact_id
			where coalesce(i.project_key, a.project_key) = $1`,
		want: []string{"artifacts as a", "asks as k", "issues as i"},
	}, {
		name: "a version keyed by a scalar subquery that binds its document",
		statement: `select v.number from artifact_versions v
			where v.artifact_id = (select a.id from artifacts a where a.id = $1 and ` + access.DocumentClause("a", viewer) + `)`,
		want: nil,
	}, {
		name: "a version keyed by a scalar subquery that binds nothing",
		statement: `select v.number from artifact_versions v
			where v.artifact_id = (select a.id from artifacts a where a.id = $1)`,
		want: []string{"artifact_versions as v", "artifacts as a"},
	}, {
		name: "a function's local, assigned from an argument a clause binds",
		statement: `select m19_issue_title_of(i.key) from issues i
			where i.key = $1 and ` + access.ProjectClause("i.project_key", viewer),
		want: nil,
	}, {
		name:      "a function's local, assigned from an argument nothing binds",
		statement: `select m19_issue_title_of($1)`,
		want:      []string{"issues as owned in m19_issue_title_of"},
	}, {
		name: "a tuple of two nodes of no project is no project's row",
		statement: `select t.to_id from priority_tuples t
			where t.from_kind = '` + graphstore.KindPerson + `' and t.from_id = $1 and t.to_kind = '` + graphstore.KindValue + `'`,
		want: nil,
	}, {
		name: "a tuple whose other node may be of a project",
		statement: `select t.to_id from priority_tuples t
			where t.from_kind = '` + graphstore.KindPerson + `' and t.from_id = $1`,
		want: []string{"priority_tuples as t"},
	}, {
		name: "a tuple bound on the node it points at, and not on its own",
		statement: `select t.from_id from workflow_tuples t
			where t.from_kind = 'step' and t.to_kind = 'issue' and ` + refs.VisibleKind("issue", "t.to_id", viewer),
		want: []string{"workflow_tuples as t"},
	}, {
		name: "a membership tuple bound by what it is a member of",
		statement: `select t.from_id from workflow_tuples t
			where t.kind = '` + graphstore.TuplePartOf + `' and t.from_kind = 'step' and t.to_kind = 'issue'
			and ` + refs.VisibleKind("issue", "t.to_id", viewer),
		want: nil,
	}, {
		name: "a view of tuples bound on the node it points at",
		statement: `select g.to_id from graph_edges g
			where g.from_kind = '` + graphstore.KindPerson + `' and g.from_id = $1 and g.to_kind = 'issue'
			and ` + refs.VisibleKind("issue", "g.to_id", viewer),
		want: nil,
	}, {
		name: "a view of tuples bound on nothing",
		statement: `select g.to_id from graph_edges g
			where g.from_kind = '` + graphstore.KindPerson + `' and g.from_id = $1 and g.to_kind = 'issue'`,
		want: []string{"graph_edges as g"},
	}, {
		name: "a recursive expression's rows bound by induction from a bound first arm",
		statement: `with recursive sub as (
				select i.key, i.project_key from issues i where i.key = $1 and ` + access.ProjectClause("i.project_key", viewer) + `
				union all
				select c.key, c.project_key from issues c join sub s on c.project_key = s.project_key
			) select key from sub`,
		want: nil,
	}, {
		name: "a recursive expression's rows from a first arm bound on nothing",
		statement: `with recursive sub as (
				select i.key, i.project_key from issues i where i.key = $1
				union all
				select c.key, c.project_key from issues c join sub s on c.project_key = s.project_key
			) select key from sub`,
		want: []string{"issues as c", "issues as i"},
	}, {
		name: "a link cited from a block of a bound document",
		statement: `select r.to_id from refs r
			join block_refs b on b.from_node = r.from_id
			join artifacts a on a.id = b.artifact_id
			where r.from_kind = 'block' and r.to_kind = '` + graphstore.KindPerson + `'
			and ` + access.DocumentClause("a", viewer),
		want: nil,
	}, {
		name: "a link cited from a block of a document bound on nothing",
		statement: `select r.to_id from refs r
			join block_refs b on b.from_node = r.from_id
			join artifacts a on a.id = b.artifact_id
			where r.from_kind = 'block' and r.to_kind = '` + graphstore.KindPerson + `'`,
		want: []string{"artifacts as a", "block_refs as b", "refs as r"},
	}, {
		name: "a tuple whose other node is null is no project's row",
		statement: `select t.from_id from priority_tuples t
			where t.from_kind = '` + graphstore.KindPerson + `' and t.from_id = $1 and t.to_kind is null`,
		want: nil,
	}, {
		name: "a tuple whose other node is not null may be a project's row",
		statement: `select t.from_id from priority_tuples t
			where t.from_kind = '` + graphstore.KindPerson + `' and t.from_id = $1 and t.to_kind is not null`,
		want: []string{"priority_tuples as t"},
	}, {
		name: "a row keyed by a union of rows every arm binds",
		statement: `select m.target from comment_mentions m join (
				select c.id from comments c where ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer) + `
				union all
				select d.id from comments d where ` + access.ItemOwnerClause("d.issue_key", "d.artifact_id", viewer) + `
			) u on m.comment_id = u.id`,
		want: nil,
	}, {
		name: "a row keyed by a union of rows one arm binds on nothing",
		statement: `select m.target from comment_mentions m join (
				select c.id from comments c where ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer) + `
				union all
				select d.id from comments d
			) u on m.comment_id = u.id`,
		want: []string{"comment_mentions as m", "comments as d"},
	}, {
		name: "a row named by an expression of its own key",
		statement: `select m.target from comments c, comment_mentions m
			where c.id = $1 and c.id = split_part(m.comment_id::text, '#', 1)::uuid
			and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: nil,
	}, {
		name: "a row named by an expression of a column that names no row",
		statement: `select m.target from comments c, comment_mentions m
			where c.id = $1 and c.id = split_part(m.target, '#', 1)::uuid
			and ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: []string{"comment_mentions as m"},
	}, {
		name: "a thread's state bound through its bound root",
		statement: `select p.thread_id from user_threads p join comments c on c.id = p.thread_id
			where ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: nil,
	}, {
		name: "a thread's state bound through its bound newest reply",
		statement: `select p.thread_id from user_threads p join comments c on c.id = p.last_reply_id
			where ` + access.ItemOwnerClause("c.issue_key", "c.artifact_id", viewer),
		want: nil,
	}, {
		name:      "a thread's state through a root bound on nothing",
		statement: `select p.thread_id from user_threads p join comments c on c.id = p.thread_id`,
		want:      []string{"comments as c", "user_threads as p"},
	}, {
		name: "a broadcast's copy bound by the broadcast it is of",
		statement: `select m.id from messages m
			where m.broadcast_id = $1 and ` + access.BroadcastClause("m.broadcast_id", viewer),
		want: nil,
	}, {
		name:      "a broadcast's copy bound on nothing",
		statement: `select m.id from messages m where m.broadcast_id = $1`,
		want:      []string{"messages as m"},
	}} {
		t.Run(row.name, func(t *testing.T) {
			found, _, err := m19UnboundReads(schema, row.statement, row.checked...)
			if err != nil {
				t.Fatalf("the census could not read the statement: %v\n%s", err, row.statement)
			}
			named := make([]string, 0, len(found))
			for _, read := range found {
				named = append(named, read.String())
			}
			slices.Sort(named)
			if !slices.Equal(named, row.want) {
				t.Errorf("the census names %v; want %v\n%s", named, row.want, oneLine(row.statement))
			}
		})
	}

	// The function the rule itself calls, called by a statement of its own: its body is read, and
	// the session pairs it reads outside any clause of the access package are named.
	t.Run("the rule's own function, called outside the rule", func(t *testing.T) {
		found, read, err := m19UnboundReads(schema, `select session_clause_visible($1, $2::jsonb, true)`)
		if err != nil {
			t.Fatalf("the census could not read the statement: %v", err)
		}
		if !slices.Contains(read, "session_clause_visible") {
			t.Errorf("the census read the bodies of %v; want session_clause_visible among them", read)
		}
		named := make([]string, 0, len(found))
		for _, unbound := range found {
			named = append(named, unbound.String())
		}
		slices.Sort(named)
		if !slices.Contains(named, "session_projects as unseen in session_clause_visible") {
			t.Errorf("the census names %v; want the session pairs session_clause_visible reads among them", named)
		}
	})
}

// TestM19EveryKindOfNoProjectIsTheRegistrys: the node kinds the census reads as no project's are
// the graph's registry's own kinds of no project, and the few kinds m19NotRegistryKinds names
// that the registry holds no rule for. An entry the registry does not hold, and is not named as
// one it does not, is a kind the census excuses by hand.
func TestM19EveryKindOfNoProjectIsTheRegistrys(t *testing.T) {
	t.Parallel()
	registry := map[string]bool{}
	for _, kind := range refs.VisibilityKinds() {
		registry[kind] = true
	}
	if len(registry) == 0 {
		t.Fatal("the graph's registry holds no kind, so the census reads no kind of it")
	}
	for kind, why := range m19KindsOfNoProject {
		outside := slices.Contains(m19NotRegistryKinds, kind)
		switch {
		case registry[kind] && outside:
			t.Errorf("m19NotRegistryKinds names %q, which the graph's registry holds a rule for", kind)
		case !registry[kind] && !outside:
			t.Errorf("m19KindsOfNoProject names %q (%s), which the graph's registry holds no rule for: name it in m19NotRegistryKinds or take it off", kind, why)
		}
	}
	for _, kind := range m19NotRegistryKinds {
		if _, held := m19KindsOfNoProject[kind]; !held {
			t.Errorf("m19NotRegistryKinds names %q, which m19KindsOfNoProject does not", kind)
		}
	}
}
