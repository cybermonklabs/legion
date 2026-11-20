package delivery

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// TestTheIntakeRecordsTheRepositoryIDOfEachPullRequestItCompletes: a pull request the intake
// completes from GitHub's answer records its repository's GitHub id (the answer's base repository)
// under the name the row is stored by and under the name GitHub gives the repository now, which is
// what Delivery holds a pull request that names no issue to (refs.RepositoryClause). acme/renamednow
// #20's answer names its base repository Acme/RenamedNow with id 7007.
//
// Control: complete the pull request without recording the id, and the test fails naming
// acme/renamednow.
func TestTheIntakeRecordsTheRepositoryIDOfEachPullRequestItCompletes(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/renamednow/pulls/20", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 20, "title": "Moved change", "html_url": "https://github.com/acme/renamednow/pull/20",
			"user": map[string]any{"login": "octocat"}, "created_at": "2026-10-01T05:00:00Z", "merged_at": "2026-10-01T07:00:00Z",
			"head": map[string]any{"ref": "work", "sha": "abc", "repo": map[string]any{"full_name": "acme/renamednow"}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"id": 7007, "full_name": "Acme/RenamedNow"}},
		})
	})
	fake.handle("GET /repos/acme/renamednow/pulls/20/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []map[string]any{{"sha": "abc", "commit": map[string]any{"author": map[string]any{"date": "2026-10-01T04:00:00Z"}}}})
	})
	client := fake.newTestClient()
	fetched, err := FetchPullRequest(ctx, client, "acme", "renamednow", 20)
	if err != nil {
		t.Fatalf("FetchPullRequest: %v", err)
	}
	if want := (PullRequestRepository{ID: 7007, FullName: "Acme/RenamedNow"}); fetched.Repository != want {
		t.Fatalf("the fetched pull request's repository = %+v, want %+v", fetched.Repository, want)
	}
	if err := completePullRequest(ctx, pool, client, "acme", "renamednow", "acme/renamednow", 20, fetched, []string{}); err != nil {
		t.Fatalf("complete acme/renamednow#20: %v", err)
	}
	rows, err := pool.Query(ctx, `select repo, github_id from repository_ids order by repo`)
	if err != nil {
		t.Fatalf("read repository_ids: %v", err)
	}
	defer rows.Close()
	recorded := map[string]int64{}
	for rows.Next() {
		var repo string
		var id int64
		if err := rows.Scan(&repo, &id); err != nil {
			t.Fatalf("read repository_ids: %v", err)
		}
		recorded[repo] = id
	}
	if recorded["acme/renamednow"] != 7007 || len(recorded) != 1 {
		t.Errorf("repository_ids = %v, want acme/renamednow recorded with 7007 alone: the stored name and GitHub's name are one", recorded)
	}
}

// TestEachPullRequestEnvelopeLearnsTheIDsOfPrivateProjectsRepositories: each pull-request envelope
// the intake takes learns the GitHub id of each repository a private project's issue resolves to
// that Dispatch holds no id for (refs.LearnRepositoryIDs), so Delivery's reads find the ids they
// turn on. PRIV-1, private, names acme/privx, and PUB-1, public, names acme/pubx: after an
// envelope of acme/widgets#5, acme/privx's id is recorded and acme/pubx's is not. With every project
// public the same envelope asks GitHub for no repository's id (R9).
//
// Control: take the envelope without LearnRepositoryIDs, and the test fails naming acme/privx.
func TestEachPullRequestEnvelopeLearnsTheIDsOfPrivateProjectsRepositories(t *testing.T) {
	for _, row := range []struct {
		name       string
		visibility string
		learned    bool
	}{
		{"with PRIV private", "private", true},
		{"with every project public", "public", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee, visibility) values
				('PUB', 'Public', 'alice', 'public'), ('PRIV', 'Private', 'alice', $1)`, row.visibility); err != nil {
				t.Fatalf("seed the projects: %v", err)
			}
			if _, err := pool.Exec(ctx, `
				insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
					('PUB-1', 'PUB', 1, 'Names acme/pubx', '{"kind":"user","id":"alice"}', 'a', null, 'acme/pubx', 'triage'),
					('PRIV-1', 'PRIV', 1, 'Names acme/privx', '{"kind":"user","id":"alice"}', 'a', null, 'acme/privx', 'triage')
			`); err != nil {
				t.Fatalf("seed the issues: %v", err)
			}
			var asked atomic.Int32
			fake := newFakeGitHub(t)
			fake.handle("GET /repos/acme/privx", func(w http.ResponseWriter, _ *http.Request) {
				asked.Add(1)
				mustEncode(t, w, map[string]any{"full_name": "acme/privx", "id": 7012})
			})
			fake.handle("GET /repos/acme/pubx", func(w http.ResponseWriter, _ *http.Request) {
				t.Errorf("GitHub was asked for acme/pubx, which only a public project's issue names")
				mustEncode(t, w, map[string]any{"full_name": "acme/pubx", "id": 7013})
			})
			intake := newIntake(nil, pool, nil, fake.newTestClient(), nil, nil)
			if _, err := intake.handlePullRequestEnvelope(ctx, DeliverySettings{}, map[string]string{
				"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "5", "title": "Some work",
				"head_ref": "work", "head_sha": "dddddddddddddddddddddddddddddddddddddddd", "base_ref": "main",
				"author": "octocat", "url": "https://github.com/acme/widgets/pull/5",
			}); err != nil {
				t.Fatalf("take the envelope: %v", err)
			}
			var recorded int
			if err := pool.QueryRow(ctx, `select count(*) from repository_ids where repo = 'acme/privx' and github_id = 7012`).Scan(&recorded); err != nil {
				t.Fatalf("read repository_ids: %v", err)
			}
			if got := recorded == 1; got != row.learned || (asked.Load() > 0) != row.learned {
				t.Errorf("acme/privx's id recorded: %t, GitHub asked %d times; want recorded and asked: %t", got, asked.Load(), row.learned)
			}
		})
	}
}
