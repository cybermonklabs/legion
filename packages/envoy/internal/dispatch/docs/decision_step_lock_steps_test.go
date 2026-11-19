package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheDecisionsStepsWritersTakeTheStoreLockOrder drives the decisions step's sites of the node
// and tuple store along the passes that reach them, with the machinery of
// TestEveryDocsStoreWriterTakesTheStoreLockOrder: each path lists its steps in the order it takes
// them, the list is checked against the order written on api/inbox_read.go's
// markInboxReadThreads (requireTheDocsOrderAdmits), and each step is held in turn while the
// path's backend is read at the statement it waits at and at the table locks it holds
// (requireEachDocsStepIsWaitedAtInTurn). The paths are:
//
//   - the third phase's conversion of an issue whose decisions document holds one section, which a
//     person kept (restoreIssueDecisions): under the issue's owner lock, the decisions document's
//     version without the section (writeVersion: its fence row, its block index, its
//     artifact.version and what the index stated under it), the spec's version holding the
//     section as a decision (writeVersion again, under the spec's own fence row), then the
//     section's move and the Keep carried onto the decision's node, with the keeper's node, under
//     the spec's event, and the decision_keeps row that projects the Keep (writeFacts), and last
//     the decisions document's retirement (RetireDocument, writeRetiredNode): its artifact node
//     stated gone, its artifacts row, the document.retired and the node's facts under it;
//   - the catch-up of a follow a process of the release before recorded (asks.CatchUpFollower,
//     run by catchUpAsk): the issue's owner lock, the ask's row, the follower row marked, and the
//     session's node and the ask node's followed_by under that build's ask.follower_added, an event
//     the log held before the pass began, so no event of this path orders them.
//
// Each path takes its issue's owner lock first, the transaction advisory lock every writer of the
// issue takes (lockArtifactOwner, store.LockOwner), which locks no table and no row; the issue's
// row is the event append's, in its write phase.
//
// Each document the conversion versions is under its own revision fence, which orders that
// document's block statement alone, as a version write's does (api's documentEdit): the version's
// insert takes the row, and its block index writes the document's block nodes, rows and tuples
// after it. A step whose table the path wrote at an earlier step is held by a gate
// (gatedDocsWrites).
func TestTheDecisionsStepsWritersTakeTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	for _, path := range []func(t *testing.T) (*docsWriterRig, docsWriter){conversionPath, followCatchUpPath} {
		rig, writer := path(t)
		t.Run(writer.name, func(t *testing.T) {
			requireTheDocsOrderAdmits(t, writer)
			requireEachDocsStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// issueOwnerStep is the owner lock of *issue, the run's issue, which the step's passes take first
// (lockArtifactOwner): a transaction advisory lock, held as every writer of the issue takes it.
func issueOwnerStep(issue *string) docsStep {
	return docsStep{
		name: "the owner lock", order: docsOwnerStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
			t.Helper()
			if err := store.LockOwner(ctx, holder, store.IssueOwner(*issue)); err != nil {
				t.Fatalf("hold issue %s's owner lock: %v", *issue, err)
			}
		},
		at: docsOwnerStatement,
	}
}

// decisionStepRig is a database of its own and a document service over it that settles an hour
// on, so no timer settles a document under a hold, and no write of another path is a write the
// step reads.
func decisionStepRig(t *testing.T) *docsWriterRig {
	t.Helper()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	return &docsWriterRig{database: database, service: service}
}

// conversionPath is the third phase's conversion of one issue (restoreIssueDecisions), run as the
// step runs it (RestoreAndConvert) over a project of its own for each run: an issue whose spec
// holds a requirement and whose decisions document holds one section, which a person kept. The
// pass versions the decisions document without the section and the spec with the section as a
// decision, carries the move and the Keep onto the decision's node, and retires the decisions
// document, which holds no section any more.
//
// The decisions document's block nodes, block rows, tie rows, version event and tuples are the
// pass's first writes of their tables, so each is held as its table, and its fence row, which its
// first version wrote, is held as a row; so is the spec's. The spec's decision is a new node, whose
// address is the spec's ref_key and the section's heading id, which the decision keeps, and the
// keeper is new to each run: each is held as a row another transaction inserted and has not
// committed. The decision_keeps row is the pass's first write of that table. Every other step
// comes after a write of its table and is held by its gate.
//
// A version's insert takes the document's fence row and, through the insert's triggers, the
// document's search row and its re-index mark (document_search, refs_grammar_owed); the decisions
// document's version event takes the issue's row, which its append locks and moves the last_seq
// of. Those are the rows the spec's fence finds taken since the decisions document's fence.
func conversionPath(t *testing.T) (*docsWriterRig, docsWriter) {
	r := decisionStepRig(t)
	// What the run under way wrote, which its steps' holds and gates name.
	var issue, project, spec, decisions, specRefKey, decisionsRefKey string
	const decisionsFence, specFence, specEvent, retirement = "the decisions document's fence", "the spec's fence",
		"the spec's version event", "the retirement's event"
	fenceRow := func(document *string) func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
		return func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
			holdDocsRow(t, ctx, holder, `select 1 from document_block_versions where artifact_id = $1::uuid for no key update`, *document)
		}
	}
	versionFence := func(name string, document *string) docsStep {
		return docsStep{
			name: name, order: docsFenceStep, hold: fenceRow(document), at: "with head as (select ",
			takes: []docsTableLock{
				{"document_block_versions", docsRowExclusiveMode},
				{"document_search", docsRowExclusiveMode}, {"refs_grammar_owed", docsRowExclusiveMode},
			},
		}
	}
	fenced := func(step docsStep, fence string) docsStep {
		step.fenced, step.fence = true, fence
		return step
	}
	caused := func(step docsStep, cause string) docsStep {
		step.caused, step.cause = true, cause
		return step
	}
	return r, docsWriter{
		name: "restoreIssueDecisions converting and retiring",
		steps: []docsStep{
			issueOwnerStep(&issue),
			versionFence(decisionsFence, &decisions),
			fenced(docsStep{
				name: "the decisions document's block nodes", order: docsNodeStep,
				hold: holdDocsTableWrites("graph_nodes"), at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			}, decisionsFence),
			fenced(docsStep{
				name: "the decisions document's block rows", order: docsProjectionStep, names: "the decisions document's block nodes",
				hold: holdDocsTableWrites("document_blocks"), at: "delete from document_blocks",
				takes: []docsTableLock{{"document_blocks", docsRowExclusiveMode}},
			}, decisionsFence),
			fenced(docsStep{
				name: "the decisions document's tie rows", order: docsProjectionStep, names: "the decisions document's block nodes",
				hold: holdDocsTableWrites("block_refs"), at: "delete from block_refs",
				takes: []docsTableLock{{"block_refs", docsRowExclusiveMode}},
			}, decisionsFence),
			{
				name: "the decisions document's version event", order: docsEventStep,
				hold: holdDocsTableWrites("events"), at: docsEventStatement,
				takes: []docsTableLock{{"events", docsRowExclusiveMode}, {"issues", docsRowExclusiveMode}},
			},
			fenced(caused(docsStep{
				name: "the decisions document's block tuples", order: docsTupleStep,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			}, "the decisions document's version event"), decisionsFence),
			versionFence(specFence, &spec),
			fenced(docsStep{
				name: "the spec's decision node", order: docsNodeStep,
				hold: holdDocsBlockNode, at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			}, specFence),
			fenced(docsStep{
				name: "the spec's block rows", order: docsProjectionStep, names: "the spec's decision node",
				hold:  gatedDocsWrites(r, "insert", "document_blocks", func() string { return "new.artifact_id = " + docsLiteral(spec) + "::uuid" }),
				at:    "insert into document_blocks (",
				takes: []docsTableLock{{"document_blocks", docsRowExclusiveMode}},
			}, specFence),
			fenced(docsStep{
				name: "the spec's tie rows", order: docsProjectionStep, names: "the spec's decision node",
				hold:  gatedDocsWrites(r, "insert", "block_refs", func() string { return "new.artifact_id = " + docsLiteral(spec) + "::uuid" }),
				at:    "insert into block_refs (",
				takes: []docsTableLock{{"block_refs", docsRowExclusiveMode}},
			}, specFence),
			{
				name: specEvent, order: docsEventStep,
				hold: gatedDocsWrites(r, "insert", "events", func() string {
					return "new.type = 'artifact.version' and new.payload->>'artifact_id' = " + docsLiteral(spec)
				}),
				at:    docsEventStatement,
				takes: []docsTableLock{{"events", docsRowExclusiveMode}},
			},
			fenced(caused(docsStep{
				name: "the spec's block tuples", order: docsTupleStep,
				hold: gatedDocsWrites(r, "insert", "graph_tuples", func() string {
					return "new.from_kind = " + docsLiteral(refs.BlockNodeKind) + " and starts_with(new.from_id, " + docsLiteral(specRefKey+"#") + ")"
				}),
				at:    docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			}, specEvent), specFence),
			caused(docsStep{
				name: "the keeper's node, the move and the Keep", order: docsNodeStep,
				hold: holdDocsNewNode, at: "with graph_nodes_written as (",
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}, {"graph_tuples", docsRowExclusiveMode}},
			}, specEvent),
			{
				name: "the Keep's row", order: docsProjectionStep, names: "the keeper's node, the move and the Keep",
				hold: holdDocsTableWrites("decision_keeps"), at: "insert into decision_keeps (",
				takes: []docsTableLock{{"decision_keeps", docsRowExclusiveMode}},
			},
			{
				name: "the retired document's node", order: docsNodeStep,
				hold: gatedDocsWrites(r, "insert", "graph_nodes", func() string {
					return "new.kind = " + docsLiteral(graphstore.KindArtifact) + " and new.id = " + docsLiteral(decisionsRefKey)
				}),
				at:    docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			{
				name: "the retired document's row", order: docsProjectionStep, names: "the retired document's node",
				hold: gatedDocsWrites(r, "update", "artifacts", func() string {
					return "new.id = " + docsLiteral(decisions) + "::uuid and new.retired_at is not null"
				}),
				at:    "update artifacts set retired_at",
				takes: []docsTableLock{{"artifacts", docsRowExclusiveMode}},
			},
			docsGatedEventStep(r, retirement, model.DocumentRetiredEventType),
			caused(docsStep{
				name: "the retirement's facts", order: docsTupleStep,
				hold: gatedDocsWrites(r, "insert", "graph_tuples", func() string {
					return "new.from_kind = " + docsLiteral(graphstore.KindArtifact) + " and new.from_id = " + docsLiteral(decisionsRefKey)
				}),
				at:    docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			}, retirement),
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			r.named++
			project = fmt.Sprintf("CONV%d", r.named)
			issue = project + "-1"
			for _, statement := range []string{
				`insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`,
				`insert into issues (key, project_key, number, title, created_by, rank)
				 values ($1 || '-1', $1, 1, 'Converted', '{"kind":"user","id":"alice"}', 'U')`,
			} {
				if _, err := r.database.Pool.Exec(ctx, statement, project); err != nil {
					t.Fatalf("create issue %s: %v", issue, err)
				}
			}
			spec = r.issueDocument(t, ctx, issue, project, "spec", "spec.md", true, "", "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
			decisions = r.issueDocument(t, ctx, issue, project, "decisions-md", "decisions.md", false, "decisions",
				"# Decisions\n\n## Files stay out of the corpus\n\nWe write no file beside the database.\n")
			for document, refKey := range map[string]*string{spec: &specRefKey, decisions: &decisionsRefKey} {
				if err := r.database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(refKey); err != nil {
					t.Fatalf("read the ref_key of %s: %v", document, err)
				}
			}
			sections := decisionSectionsOf(liveTree(t, r.service, decisions))
			if len(sections) != 1 {
				t.Fatalf("the decisions document of %s holds %d sections, want one", issue, len(sections))
			}
			heading := sections[0].headingID()
			section, found, err := r.service.DecisionHeading(ctx, r.database.Pool, decisions, heading)
			if err != nil || !found {
				t.Fatalf("the heading %s reads no section (%t, %v)", heading, found, err)
			}
			keeper := r.fresh("lock-keeper")
			if _, err := r.database.Pool.Exec(ctx, `
				insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
				values ($1::uuid, $2, $3, 1, $4)
			`, decisions, heading, section.TextSHA256, keeper); err != nil {
				t.Fatalf("write %s's Keep of %s: %v", keeper, heading, err)
			}
			at := time.Now().UTC()
			converted, retired := issue, decisions
			return docsRun{
				document: spec,
				made:     graphstore.PersonNode(keeper, DecisionStepActor, at, graphstore.NoEvent),
				block: graphstore.Node{
					Kind: refs.BlockNodeKind, ID: specRefKey + "#" + heading, Supertag: refs.DecisionSupertag,
					Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: project,
					MadeAt: at, MadeBy: DecisionStepActor, SourceEvent: graphstore.NoEvent,
				},
				write: func() error {
					step := NewDecisionStep(r.database, "", nil)
					step.UseDocuments(r.service)
					done, failed, err := step.RestoreAndConvert(ctx, project, true, false)
					if err != nil || len(failed) > 0 {
						return fmt.Errorf("the third phase answered %v with failures %+v", err, failed)
					}
					if len(done) != 1 || done[0].Issue != converted || done[0].Decisions != 1 || !done[0].Retired {
						return fmt.Errorf("the third phase did %+v, want %s's section a decision and its decisions document retired", done, converted)
					}
					var gone bool
					if err := r.database.Pool.QueryRow(ctx, `select retired_at is not null from artifacts where id = $1`, retired).Scan(&gone); err != nil || !gone {
						return fmt.Errorf("the decisions document of %s reads retired %v (%v), want it retired", converted, gone, err)
					}
					return nil
				},
			}
		},
	}
}

// issueDocument creates document slug of issue in project holding markdown as its version 1,
// written by alice and indexed as an upload of a new document is, and settles it, as the decisions
// step's tests write the documents of DOC-1 (restoreIssue.document).
func (r *docsWriterRig) issueDocument(t *testing.T, ctx context.Context, issue, project, slug, name string, primary bool, role, markdown string) string {
	t.Helper()
	var id string
	var rolePointer *string
	if role != "" {
		rolePointer = &role
	}
	if err := r.database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ($1, $2, $3, $4, 'doc', $5, $6, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue, project, slug, name, primary, rolePointer).Scan(&id); err != nil {
		t.Fatalf("create document %s of %s: %v", slug, issue, err)
	}
	if err := joinedWrite(r.service, func(ctx context.Context) error {
		written, err := r.service.SeedText(ctx, id, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, id, written); err != nil {
			return err
		}
		_, write, err := r.service.IndexDocumentReferences(ctx, id, 1, written)
		if err != nil {
			return err
		}
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write document %s of %s: %v", slug, issue, err)
	}
	settleCurrentGeneration(t, r.service, id)
	return id
}

// followCatchUpPath is the catch-up of a follow the release before recorded on an ask this build
// marked (catchUpAsk, asks.CatchUpFollower), run as one pass of the step runs it, over an issue of
// its own for each run: that build's follow wrote the follower row unmarked and its
// ask.follower_added, and stated nothing on the store. The pass takes the ask's owner row, then
// the ask's row, marks the follower row, which names the ask's node, and then writes the session's
// node and the ask node's followed_by under that event. The issue's and the ask's rows are held as
// rows, and the follower row, the session's node and the followed_by, each the pass's first write
// of its table, as their tables. The follow's event was in the log before the pass, so no event
// step orders the followed_by.
func followCatchUpPath(t *testing.T) (*docsWriterRig, docsWriter) {
	r := decisionStepRig(t)
	// The run under way's issue and ask, which the holds name.
	var issue, ask string
	return r, docsWriter{
		name: "CatchUpFollower",
		steps: []docsStep{
			issueOwnerStep(&issue),
			{
				name: "the ask's row", order: docsProjectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
					holdDocsRow(t, ctx, holder, `select 1 from asks where id = $1::uuid for update`, ask)
				},
				at:    "select node_kind is not null, coalesce(issue_key, ''), coalesce(artifact_id::text, '') from asks",
				takes: []docsTableLock{{"asks", docsRowShareMode}},
			},
			{
				name: "the follower row", order: docsProjectionStep,
				hold: holdDocsTableWrites("ask_followers"), at: "update ask_followers f set node_kind",
				takes: []docsTableLock{{"ask_followers", docsRowExclusiveMode}},
			},
			{
				name: "the session's node", order: docsNodeStep,
				hold: holdDocsTableWrites("graph_nodes"), at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			{
				name: "the followed_by", order: docsTupleStep,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			r.named++
			ask, issue = seedCatchUpAsk(t, r.database, fmt.Sprintf("CATCH%d", r.named))
			session := r.fresh("lock-steps-follower")
			followed := asThePreviousBuild(t, r.database, issue, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
				if _, err := tx.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, $2)`, ask, session); err != nil {
					return model.Event{}, err
				}
				return model.Event{Type: "ask.follower_added", Actor: catchUpAlice, Payload: model.AskFollowerEventPayload{
					AskID: ask, SessionID: session, By: catchUpAlice,
				}}, nil
			})
			caughtUp := ask
			return docsRun{write: func() error {
				failed, err := NewDecisionStep(r.database, catchUpCommit, nil).catchUp(ctx)
				if err != nil || failed {
					return fmt.Errorf("the catch-up answered failed=%v: %v", failed, err)
				}
				var marked bool
				var cause int64
				if err := r.database.Pool.QueryRow(ctx, `
					select f.node_kind is not null, coalesce((
						select t.source_event from graph_tuples t
						where t.from_kind = $3 and t.from_id = $1::uuid::text and t.kind = $4 and t.to_id = $2 and t.asserted
						order by t.seq desc limit 1), 0)
					from ask_followers f where f.ask_id = $1::uuid and f.session_id = $2
				`, caughtUp, session, graphstore.KindAsk, graphstore.TupleFollowedBy).Scan(&marked, &cause); err != nil {
					return fmt.Errorf("read the caught-up follow of %s: %w", session, err)
				}
				if !marked || cause != followed {
					return fmt.Errorf("the follow of %s reads marked %v, its followed_by caused by %d, want it marked and caused by the follow's event %d",
						session, marked, cause, followed)
				}
				return nil
			}}
		},
	}
}
