package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The reads of c-approval-bytes (TestApprovalCoversTheDigestOfOneQuerysRecord) that its steps share:
// the order a record holds its nodes in, the walk a change in the text leaves as it read, the
// historical review a refused gate leaves standing for its own version, the catalog of asks as
// the release before left it, and the migrations that hold a statement on artifact_reviews.

// requireRecordOrder fails unless the record of query holds each node after its parent, where the
// record holds that parent too, and the ranked children of one parent in increasing rank, byte for
// byte, a tie by the lower address: the record reads in the tree's own order (R14).
func requireRecordOrder(t *testing.T, query string, record docs.QueryRecord) {
	t.Helper()
	at := map[string]int{}
	for index, node := range record.Nodes {
		at[node.Address()] = index
	}
	ranked := map[string][]docs.TreeNode{}
	var parents []string
	for index, node := range record.Nodes {
		parent := node.Parent.Kind + ":" + node.Parent.ID
		if held, holds := at[parent]; holds && held >= index {
			t.Errorf("the %s record takes %s at %d and its parent %s at %d, want the parent first", query, node.Address(), index, parent, held)
		}
		if !node.Ranked {
			continue
		}
		if _, seen := ranked[parent]; !seen {
			parents = append(parents, parent)
		}
		ranked[parent] = append(ranked[parent], node)
	}
	for _, parent := range parents {
		children := ranked[parent]
		for index := 1; index < len(children); index++ {
			before, after := children[index-1], children[index]
			if before.Rank > after.Rank || before.Rank == after.Rank && before.Address() > after.Address() {
				t.Errorf("under %s the %s record takes %s (rank %q) before %s (rank %q), want increasing rank, byte for byte",
					parent, query, before.Address(), before.Rank, after.Address(), after.Rank)
			}
		}
	}
}

// walkPlace is a node of a walk as a change that moves no node must leave it: its address, the
// address of its parent and its rank.
func walkPlace(node docs.TreeNode) string {
	return node.Address() + " under " + node.Parent.Kind + ":" + node.Parent.ID + " at " + strconv.Quote(node.Rank)
}

// requireWalkKept fails, naming what, unless the walk after reads every node the walk before
// read, in the order it read them, each under its parent at its rank. A node the change wrote anew
// - the version node an edit adds - stands in the walk after alone; a node before that the walk
// after lacks fails unless gone, where it is not nil, answers that the change rewrote it.
func requireWalkKept(t *testing.T, what string, before, after docs.IssueTree, gone func(docs.TreeNode) bool) {
	t.Helper()
	stands := map[string]bool{}
	for _, node := range after.Nodes {
		stands[node.Address()] = true
	}
	held := map[string]bool{}
	var was []string
	for _, node := range before.Nodes {
		held[node.Address()] = true
		if stands[node.Address()] {
			was = append(was, walkPlace(node))
		} else if gone == nil || !gone(node) {
			t.Errorf("%s: the walk of %s no longer takes %s, which it read before", what, before.Issue, walkPlace(node))
		}
	}
	var now []string
	for _, node := range after.Nodes {
		if held[node.Address()] {
			now = append(now, walkPlace(node))
		}
	}
	for index := range max(len(was), len(now)) {
		if index >= len(was) || index >= len(now) || was[index] != now[index] {
			read, reads := "nothing", "nothing"
			if index < len(was) {
				read = was[index]
			}
			if index < len(now) {
				reads = now[index]
			}
			t.Errorf("%s: at %d the walk of %s reads %s, where it read %s before: want every node in the order it read before, each under its parent at its rank",
				what, index, before.Issue, reads, read)
			return
		}
	}
}

// requireStale is requireGate refusing the query document at every place that decides it, and
// the historical review beside it at the same time: the document's newest review is still the
// person's own approval, of a version before the newest, and it still pins the digest its own
// version answers, so it reads approved for that version while the gates refuse the newest.
func requireStale(t *testing.T, rig *digestRig, key, step string, document model.AskApprovalDocument, when string) {
	t.Helper()
	requireGate(t, rig, key, step, document, false, when)
	newest := newestVersionOf(t, rig.database, document.ArtifactID)
	reviews := listReviews(t, rig.handler, document.ArtifactID)
	if len(reviews) == 0 || reviews[0].State != "approved" || reviews[0].CarriedFromVersion != nil || reviews[0].Version >= newest {
		t.Errorf("%s: the reviews of %s read %s at version %d, want the person's own approval of an earlier version newest and still approved", when, document.ArtifactID, reviewsRead(reviews), newest)
		return
	}
	if own := versionDigestOf(t, rig.database, document.ArtifactID, reviews[0].Version); reviews[0].ContentSHA256 == nil || *reviews[0].ContentSHA256 != own {
		t.Errorf("%s: the historical review %s of version %d of %s pins %s, want the digest its own version answers, %s: it no longer reads approved for its own version",
			when, reviews[0].ID, reviews[0].Version, document.ArtifactID, valueOrNone(reviews[0].ContentSHA256), own)
	}
}

// reviewsRead is reviews, newest first, as a failure prints them: each one's id, version and
// state, the digest it pins and the version it was carried from, by value.
func reviewsRead(reviews []model.ArtifactReview) string {
	read := make([]string, len(reviews))
	for index, review := range reviews {
		carried := "none"
		if review.CarriedFromVersion != nil {
			carried = strconv.Itoa(*review.CarriedFromVersion)
		}
		read[index] = fmt.Sprintf("%s at version %d %s pinning %s carried from %s", review.ID, review.Version, review.State, valueOrNone(review.ContentSHA256), carried)
	}
	return "[" + strings.Join(read, "; ") + "]"
}

// approvalText is the artifact read's approval as a failure prints it, each member by value: a
// pointer member prints its value, or none or absent.
func approvalText(read hashGateApproval) string {
	approval := read.Approval
	if approval == nil {
		return "no approval"
	}
	version, owed := "none", "absent"
	if approval.Version != nil {
		version = fmt.Sprint(*approval.Version)
	}
	if approval.DigestOwed != nil {
		owed = fmt.Sprint(*approval.DigestOwed)
	}
	return fmt.Sprintf("{state %s, version %s, latest_version %d, content_sha256 %s, current_sha256 %s, digest_owed %s}",
		approval.State, version, approval.LatestVersion, valueOrNone(approval.ContentSHA256), valueOrNone(approval.CurrentSHA256), owed)
}

// releaseBeforeApprovalKindCheck is the expression of asks_approval_kind_check as the release
// before left it: the check of store/migrations/20261102110100_asks_approval_documents.up.sql, the
// last migration main holds that writes it, byte for byte.
const releaseBeforeApprovalKindCheck = `
    case
        when kind = 'approval' then coalesce(
            approval ->> 'artifact_id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                and jsonb_typeof(approval -> 'name') = 'string'
                and case
                    when jsonb_typeof(approval -> 'version') = 'number'
                        and approval ->> 'version' ~ '^[1-9][0-9]{0,9}$'
                        and jsonb_typeof(approval -> 'requested_version') = 'number'
                        and approval ->> 'requested_version' ~ '^[1-9][0-9]{0,9}$'
                    then (approval ->> 'requested_version')::bigint <= (approval ->> 'version')::bigint
                    else false
                end
                and jsonb_typeof(approval -> 'documents') = 'array'
                and jsonb_array_length(approval -> 'documents') >= 1
                and approval -> 'documents' -> 0 ->> 'artifact_id' = approval ->> 'artifact_id'
                and not jsonb_path_exists(approval, '$.documents[*] ? (!(@.artifact_id.type() == "string" && @.artifact_id like_regex "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" && @.name.type() == "string" && @.version.type() == "number" && @.version >= 1 && @.version <= 9999999999 && @.version.floor() == @.version && @.requested_version.type() == "number" && @.requested_version >= 1 && @.requested_version.floor() == @.requested_version && @.requested_version <= @.version))'),
            false
        )
        else approval is null
    end
`

// releaseBeforeNormalizeDocuments is the body of asks_normalize_approval_documents as the same
// migration writes it, between its dollar quotes, byte for byte: the text the catalog keeps as
// the function's source.
const releaseBeforeNormalizeDocuments = `
begin
    if new.kind = 'approval' and new.approval is not null and not (new.approval ? 'documents') then
        new.approval := jsonb_set(new.approval, '{documents}', jsonb_build_array(jsonb_build_object(
            'artifact_id', new.approval -> 'artifact_id',
            'name', new.approval -> 'name',
            'version', new.approval -> 'version',
            'requested_version', new.approval -> 'requested_version')));
    end if;
    return new;
end;
`

// releaseBeforeNormalizeTrigger is the trigger the same migration creates on asks, the table
// left out (%s).
const releaseBeforeNormalizeTrigger = `create trigger asks_normalize_approval_documents
before insert or update of approval on %s
for each row execute function asks_normalize_approval_documents()`

// requireAsksAsTheReleaseBeforeLeftThem reads asks_approval_kind_check, the function
// asks_normalize_approval_documents and its trigger out of the catalog and fails unless each is
// as the release before left it. The check and the trigger are compared as the catalog deparses
// them against a probe the test writes in a transaction it rolls back - a table of asks' columns
// that takes the release before's check and trigger as that migration wrote them - so the two read
// alike exactly where the two definitions are one; the function is compared by its stored source.
func requireAsksAsTheReleaseBeforeLeftThem(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the catalog's probe: %v", err)
	}
	defer tx.Rollback(ctx)
	const probe = "approval_bytes_release_before"
	for _, statement := range []string{
		`create temporary table ` + probe + ` (like asks) on commit drop`,
		`alter table ` + probe + ` add constraint asks_approval_kind_check check (` + releaseBeforeApprovalKindCheck + `)`,
		strings.Replace(releaseBeforeNormalizeTrigger, "%s", probe, 1),
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("write the release before's definition on the probe (%s): %v", statement, err)
		}
	}
	read := func(what, statement string, args ...any) string {
		t.Helper()
		var held string
		if err := tx.QueryRow(ctx, statement, args...).Scan(&held); err != nil {
			t.Fatalf("read %s out of the catalog: %v", what, err)
		}
		return held
	}
	const constraint = `select pg_get_constraintdef(oid) from pg_constraint where conrelid = $1::regclass and conname = 'asks_approval_kind_check'`
	if held, want := read("asks_approval_kind_check", constraint, "asks"), read("the release before's check", constraint, probe); held != want {
		t.Errorf("asks_approval_kind_check reads\n%s\nwant it as the release before left it\n%s", held, want)
	}
	const trigger = `select replace(pg_get_triggerdef(t.oid, true), ' ON ' || t.tgrelid::regclass::text || ' ', ' ON <asks> ') || ' ' || t.tgenabled::text
		from pg_trigger t where t.tgrelid = $1::regclass and t.tgname = 'asks_normalize_approval_documents' and not t.tgisinternal`
	if held, want := read("the trigger asks_normalize_approval_documents", trigger, "asks"), read("the release before's trigger", trigger, probe); held != want {
		t.Errorf("the trigger asks_normalize_approval_documents reads\n%s\nwant it as the release before left it\n%s", held, want)
	}
	if count := read("the functions named asks_normalize_approval_documents", `select count(*)::text from pg_proc where proname = 'asks_normalize_approval_documents'`); count != "1" {
		t.Errorf("the catalog holds %s functions named asks_normalize_approval_documents, want the one the release before left", count)
	}
	held := read("the function asks_normalize_approval_documents", `
		select l.lanname::text || ' ' || p.prorettype::regtype::text || ' (' || pg_get_function_identity_arguments(p.oid) || ')' || p.prosrc
		from pg_proc p join pg_language l on l.oid = p.prolang where p.proname = 'asks_normalize_approval_documents'`)
	if want := "plpgsql trigger ()" + releaseBeforeNormalizeDocuments; held != want {
		t.Errorf("the function asks_normalize_approval_documents reads\n%s\nwant it as the release before left it\n%s", held, want)
	}
}

// releaseBeforeArtifactReviewsMigrations are the migrations the release before holds that state
// anything of artifact_reviews, by file name: main's, at this branch's merge base 89f349d0fa, whose
// statements, their comments taken out, name the table. Every other migration the store carries is
// of the migration set this release adds, or of a release it ships beside, and names none.
var releaseBeforeArtifactReviewsMigrations = []string{
	"0014_artifact_reviews.up.sql",
	"0120_artifact_reviews_event_seq.up.sql",
	"20261102110200_artifact_review_kinds.up.sql",
	"20261105084017_artifact_review_content_sha256.up.sql",
}

// sqlComment is a comment of a migration's SQL, a line's or a block's.
var sqlComment = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)

// namesArtifactReviews matches a statement that names artifact_reviews, and no table whose name
// merely begins so (artifact_review_carries).
var namesArtifactReviews = regexp.MustCompile(`(?i)\bartifact_reviews\b`)

// requireNoMigrationRunsOnArtifactReviews reads every migration the store's migration directory
// holds - each up, down and census file - and fails on any statement, its comments taken out, that
// names artifact_reviews in a migration the release before does not hold
// (releaseBeforeArtifactReviewsMigrations). Each of those must stand and name the table, so the
// scan is shown to find a statement that names it.
func requireNoMigrationRunsOnArtifactReviews(t *testing.T) {
	t.Helper()
	directory := filepath.Join("..", "store", "migrations")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read the migrations under %s: %v", directory, err)
	}
	read := 0
	named := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read the migration %s: %v", entry.Name(), err)
		}
		read++
		releaseBefore := slices.Contains(releaseBeforeArtifactReviewsMigrations, entry.Name())
		for _, statement := range strings.Split(sqlComment.ReplaceAllString(string(body), ""), ";") {
			if !namesArtifactReviews.MatchString(statement) {
				continue
			}
			named[entry.Name()] = true
			if !releaseBefore {
				t.Errorf("the migration %s runs %q against artifact_reviews: the migration set runs no statement against that table",
					entry.Name(), strings.TrimSpace(statement))
			}
		}
	}
	if read == 0 {
		t.Fatalf("no migration stands under %s", directory)
	}
	for _, name := range releaseBeforeArtifactReviewsMigrations {
		if !named[name] {
			t.Errorf("the release before's migration %s stands under %s with no statement naming artifact_reviews, so the scan finds none it should", name, directory)
		}
	}
}
