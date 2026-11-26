package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// This file is c-approval-hash of dispatch://INFRA-1098 (R28, the root's
// dispatch://INFRA-1098/spec#b-r-approval-hash): the record an approval of a Verification query
// covers, the seven changes that move its digest and the two that do not, and the two answers an
// approval gives apart, the current one and the historical one. It reuses the digest rig of
// approval_digest_test.go, which builds the same record for INFRA-1253's own check, so the two
// read one issue the same way.

// queryDigestChecks is the Verification document of the rig's issue, CORE-1: c1 of r1 in the words
// first, c2 of r1, and, where third is set, c3 of r2.
func queryDigestChecks(first string, third bool) string {
	checks := digestChecks(first, "The list reads an ask's urgency back.")
	if third {
		checks += "\n:::check{#c3 serves=\"dispatch://CORE-1/spec#b-r2\" surface=\"cli\" run=\"go test ./...\"}\nThe list reads an ask's size.\n:::\n"
	}
	return checks
}

// queryDigestRig is the digest rig with the issue's Verification document beside its spec: the
// document's id and ref_key, which name its query node and its query versions.
type queryDigestRig struct {
	*digestRig
	verification, verificationRef string
}

// record is the verification record of the rig's issue at the head.
func (r *queryDigestRig) record(t *testing.T) docs.QueryRecord {
	t.Helper()
	return r.digestRig.record(t, r.issue.Key, graphstore.QueryVerification)
}

// upload replaces the issue's Verification document with checks, as an agent's upload does.
func (r *queryDigestRig) upload(t *testing.T, checks string) {
	t.Helper()
	uploadDocument(t, r.handler, "/api/v1/issues/"+r.issue.Key, "verification.md", checks)
}

// result records a result of check, a block id of the Verification document, at commit, and
// returns its id.
func (r *queryDigestRig) result(t *testing.T, check, verdict, commit string) string {
	t.Helper()
	recorded := recordResult(t, r.handler, r.issue.Key, resultBody(check, verdict, commit, map[string]any{"surface": "cli"}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a %s result of %s at %s: status=%d body=%s", verdict, check, commit, recorded.Code, recorded.Body.String())
	}
	return decodeBody[recordedResult](t, recorded).Result.ID
}

// resultNodeAddress is the store address of the result node id.
func resultNodeAddress(id string) string { return graphstore.KindResult + ":" + id }

// recordNode is one node of a query's record read back field by field: its address, its type,
// its parent's address and its rank; each of its values, a name and a value, in the order the
// record holds them; its body text; and each relation out of it, a kind and a far end's address,
// in the order the record holds them.
type recordNode struct {
	address, typ, parent, rank string
	values                     [][2]string
	text                       string
	relations                  [][2]string
}

// lengthFields splits fields, written each as its byte length, one space, then its bytes, into
// those fields. It refuses bytes that are not such a sequence to its end.
func lengthFields(fields []byte) ([]string, error) {
	var split []string
	for len(fields) > 0 {
		space := bytes.IndexByte(fields, ' ')
		if space <= 0 {
			return nil, fmt.Errorf("a field opens with %q, not a length and one space", fields[:min(len(fields), 16)])
		}
		length, err := strconv.Atoi(string(fields[:space]))
		if err != nil || length < 0 || space+1+length > len(fields) {
			return nil, fmt.Errorf("a field's length %q does not fit the %d bytes after it", fields[:space], len(fields)-space-1)
		}
		split = append(split, string(fields[space+1:space+1+length]))
		fields = fields[space+1+length:]
	}
	return split, nil
}

// pairs splits a nested field into its pairs of fields: a values field into names and values, a
// relations field into kinds and far ends.
func pairs(field string) ([][2]string, error) {
	split, err := lengthFields([]byte(field))
	if err != nil {
		return nil, err
	}
	if len(split)%2 != 0 {
		return nil, fmt.Errorf("the nested field holds %d fields, not pairs", len(split))
	}
	held := make([][2]string, 0, len(split)/2)
	for index := 0; index < len(split); index += 2 {
		held = append(held, [2]string{split[index], split[index+1]})
	}
	return held, nil
}

// readRecord reads a record back into its nodes, seven fields each, and fails t where it is not
// that.
func readRecord(t *testing.T, record docs.QueryRecord) []recordNode {
	t.Helper()
	fields, err := lengthFields(record.Record)
	if err != nil {
		t.Fatalf("the %s record is not a sequence of length-prefixed fields: %v\n%q", record.Query, err, record.Record)
	}
	if len(fields) != 7*len(record.Nodes) {
		t.Fatalf("the %s record holds %d fields for %d nodes, want seven a node", record.Query, len(fields), len(record.Nodes))
	}
	nodes := make([]recordNode, 0, len(record.Nodes))
	for index := 0; index < len(fields); index += 7 {
		values, err := pairs(fields[index+4])
		if err != nil {
			t.Fatalf("the values field of %s: %v", fields[index], err)
		}
		relations, err := pairs(fields[index+6])
		if err != nil {
			t.Fatalf("the relations field of %s: %v", fields[index], err)
		}
		nodes = append(nodes, recordNode{
			address: fields[index], typ: fields[index+1], parent: fields[index+2], rank: fields[index+3],
			values: values, text: fields[index+5], relations: relations,
		})
	}
	return nodes
}

// joinedWithSeparator is the record as an encoding that joins its fields with a newline would
// write it: every field, the nested values and relations flattened, one newline between two. Two
// stores whose records join to one text are two stores that encoding cannot tell apart.
func joinedWithSeparator(nodes []recordNode) string {
	var fields []string
	for _, node := range nodes {
		fields = append(fields, node.address, node.typ, node.parent, node.rank)
		for _, value := range node.values {
			fields = append(fields, value[0], value[1])
		}
		fields = append(fields, node.text)
		for _, relation := range node.relations {
			fields = append(fields, relation[0], relation[1])
		}
	}
	return strings.Join(fields, "\n")
}

// splitAddress is the kind and the id of a store address, `<kind>:<id>`.
func splitAddress(t *testing.T, address string) (string, string) {
	t.Helper()
	kind, id, found := strings.Cut(address, ":")
	if !found {
		t.Fatalf("%q is no store address", address)
	}
	return kind, id
}

// latestReview is the newest review of the document artifactID, in the order their events
// committed (model.ReviewOrder), and false where it holds none.
func latestReview(t *testing.T, r *queryDigestRig, artifactID string) (model.ArtifactReview, bool) {
	t.Helper()
	review, err := docs.ScanReview(r.database.Pool.QueryRow(context.Background(),
		`select `+docs.ReviewColumns+` from artifact_reviews where artifact_id = $1 order by `+model.ReviewOrder+` limit 1`, artifactID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ArtifactReview{}, false
	}
	if err != nil {
		t.Fatalf("read the newest review of %s: %v", artifactID, err)
	}
	return review, true
}

// recordedVersionRender is what version_renders records of version number of the document: the
// digest of the record the version was written with, and the cut that record was read at, and
// false where the version holds no row.
func recordedVersionRender(t *testing.T, r *queryDigestRig, artifactID string, number int) (string, refs.TreeCut, bool) {
	t.Helper()
	var digest string
	var cut refs.TreeCut
	err := r.database.Pool.QueryRow(context.Background(),
		`select record_sha256, read_seq, at from version_renders where artifact_id = $1 and number = $2`, artifactID, number,
	).Scan(&digest, &cut.ReadSeq, &cut.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refs.TreeCut{}, false
	}
	if err != nil {
		t.Fatalf("read the version_renders row of version %d of %s: %v", number, artifactID, err)
	}
	return digest, cut, true
}

// latestNumber is the number of the newest version of the document.
func latestNumber(t *testing.T, r *queryDigestRig, artifactID string) int {
	t.Helper()
	var number int
	if err := r.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(number), 0) from artifact_versions where artifact_id = $1`, artifactID).Scan(&number); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return number
}

// approvalMigration is the migration that defines asks_approval_kind_check, the function
// asks_normalize_approval_documents and the trigger of that name as they stand: none of the three
// is on the root's list of what the work drops (dispatch://INFRA-1253/spec#b-r-approval-bytes), so
// each must still be what this file made it.
const approvalMigration = "20261102110100_asks_approval_documents.up.sql"

// migrationsDirectory is the store's migrations, read from this package's directory.
var migrationsDirectory = filepath.Join("..", "store", "migrations")

// statementFrom is the text of migration from its first occurrence of opening to the first
// occurrence of closing after it, closing included.
func statementFrom(t *testing.T, migration, opening, closing string) string {
	t.Helper()
	start := strings.Index(migration, opening)
	if start < 0 {
		t.Fatalf("%s holds no %q", approvalMigration, opening)
	}
	end := strings.Index(migration[start:], closing)
	if end < 0 {
		t.Fatalf("%s holds no %q after %q", approvalMigration, closing, opening)
	}
	return migration[start : start+end+len(closing)]
}

// TestAnApprovalCoversTheVerificationQueryDigest reads the verification query of the seeded issue
// CORE-1 and the record it hashes (c-approval-hash). The query returns the issue's checks and the
// results under them, in the walk of the issue's tree, a parent before its children and siblings
// in the order the store holds; each node writes its id, its supertag, its parent's id and its
// rank, then each of its values in attribute order, the text of its body and each tuple out of
// it, every field its length in bytes before its bytes. An unchanged tree hashes to one digest
// twice; a check whose body holds what a newline-joined encoding splits on reads apart from the
// two fields it imitates. Seven changes move the digest, one at a time: a check's words, an
// attribute of a check, an exchange of two siblings, an added check, a removed check, one check's
// serves retargeted with words, values, nodes and order unchanged, and a result's verdict. A
// change of a node the query does not return, and a tuple from outside into a returned node, leave
// it unmoved.
//
// The approval half: the approval covers that digest at the version it approves, stored in
// artifact_reviews.content_sha256 under its constraint, inserted by its trigger and never moved;
// the request it answers carries one approves tuple to each query version node, whose projection
// the asks row's approval keeps; asks_approval_kind_check, asks_normalize_approval_documents and
// its trigger stand as their migration made them. A retarget leaves the current query not approved
// and the gate asking again, while the historical approval still answers for its revision; a
// result appended under unchanged checks carries the approval to its version, recording the
// version approved and the result. A version written before the cutover still answers the digest
// of its stored markdown.
//
// Each clause is a step of its own, so a clause this tree does not hold yet fails by name and the
// clauses after it still run.
func TestAnApprovalCoversTheVerificationQueryDigest(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	var collated bool
	if err := database.Pool.QueryRow(ctx, `select 'a' < 'B'`).Scan(&collated); err != nil {
		t.Fatalf("read the database's collation of a and B: %v", err)
	}
	if !collated {
		t.Fatal("this database orders 'B' before 'a', so a record ordered byte for byte reads as one that forgot to: run this test where select 'a' < 'B' is true")
	}
	createReferenceAPIProject(t, handler, "CORE")
	rig := &queryDigestRig{digestRig: &digestRig{handler: handler, database: database}}
	rig.issue = createDigestIssue(t, handler, database, "CORE", "The digest's issue", digestSpec, false)
	if rig.issue.Key != "CORE-1" {
		t.Fatalf("the rig's issue is %s, want CORE-1, which its documents' references name", rig.issue.Key)
	}
	rig.spec = readDecisionDocument(t, database, rig.issue.Key, "spec")
	uploaded := uploadDocument(t, handler, "/api/v1/issues/"+rig.issue.Key, "verification.md", queryDigestChecks("The list reads urgent asks first.", false))
	rig.verification = uploaded.Artifact.ID
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, rig.verification).Scan(&rig.verificationRef); err != nil {
		t.Fatalf("read the ref_key of the Verification document: %v", err)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+rig.issue.Key, "plan.md", ":::plan_task{#t1 serves=\"dispatch://CORE-1/spec#b-r2\"}\nBuild the order.\n:::\n")

	for _, step := range []struct {
		name string
		run  func(t *testing.T, rig *queryDigestRig)
	}{
		{"the query returns the checks and the results under them in the walk, each node written field by field", queryDigestStepRecord},
		{"an unchanged tree hashes to one digest, and a body imitating a field boundary reads apart", queryDigestStepLengths},
		{"seven changes move the digest, one at a time", queryDigestStepMoves},
		{"a node the query does not return and a tuple into a returned node leave the digest", queryDigestStepUnmoved},
		{"an approval covers the digest, keeps its column, carries approves tuples and leaves the asks row's shape", queryDigestStepApproval},
	} {
		t.Run(step.name, func(t *testing.T) { step.run(t, rig) })
	}
}

func queryDigestStepRecord(t *testing.T, rig *queryDigestRig) {
	ctx := context.Background()
	result := rig.result(t, "c1", "pass", headCommit)
	c1, c2, r1 := rig.block("c1"), rig.block("c2"), rig.block("r1")
	record := rig.record(t)
	// The fixture states the order: c1, the result under it, then c2, c1's sibling after it.
	want := []string{blockAddress(c1), resultNodeAddress(result), blockAddress(c2)}
	if got := walkAddresses(record.Nodes); !slices.Equal(got, want) {
		t.Errorf("the verification query returns %v, want the checks and the results under them in the walk of the tree: %v", got, want)
	}
	requireWalkOrder(t, rig.walk(t, rig.issue.Key))
	nodes := readRecord(t, record)
	for index, node := range nodes {
		walked := record.Nodes[index]
		if node.address != walked.Address() || node.typ != walked.Type || node.parent != walked.Parent.Kind+":"+walked.Parent.ID || node.rank != walked.Rank {
			t.Errorf("node %d of the record writes id %q, supertag %q, parent %q and rank %q, want %q, %q, %q and %q",
				index, node.address, node.typ, node.parent, node.rank, walked.Address(), walked.Type, walked.Parent.Kind+":"+walked.Parent.ID, walked.Rank)
		}
		kind, id := splitAddress(t, node.address)
		names := make([]string, len(node.values))
		for position, value := range node.values {
			names[position] = value[0]
			if value[0] == refs.AttributeRank || value[0] == graphstore.AttributeText {
				t.Errorf("%s writes its %s among its values; the rank and the text are fields of their own, written once", node.address, value[0])
			}
			held, stated, err := graphstore.CurrentValue(ctx, rig.database.Pool, kind, id, value[0])
			if err != nil {
				t.Fatalf("read the %s of %s: %v", value[0], node.address, err)
			}
			if !stated || held != value[1] {
				t.Errorf("%s writes its %s as %q, and the store holds %q (stated %t)", node.address, value[0], value[1], held, stated)
			}
		}
		if !sort.StringsAreSorted(names) {
			t.Errorf("%s writes its values in the order %v, want attribute order", node.address, names)
		}
		text, _, err := graphstore.CurrentValue(ctx, rig.database.Pool, kind, id, graphstore.AttributeText)
		if err != nil {
			t.Fatalf("read the text of %s: %v", node.address, err)
		}
		if node.text != text {
			t.Errorf("%s writes the body %q, and the store holds the text %q", node.address, node.text, text)
		}
		ordered := slices.IsSortedFunc(node.relations, func(a, b [2]string) int {
			if a[0] != b[0] {
				return strings.Compare(a[0], b[0])
			}
			return strings.Compare(a[1], b[1])
		})
		if !ordered {
			t.Errorf("%s writes its relations in the order %v, want by kind and then far end", node.address, node.relations)
		}
		parentHeld := false
		for _, relation := range node.relations {
			farKind, farID := splitAddress(t, relation[1])
			held, err := graphstore.CurrentOut(ctx, rig.database.Pool, kind, id, []string{relation[0]})
			if err != nil {
				t.Fatalf("read the %s tuples out of %s: %v", relation[0], node.address, err)
			}
			if !slices.ContainsFunc(held, func(tuple graphstore.Tuple) bool { return tuple.ToKind == farKind && tuple.ToID == farID }) {
				t.Errorf("%s writes the relation %s to %s, which the store does not hold: %+v", node.address, relation[0], relation[1], held)
			}
			if relation[0] == graphstore.TuplePartOf && relation[1] == node.parent {
				parentHeld = true
			}
		}
		if !parentHeld {
			t.Errorf("%s writes no part_of to its parent %s among its relations %v", node.address, node.parent, node.relations)
		}
	}
	if len(nodes) == 3 {
		if !slices.Contains(nodes[0].relations, [2]string{"serves", blockAddress(r1)}) {
			t.Errorf("c1 writes the relations %v, want its serves to %s among them", nodes[0].relations, blockAddress(r1))
		}
		if !slices.Contains(nodes[1].relations, [2]string{graphstore.TupleResultOf, blockAddress(c1)}) {
			t.Errorf("the result writes the relations %v, want its result_of to %s among them", nodes[1].relations, blockAddress(c1))
		}
	}
}

func queryDigestStepLengths(t *testing.T, rig *queryDigestRig) {
	ctx := context.Background()
	once, twice := rig.record(t), rig.record(t)
	if !bytes.Equal(once.Record, twice.Record) || once.Digest != twice.Digest {
		t.Errorf("an unchanged tree hashes to %s and then %s", once.Digest, twice.Digest)
	}
	// The imitation: c2's body, its last field but one, and its first relation, kind and far end,
	// are three fields in one store; in the other the body holds all three, joined by the newline
	// a separator-joined encoding would split them on, and the relation is gone. Joined by that
	// newline the two records are one text.
	c2, r2 := rig.block("c2"), rig.block("r2")
	text, stated, err := graphstore.CurrentValue(ctx, rig.database.Pool, refs.BlockNodeKind, c2, graphstore.AttributeText)
	if err != nil || !stated {
		t.Fatalf("read the text of %s: %v (stated %t)", c2, err, stated)
	}
	const probe = "aaprobe"
	rig.state(t, blockTuple(c2, probe, refs.BlockNodeKind, r2, true))
	fields := rig.record(t)
	rig.state(t, blockTuple(c2, probe, refs.BlockNodeKind, r2, false),
		blockValue(c2, graphstore.AttributeText, text+"\n"+probe+"\n"+blockAddress(r2)))
	imitated := rig.record(t)
	rig.state(t, blockValue(c2, graphstore.AttributeText, text))
	if joinedWithSeparator(readRecord(t, fields)) != joinedWithSeparator(readRecord(t, imitated)) {
		t.Errorf("the two stores do not join to one text with a newline between fields, so the body imitates no boundary:\n%q\n%q",
			joinedWithSeparator(readRecord(t, fields)), joinedWithSeparator(readRecord(t, imitated)))
	}
	if fields.Digest == imitated.Digest {
		t.Errorf("a body holding the newline-joined text of the fields after it, %q, reads the digest %s of the two fields it imitates",
			probe+"\n"+blockAddress(r2), fields.Digest)
	}
}

func queryDigestStepMoves(t *testing.T, rig *queryDigestRig) {
	ctx := context.Background()
	c1, c2, r1, r2 := rig.block("c1"), rig.block("c2"), rig.block("r1"), rig.block("r2")
	// moved records the digest before change and after it, and fails unless it moved.
	moved := func(t *testing.T, what string, change func(t *testing.T)) (docs.QueryRecord, docs.QueryRecord) {
		t.Helper()
		before := rig.record(t)
		change(t)
		after := rig.record(t)
		t.Logf("%s: %s -> %s", what, before.Digest, after.Digest)
		if after.Digest == before.Digest {
			t.Errorf("%s leaves the verification digest at %s", what, before.Digest)
		}
		return before, after
	}
	t.Run("a check's words", func(t *testing.T) {
		moved(t, "c1 reworded", func(t *testing.T) { rig.upload(t, queryDigestChecks("The list reads the urgent asks first.", false)) })
	})
	t.Run("an attribute of a check", func(t *testing.T) {
		held, _, err := graphstore.CurrentValue(ctx, rig.database.Pool, refs.BlockNodeKind, c2, graphstore.AttributeSurface)
		if err != nil {
			t.Fatalf("read the surface of %s: %v", c2, err)
		}
		to := "api"
		if held == to {
			to = "browser"
		}
		before, after := moved(t, "c2's surface from "+strconv.Quote(held)+" to "+to, func(t *testing.T) {
			rig.state(t, blockValue(c2, graphstore.AttributeSurface, to))
		})
		if !slices.Equal(walkAddresses(before.Nodes), walkAddresses(after.Nodes)) {
			t.Errorf("an attribute of c2 moves the query's nodes from %v to %v", walkAddresses(before.Nodes), walkAddresses(after.Nodes))
		}
	})
	t.Run("an exchange of two siblings", func(t *testing.T) {
		// Two checks that hold a rank: each takes the other's.
		first, _, err := graphstore.CurrentValue(ctx, rig.database.Pool, refs.BlockNodeKind, c1, refs.AttributeRank)
		if err != nil {
			t.Fatalf("read the rank of %s: %v", c1, err)
		}
		second, _, err := graphstore.CurrentValue(ctx, rig.database.Pool, refs.BlockNodeKind, c2, refs.AttributeRank)
		if err != nil {
			t.Fatalf("read the rank of %s: %v", c2, err)
		}
		_, after := moved(t, "c1 and c2 exchanged by rank", func(t *testing.T) {
			rig.state(t, blockValue(c1, refs.AttributeRank, second), blockValue(c2, refs.AttributeRank, first))
		})
		if at1, at2 := slices.Index(walkAddresses(after.Nodes), blockAddress(c1)), slices.Index(walkAddresses(after.Nodes), blockAddress(c2)); at1 < at2 {
			t.Errorf("after the exchange the query returns %v, want c2 before c1", walkAddresses(after.Nodes))
		}
		rig.state(t, blockValue(c1, refs.AttributeRank, first), blockValue(c2, refs.AttributeRank, second))
		// Two results under one check, which hold no rank and stand in the order they were made:
		// each takes the other's time, and nothing the record writes of either changes.
		earlier := rig.result(t, "c1", "pass", newHeadCommit)
		later := rig.result(t, "c1", "fail", mergeCommit)
		before := rig.record(t)
		if _, err := rig.database.Pool.Exec(ctx, `
			update graph_nodes n set made_at = other.made_at
			from graph_nodes other
			where n.kind = $1 and other.kind = $1
			  and ((n.id = $2 and other.id = $3) or (n.id = $3 and other.id = $2))
		`, graphstore.KindResult, earlier, later); err != nil {
			t.Fatalf("exchange the times of the results %s and %s: %v", earlier, later, err)
		}
		after = rig.record(t)
		t.Logf("results %s and %s exchanged by the time each was made: %s -> %s", earlier, later, before.Digest, after.Digest)
		if after.Digest == before.Digest {
			t.Errorf("the results %s and %s exchanged under c1 read one digest for both, %s: the record holds the order of the walk, siblings in the order the store holds",
				earlier, later, before.Digest)
		}
		if a, b := slices.Index(walkAddresses(after.Nodes), resultNodeAddress(earlier)), slices.Index(walkAddresses(after.Nodes), resultNodeAddress(later)); a < b {
			t.Errorf("after the exchange the query returns %v, want %s after %s", walkAddresses(after.Nodes), earlier, later)
		}
	})
	t.Run("an added check", func(t *testing.T) {
		_, after := moved(t, "c3 added", func(t *testing.T) { rig.upload(t, queryDigestChecks("The list reads the urgent asks first.", true)) })
		if !slices.Contains(walkAddresses(after.Nodes), blockAddress(rig.block("c3"))) {
			t.Errorf("the query returns %v after c3 is added, want c3 among them", walkAddresses(after.Nodes))
		}
	})
	t.Run("a removed check", func(t *testing.T) {
		_, after := moved(t, "c3 removed", func(t *testing.T) { rig.upload(t, queryDigestChecks("The list reads the urgent asks first.", false)) })
		if slices.Contains(walkAddresses(after.Nodes), blockAddress(rig.block("c3"))) {
			t.Errorf("the query returns %v after c3 is removed, want no c3", walkAddresses(after.Nodes))
		}
	})
	t.Run("one check's serves retargeted", func(t *testing.T) {
		before, after := moved(t, "c2's serves retargeted from r1 to r2", func(t *testing.T) {
			rig.state(t, blockTuple(c2, "serves", refs.BlockNodeKind, r2, true), blockTuple(c2, "serves", refs.BlockNodeKind, r1, false))
		})
		// Words, values, nodes and order stand: the relations alone moved.
		was, now := readRecord(t, before), readRecord(t, after)
		if !slices.Equal(walkAddresses(before.Nodes), walkAddresses(after.Nodes)) || len(was) != len(now) {
			t.Fatalf("the retarget moves the query's nodes from %v to %v", walkAddresses(before.Nodes), walkAddresses(after.Nodes))
		}
		for index := range was {
			if was[index].text != now[index].text || !slices.Equal(was[index].values, now[index].values) || was[index].parent != now[index].parent || was[index].rank != now[index].rank {
				t.Errorf("the retarget changes %s beyond its relations: %+v -> %+v", was[index].address, was[index], now[index])
			}
		}
		rig.state(t, blockTuple(c2, "serves", refs.BlockNodeKind, r1, true), blockTuple(c2, "serves", refs.BlockNodeKind, r2, false))
	})
	t.Run("a result's verdict", func(t *testing.T) {
		result := rig.result(t, "c2", "pass", headCommit)
		moved(t, "the verdict of result "+result+" from pass to fail", func(t *testing.T) {
			rig.state(t, graphstore.Tuple{
				FromKind: graphstore.KindResult, FromID: result, Kind: graphstore.AttributeVerdict,
				ToKind: graphstore.KindValue, ToID: "fail",
				At: time.Now().UTC(), Who: digestWriter, SourceEvent: graphstore.NoEvent, Asserted: true,
			})
		})
	})
}

func queryDigestStepUnmoved(t *testing.T, rig *queryDigestRig) {
	c1, r2 := rig.block("c1"), rig.block("r2")
	before := rig.record(t)
	editSpec(t, rig.handler, rig.issue.Key, []map[string]any{{"op": "replace", "find": "An ask keeps its urgency.", "with": "An ask keeps the urgency it was raised with."}})
	if after := rig.record(t); after.Digest != before.Digest {
		t.Errorf("a word of r2, a node the verification query does not return, moves its digest from %s to %s", before.Digest, after.Digest)
	}
	// A tuple that runs into c1 from r2, a node outside the query, is r2's fact and not c1's.
	rig.state(t, blockTuple(r2, graphstore.TupleMentions, refs.BlockNodeKind, c1, true))
	if after := rig.record(t); after.Digest != before.Digest {
		t.Errorf("a tuple from %s into %s, a returned node, moves the verification digest from %s to %s", r2, c1, before.Digest, after.Digest)
	}
}

func queryDigestStepApproval(t *testing.T, rig *queryDigestRig) {
	ctx := context.Background()
	c2, r1, r2 := rig.block("c2"), rig.block("r1"), rig.block("r2")
	request := requestApprovalWithVerification(t, rig.handler, rig.spec.id)
	reviewDocument(t, rig.handler, rig.spec.id, "alice", "approved", "")
	approved, found := latestReview(t, rig, rig.verification)
	if !found || approved.State != "approved" || approved.AskID == nil || *approved.AskID != request.ID {
		t.Fatalf("the Verification document's newest review is %s at version %d answering %s (found %t), want an approval answering the request %s",
			approved.State, approved.Version, nullable(approved.AskID), found, request.ID)
	}
	head := rig.record(t)
	t.Logf("the approval of version %d of the Verification document stores %s; the query's record at the head digests to %s",
		approved.Version, nullable(approved.ContentSHA256), head.Digest)

	t.Run("the stored digest keeps the column's shape and never moves", func(t *testing.T) {
		if approved.ContentSHA256 == nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(*approved.ContentSHA256) {
			t.Errorf("the approval stores content_sha256 %s, want 64 lower-case hex digits", nullable(approved.ContentSHA256))
		}
		if approved.ContentSHA256Source == nil || *approved.ContentSHA256Source != "insert" {
			t.Errorf("the approval's content_sha256_source is %s, want insert", nullable(approved.ContentSHA256Source))
		}
		var definition string
		if err := rig.database.Pool.QueryRow(ctx, `
			select pg_get_constraintdef(oid) from pg_constraint
			where conrelid = 'artifact_reviews'::regclass and conname = 'artifact_reviews_content_sha256_hex'
		`).Scan(&definition); err != nil {
			t.Fatalf("read the constraint artifact_reviews_content_sha256_hex: %v", err)
		}
		if !strings.Contains(definition, `'^[0-9a-f]{64}$'`) {
			t.Errorf("artifact_reviews_content_sha256_hex is %s, want it to hold the column to ^[0-9a-f]{64}$", definition)
		}
		tx, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, `update artifact_reviews set content_sha256 = repeat('0', 64) where id = $1`, approved.ID)
		var refused *pgconn.PgError
		if !errors.As(err, &refused) || !strings.Contains(refused.Where, "artifact_review_content_sha256_update") {
			t.Errorf("an update of the stored digest answers %v, want the refusal artifact_review_content_sha256_update raises", err)
		} else {
			t.Logf("the update of a stored digest is refused: %s (%s)", refused.Message, refused.Where)
		}
	})

	t.Run("the approval covers the digest of the verification query at the version it approves", func(t *testing.T) {
		var supertag string
		var definition *string
		if err := rig.database.Pool.QueryRow(ctx, `select supertag, attributes->>$3 from graph_nodes where kind = $1 and id = $2`,
			graphstore.KindVersion, graphstore.VersionID(rig.verificationRef, approved.Version), graphstore.AttributeDefinitionVersion,
		).Scan(&supertag, &definition); err != nil {
			t.Errorf("read the node of version %d of the Verification document: %v", approved.Version, err)
		} else if supertag != graphstore.SupertagQueryVersion || definition == nil || *definition != strconv.Itoa(graphstore.QueryDefinitionVersion) {
			t.Errorf("version %d of the Verification document is a %s holding definition_version %s, want a query version holding %d",
				approved.Version, supertag, nullable(definition), graphstore.QueryDefinitionVersion)
		}
		digest, cut, rendered := recordedVersionRender(t, rig, rig.verification, approved.Version)
		if !rendered {
			t.Errorf("version %d of the Verification document holds no version_renders row, so the approval records no read sequence and no record: "+
				"this tree's version write writes none (docs.QueueVersionRender has no caller), and the approval's content_sha256 %s is the sha256 of the stored markdown, not of the verification query's record (%s)",
				approved.Version, nullable(approved.ContentSHA256), head.Digest)
			return
		}
		record := rig.recordAt(t, rig.issue.Key, graphstore.QueryVerification, cut)
		if approved.ContentSHA256 == nil || *approved.ContentSHA256 != record.Digest || digest != record.Digest {
			t.Errorf("the approval of version %d stores %s and its render row %s, want the digest %s of the verification record at the read sequence %d",
				approved.Version, nullable(approved.ContentSHA256), digest, record.Digest, cut.ReadSeq)
		}
	})

	t.Run("the request carries one approves tuple to each query version node, which the asks row projects", func(t *testing.T) {
		var specVersion int
		for _, entry := range readApprovalDocuments(t, rig, request.ID) {
			if entry.ArtifactID == rig.spec.id {
				specVersion = entry.Version
			}
		}
		want := []string{
			graphstore.KindVersion + ":" + graphstore.VersionID(rig.spec.refKey, specVersion),
			graphstore.KindVersion + ":" + graphstore.VersionID(rig.verificationRef, approved.Version),
		}
		slices.Sort(want)
		held, err := graphstore.CurrentOut(ctx, rig.database.Pool, graphstore.KindAsk, request.ID, []string{"approves"})
		if err != nil {
			t.Fatalf("read the approves tuples of the request %s: %v", request.ID, err)
		}
		var got []string
		for _, tuple := range held {
			got = append(got, tuple.ToKind+":"+tuple.ToID)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("the request %s carries the approves tuples %v, want one to each query version node it approves, %v", request.ID, got, want)
		}
		var projected []string
		for _, entry := range readApprovalDocuments(t, rig, request.ID) {
			var refKey string
			if err := rig.database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, entry.ArtifactID).Scan(&refKey); err != nil {
				t.Fatalf("read the ref_key of %s: %v", entry.ArtifactID, err)
			}
			projected = append(projected, graphstore.KindVersion+":"+graphstore.VersionID(refKey, entry.Version))
		}
		slices.Sort(projected)
		if !slices.Equal(projected, got) {
			t.Errorf("the asks row's approval names the versions %v, and the request's approves tuples %v: the row is the projection of the tuples", projected, got)
		}
	})

	t.Run("asks_approval_kind_check, asks_normalize_approval_documents and its trigger stand as their migration made them", func(t *testing.T) {
		requireApprovalShapeStands(t, rig)
	})

	// The retarget: c2's serves leaves r1 for r2, with words, values, nodes and order as they were.
	rig.state(t, blockTuple(c2, "serves", refs.BlockNodeKind, r2, true), blockTuple(c2, "serves", refs.BlockNodeKind, r1, false))
	retargeted := rig.record(t)

	t.Run("the retargeted current query reads not approved and the gate asks again", func(t *testing.T) {
		if retargeted.Digest == head.Digest {
			t.Fatalf("the retarget leaves the verification digest at %s, so no gate can tell it", head.Digest)
		}
		if read := readApproval(t, rig.handler, rig.verification); read.Approval != nil && read.Approval.State == "approved" {
			t.Errorf("after c2's serves moved from r1 to r2 the Verification document reads %q at version %d, stored digest %s, current query digest %s: "+
				"the gate compares the review's version with the latest version (applyApprovals), never the stored digest with the current query's, so it presents a changed current query as approved",
				read.Approval.State, read.Approval.LatestVersion, nullable(approved.ContentSHA256), retargeted.Digest)
		}
		asked := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+rig.spec.id+"/approval-requests",
			map[string]any{"actor": sessionActor(), "include_verification": true})
		answer := decodeBody[struct {
			Ask *struct {
				ID string `json:"id"`
			} `json:"ask"`
		}](t, asked)
		if asked.Code != http.StatusCreated || answer.Ask == nil {
			t.Errorf("the approval gate over the retargeted query answers %d with no ask (%s), want a request asking the person again: it reads the documents approved by version alone (docs.NamedDocumentsApproved)",
				asked.Code, strings.TrimSpace(asked.Body.String()))
		} else {
			reviewDocument(t, rig.handler, rig.spec.id, "alice", "approved", "")
		}
	})

	t.Run("the historical approval still answers for the revision it covered", func(t *testing.T) {
		still, found := reviewByID(t, rig, approved.ID)
		if !found || still.Version != approved.Version || nullable(still.ContentSHA256) != nullable(approved.ContentSHA256) {
			t.Fatalf("the approval %s reads version %d and digest %s after the retarget, want it as it was: version %d and digest %s",
				approved.ID, still.Version, nullable(still.ContentSHA256), approved.Version, nullable(approved.ContentSHA256))
		}
		_, cut, rendered := recordedVersionRender(t, rig, rig.verification, approved.Version)
		if !rendered {
			t.Errorf("version %d holds no version_renders row, so no read sequence lets the approval answer for the revision it covered after the retarget", approved.Version)
			return
		}
		if record := rig.recordAt(t, rig.issue.Key, graphstore.QueryVerification, cut); approved.ContentSHA256 == nil || record.Digest != *approved.ContentSHA256 {
			t.Errorf("the record at the approval's read sequence %d digests to %s, want the stored %s", cut.ReadSeq, record.Digest, nullable(approved.ContentSHA256))
		}
	})

	t.Run("a result appended under unchanged checks carries the approval to its version", func(t *testing.T) {
		current, found := latestReview(t, rig, rig.verification)
		if !found || current.State != "approved" {
			t.Fatalf("the Verification document's newest review is %s at version %d (found %t), want an approval to carry", current.State, current.Version, found)
		}
		if current.Version != latestNumber(t, rig, rig.verification) {
			// The gate asked nobody again, so the approval stands at the version it was given at.
			reviewDocument(t, rig.handler, rig.verification, "alice", "approved", "")
			current, _ = latestReview(t, rig, rig.verification)
		}
		before := readRecord(t, rig.record(t))
		appended := rig.result(t, "c1", "pass", baseMergeCommit)
		after := readRecord(t, rig.record(t))
		for _, node := range before {
			if node.typ != "check" {
				continue
			}
			index := slices.IndexFunc(after, func(other recordNode) bool { return other.address == node.address })
			if index < 0 || after[index].text != node.text || !slices.Equal(after[index].values, node.values) || !slices.Equal(after[index].relations, node.relations) {
				t.Fatalf("appending the result %s changed the check %s", appended, node.address)
			}
		}
		carried, _ := latestReview(t, rig, rig.verification)
		latest := latestNumber(t, rig, rig.verification)
		from, isCarry := 0, false
		if carried.Reason != nil {
			from, isCarry = docs.CarriedFromVersion(*carried.Reason)
		}
		if latest <= current.Version || carried.Version != latest || !isCarry || from != current.Version ||
			carried.Reason == nil || !strings.Contains(*carried.Reason, appended) || carried.State != "approved" {
			t.Errorf("after the result %s was appended under c1, every check unchanged, the Verification document's latest version is %d and its newest review is %s at version %d with the reason %q, "+
				"want a review carried from version %d to a new version, naming the result: a recorded result writes no version of the verification query, so nothing carries the approval to one",
				appended, latest, carried.State, carried.Version, nullable(carried.Reason), current.Version)
		}
	})

	t.Run("a version written before the cutover still answers the digest of its stored markdown", func(t *testing.T) {
		var markdown string
		var answered *string
		if err := rig.database.Pool.QueryRow(ctx, `
			select v.markdown, artifact_version_text_sha256(v.artifact_id, v.number)
			from artifact_versions v where v.artifact_id = $1 and v.number = 1
		`, rig.verification).Scan(&markdown, &answered); err != nil {
			t.Fatalf("read version 1 of the Verification document: %v", err)
		}
		if _, _, rendered := recordedVersionRender(t, rig, rig.verification, 1); rendered {
			t.Skip("version 1 of the Verification document holds a version_renders row, so it is no version written before the cutover")
		}
		sum := sha256.Sum256([]byte(markdown))
		if answered == nil || *answered != hex.EncodeToString(sum[:]) {
			t.Errorf("artifact_version_text_sha256 answers %s for version 1, which holds no tree, want the digest of its stored markdown %x", nullable(answered), sum)
		}
		rows, err := rig.database.Pool.Query(ctx, `
			select r.id::text, r.version, r.content_sha256, artifact_version_text_sha256(r.artifact_id, r.version)
			from artifact_reviews r
			where not exists (select 1 from version_renders v where v.artifact_id = r.artifact_id and v.number = r.version)
		`)
		if err != nil {
			t.Fatalf("read the reviews of versions written before the cutover: %v", err)
		}
		standing, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			ID       string
			Version  int
			Pinned   *string
			Answered *string
		}])
		if err != nil {
			t.Fatalf("read the reviews of versions written before the cutover: %v", err)
		}
		for _, review := range standing {
			if nullable(review.Pinned) != nullable(review.Answered) {
				t.Errorf("the review %s of version %d pins %s and the function answers %s for its version: an approval standing on the day of the cutover must cover what it covered",
					review.ID, review.Version, nullable(review.Pinned), nullable(review.Answered))
			}
		}
	})
}

// readApprovalDocuments is the documents the asks row of the approval request askID names.
func readApprovalDocuments(t *testing.T, rig *queryDigestRig, askID string) []model.AskApprovalDocument {
	t.Helper()
	var encoded []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `select approval -> 'documents' from asks where id = $1`, askID).Scan(&encoded); err != nil {
		t.Fatalf("read the approval of the request %s: %v", askID, err)
	}
	var documents []model.AskApprovalDocument
	if err := json.Unmarshal(encoded, &documents); err != nil {
		t.Fatalf("decode the approval documents of the request %s: %v", askID, err)
	}
	return documents
}

// nullable is the text p points at, or null where it points at nothing, as a failure prints a
// column that may be null.
func nullable(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// reviewByID is the review id as its row stands, and false where no row holds it.
func reviewByID(t *testing.T, rig *queryDigestRig, id string) (model.ArtifactReview, bool) {
	t.Helper()
	review, err := docs.ScanReview(rig.database.Pool.QueryRow(context.Background(),
		`select `+docs.ReviewColumns+` from artifact_reviews where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ArtifactReview{}, false
	}
	if err != nil {
		t.Fatalf("read the review %s: %v", id, err)
	}
	return review, true
}

// requireApprovalShapeStands holds asks_approval_kind_check, the function
// asks_normalize_approval_documents and its trigger to approvalMigration: no later migration names
// any of them, the function's body is the migration's byte for byte, and the constraint and the
// trigger the database holds are the ones the migration's own statements make on a scratch copy
// of the asks table, as Postgres prints each.
func requireApprovalShapeStands(t *testing.T, rig *queryDigestRig) {
	t.Helper()
	ctx := context.Background()
	entries, err := os.ReadDir(migrationsDirectory)
	if err != nil {
		t.Fatalf("read the migrations: %v", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".up.sql") || entry.Name() <= approvalMigration {
			continue
		}
		body, err := os.ReadFile(filepath.Join(migrationsDirectory, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, name := range []string{"asks_approval_kind_check", "asks_normalize_approval_documents"} {
			if strings.Contains(string(body), name) {
				t.Errorf("%s, after %s, names %s: none of the three is on the root's list, so no migration of this tree moves it", entry.Name(), approvalMigration, name)
			}
		}
	}
	text, err := os.ReadFile(filepath.Join(migrationsDirectory, approvalMigration))
	if err != nil {
		t.Fatalf("read %s: %v", approvalMigration, err)
	}
	migration := string(text)
	function := statementFrom(t, migration, "create function asks_normalize_approval_documents()", "$$;")
	body := function[strings.Index(function, "$$")+2 : len(function)-len("$$;")]
	var source string
	if err := rig.database.Pool.QueryRow(ctx, `select prosrc from pg_proc where proname = 'asks_normalize_approval_documents'`).Scan(&source); err != nil {
		t.Fatalf("read the function asks_normalize_approval_documents: %v", err)
	}
	if source != body {
		t.Errorf("asks_normalize_approval_documents reads\n%s\nwant the body %s gave it\n%s", source, approvalMigration, body)
	}
	trigger := statementFrom(t, migration, "create trigger asks_normalize_approval_documents", ";")
	constraint := statementFrom(t, migration, "alter table asks add constraint asks_approval_kind_check", "\n);")
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the scratch copy of asks: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `create temporary table asks_scratch (like asks)`); err != nil {
		t.Fatalf("make the scratch copy of asks: %v", err)
	}
	for _, statement := range []string{
		strings.Replace(trigger, "on asks", "on pg_temp.asks_scratch", 1),
		strings.Replace(constraint, "alter table asks ", "alter table pg_temp.asks_scratch ", 1),
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("run %s's statement on the scratch copy of asks: %v\n%s", approvalMigration, err, statement)
		}
	}
	table := regexp.MustCompile(` ON \S+ `)
	for _, read := range []struct{ what, sql string }{
		{"the constraint asks_approval_kind_check", `select pg_get_constraintdef(oid) from pg_constraint where conrelid = $1::regclass and conname = 'asks_approval_kind_check'`},
		{"the trigger asks_normalize_approval_documents", `select pg_get_triggerdef(oid) from pg_trigger where tgrelid = $1::regclass and tgname = 'asks_normalize_approval_documents'`},
	} {
		var stands, made string
		if err := tx.QueryRow(ctx, read.sql, "asks").Scan(&stands); err != nil {
			t.Errorf("read %s on asks: %v", read.what, err)
			continue
		}
		if err := tx.QueryRow(ctx, read.sql, "pg_temp.asks_scratch").Scan(&made); err != nil {
			t.Fatalf("read %s on the scratch copy: %v", read.what, err)
		}
		if table.ReplaceAllString(stands, " ON asks ") != table.ReplaceAllString(made, " ON asks ") {
			t.Errorf("%s reads\n%s\nwant what %s makes\n%s", read.what, stands, approvalMigration, made)
		}
	}
}
