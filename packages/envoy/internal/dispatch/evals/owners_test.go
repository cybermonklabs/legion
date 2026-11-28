package evals

import (
	"slices"
	"testing"
)

// TestASourceKeyIsMadeAndReadBackWithItsRepository (dispatch://INFRA-978, R12): a deploy run that
// ships no pull request is a row of its repository's source key, made by OwnerKey with the one list
// ">~" (sourceKey), ':' + repo + '||>~', the repository spelled as given, and no other key is it:
// not the plain key, one whose list names a project, or one with a sharer. parseOwner reads it back
// to that repository, no sharer and that list. Stored, by recordOwners as a rebuild stages it and by
// the eval_days change trigger as a write records it, eval_day_owners holds its repository, no
// sharer and the lists '{>~}', which eval_project_owners reads it by.
func TestASourceKeyIsMadeAndReadBackWithItsRepository(t *testing.T) {
	t.Parallel()
	const repo, written = "Acme/Deploy-App", "acme/written"
	key := sourceKey(repo)
	if want := ":Acme/Deploy-App||>~"; key != want {
		t.Fatalf("the source key of %s is %q, want %q", repo, key, want)
	}
	if made := OwnerKey(repo, nil, []attributionList{{terminal: sourceTerminal}}); made != key {
		t.Errorf("OwnerKey with the source list makes %q, and sourceKey %q", made, key)
	}
	gotRepo, sharers, lists, ok := parseOwner(key)
	if !ok || gotRepo != repo || len(sharers) != 0 || !slices.Equal(lists, []string{">~"}) {
		t.Errorf("parseOwner(%q) = %q, %v, %v, %t; want %q, no sharer, [>~], true", key, gotRepo, sharers, lists, ok, repo)
	}
	for _, other := range []string{
		OwnerKey(repo, nil, nil),
		OwnerKey(repo, nil, []attributionList{{terminal: "ALPHA"}}),
		OwnerKey(repo, []string{"ALPHA"}, []attributionList{{terminal: sourceTerminal}}),
	} {
		if other == key {
			t.Errorf("%q is the source key too", other)
		}
	}

	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := recordOwners(ctx, tx, []string{key}); err != nil {
		t.Fatalf("record %s: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, via_repository, built_by)
		values ($1, $1 || '/deploy-frequency', $2, 'deploys', '', date '2026-11-03', true, 1, 1, 1, 'test')
	`, sourceKey(written), written); err != nil {
		t.Fatalf("write a day row of %s: %v", sourceKey(written), err)
	}
	for owner, want := range map[string]string{key: repo, sourceKey(written): written} {
		var stored string
		var storedSharers, storedLists []string
		if err := pool.QueryRow(ctx, `select repo, sharers, lists from eval_day_owners where owner = $1`, owner).Scan(&stored, &storedSharers, &storedLists); err != nil {
			t.Fatalf("read %s back from eval_day_owners: %v", owner, err)
		}
		if stored != want || len(storedSharers) != 0 || !slices.Equal(storedLists, []string{">~"}) {
			t.Errorf("eval_day_owners holds %s as %q, sharers %v, lists %v; want %q, none, [>~]", owner, stored, storedSharers, storedLists, want)
		}
	}
}
