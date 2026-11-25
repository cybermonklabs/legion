package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// itemBodyBytes is roughly how much text a comment, a message or an ask carries. It is the size
// of the items the production tables hold, not a figure the spec gives: what the measurements
// read of an item is its tsvector and its first block, both of which this size exercises.
const itemBodyBytes = 600

// askQuestionBlock is the part id of an ask's question, the part a reply under an ask names. The
// question is the first part of every ask (refs.AskParts), and a part the index has not seen
// before takes the next id of the ask's state row, counted from one (itemblocks.Align), so b1 is
// the question of every ask the seed writes, as it is the first block of every comment and
// message it writes.
const askQuestionBlock = "b1"

// writeAsks writes the asks with their options, their followers, their mentions, the parts the
// ask is indexed into and the `ask.opened` event each one is read through.
//
// The parts - the question, each option and the answer the ask holds - are recorded with the
// server's own indexer (refs.PlanAskParts), in the ask's own transaction, as every route that
// writes an ask records them (api/references.go's queueAskParts). The insert owes that index
// (migration 20261101143212's asks_item_blocks_owed trigger), so an ask the seed left unindexed
// is work a started server would do for itself, rewriting rows a measurement is being read from.
// A mention the question carries is then a row of `refs`, the ask's own, and a row of
// `ask_block_refs`, held by the part the text is in.
//
// The opening event is written in the ask's own transaction, as the create route writes it
// (api/asks.go), because its payload carries the question the row was written with. Every read
// of an ask resolves that event (events.OpenedEventIDs, through api.attachOpenedEventIDs) and
// fails the whole request when the ask has none, so an ask written without one answers 500 on
// every route that lists or reads it.
func (r *seedRun) writeAsks(ctx context.Context) error {
	seqs := r.askEventSeqs()
	// An ask costs a part index as a comment costs a block index, so the asks go in the smaller
	// batches the indexed items are written in.
	return r.inBatches(ctx, "asks", len(r.plan.asks), batchItems, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		mentions := newMentionRows("ask")
		var questionCited []graphstore.UncausedTuple
		followers := make([][]any, 0, to-from)
		followerNodes := make([]graphstore.Node, 0, to-from)
		followed := map[string]struct{}{}
		opened := make([][]any, 0, to-from)
		// Each ask's node goes in before any row of the batch, in its project, as the server's
		// ask create writes it; the seed records no event of it here, so it takes the sentinel.
		// The owner lock of every ask of the batch comes first, in owner key order, as the
		// store's lock order has it: the window write below takes each ask's owner lock
		// (refs.WriteItemWindows), which a batch holding the asks' nodes would otherwise take
		// after them.
		nodes := make([]graphstore.Node, 0, to-from)
		owners := make([]store.Owner, 0, to-from)
		for index := from; index < to; index++ {
			ask := r.plan.asks[index]
			nodes = append(nodes, graphstore.AskNode(ask.ID, r.itemProject(ask.Issue, ask.Document), nil, seedAuthor, ask.CreatedAt))
			var issueKey, artifactID *string
			if ask.Issue >= 0 {
				issueKey = &r.plan.issues[ask.Issue].Key
			}
			if ask.Document >= 0 {
				artifactID = &r.plan.documents[ask.Document].ID
			}
			if owner := store.OwnerOf(issueKey, artifactID); owner != nil {
				owners = append(owners, *owner)
			}
		}
		if err := store.LockOwners(ctx, tx, owners...); err != nil {
			return fmt.Errorf("the asks' owners: %w", err)
		}
		if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
			return fmt.Errorf("the asks' nodes: %w", err)
		}
		for index := from; index < to; index++ {
			ask := r.plan.asks[index]
			mention := ""
			if nth := share(index, len(r.plan.asks), r.sizes.AskMentions); nth > 0 {
				mention = r.plan.mentionText(index%len(mentionKinds), index)
			}
			question := r.plan.askQuestion(r.random, mention, itemBodyBytes)
			options := r.plan.askOptions(r.random, ask.Options)
			var issueKey, anchor, blockID *string
			var artifactID, blockArtifact *string
			if ask.Issue >= 0 {
				issueKey = &r.plan.issues[ask.Issue].Key
			}
			if ask.Document >= 0 {
				artifactID = &r.plan.documents[ask.Document].ID
			}
			var anchorValue *model.Anchor
			if ask.Anchored {
				document := r.plan.documents[ask.Document]
				block := firstParagraphBlock
				if ask.TableRow {
					block = r.tableRow
				}
				value := anchorJSON(document.ID, block, document.Versions, r.plan.vocabulary.phrase(r.random, 4))
				anchor = &value
				// The payload carries the anchor the row holds, read back with the server's own
				// reader of a stored anchor, so the two cannot describe different marks.
				anchorValue = &model.Anchor{}
				if err := model.DecodeAnchor([]byte(value), anchorValue); err != nil {
					return fmt.Errorf("ask %s: decode the anchor it is written with: %w", ask.ID, err)
				}
			}
			if ask.BlockDoc >= 0 {
				blockArtifact = &r.plan.documents[ask.BlockDoc].ID
				blockID = &r.plan.asks[index].BlockID
			}
			var answer *model.AskAnswer
			var resolution any
			switch ask.State {
			case "answered":
				given := r.askAnswer(ask, options)
				answer = &given
			case "resolved":
				resolution = fmt.Sprintf(`{"reason":%q,"by":%s}`, r.plan.vocabulary.phrase(r.random, 4), seedActor)
			}
			optionsJSON, err := json.Marshal(options)
			if err != nil {
				return fmt.Errorf("ask %s: encode its options: %w", ask.ID, err)
			}
			var answerJSON any
			if answer != nil {
				encoded, err := json.Marshal(answer)
				if err != nil {
					return fmt.Errorf("ask %s: encode its answer: %w", ask.ID, err)
				}
				answerJSON = string(encoded)
			}
			// The row says in its own insert that the store holds what the seed states of the
			// ask, as the server's ask create does (asks.tuples_stored_at), at its created_at.
			if _, err := tx.Exec(ctx, `
				insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency,
				                  anchor, state, answer, resolution, created_at, kind, block_id, block_artifact_id, tuples_stored_at)
				values ($1, $2, $3, $4::jsonb, $5, $6::jsonb, false, 'med', $7::jsonb, $8, $9::jsonb, $10::jsonb, $11, 'question', $12, $13, $11)
			`, ask.ID, issueKey, artifactID, seedActor, question, string(optionsJSON), anchor, ask.State,
				answerJSON, resolution, ask.CreatedAt, blockID, blockArtifact); err != nil {
				return fmt.Errorf("ask %s: %w", ask.ID, err)
			}
			mentions.add(ask.ID, text.Refs(text.ExtractAt(question, serverURL)), ask.CreatedAt)
			if index < r.sizes.AskFollowers {
				followers = append(followers, []any{ask.ID, ask.Follower, ask.CreatedAt})
				if _, written := followed[ask.Follower]; !written {
					followed[ask.Follower] = struct{}{}
					followerNodes = append(followerNodes,
						graphstore.SessionNode(ask.Follower, seedAuthor, ask.CreatedAt, graphstore.NoEvent))
				}
			}
			opening := model.Ask{
				ID:              ask.ID,
				IssueKey:        issueKey,
				ArtifactID:      artifactID,
				BlockID:         blockID,
				BlockArtifactID: blockArtifact,
				Author:          seedAuthor,
				Kind:            "question",
				Question:        question,
				Options:         options,
				Urgency:         "med",
				Anchor:          anchorValue,
				State:           "open",
				CreatedAt:       ask.CreatedAt,
			}
			payload, err := askOpenedPayload(opening)
			if err != nil {
				return err
			}
			// The parts are read from the ask as its row stands, answer and all, not from the ask
			// as it opened: the index records what a reader of the row finds now. The time is the
			// ask's own, which is the time its row was last written (coalesce(edited_at,
			// created_at)), so a re-index of it would date its parts as this one did.
			stored := opening
			stored.State, stored.Answer = ask.State, answer
			// The seed's transaction joins no write phase of anyone's, so it reads the index the
			// ask holds - none, its row having just been inserted - plans the parts against it
			// and runs the plan's statements at once, as a batch of their own, the way a writer
			// that queues no write phase indexes an item it wrote (refs.IndexItem). The parts
			// are read through a reader of the ask's own (refs.NewReader), as every other write
			// of an ask reads them. A write of the parts alone reconciles the question's own
			// references after them and settles the ask's row of the forms queue there
			// (refs.Replacement.SettlingForms). They are planned as the server's ask create plans
			// them, with the ask's citations (refs.PlanItemReplace) under the node this batch wrote
			// (refs.WrittenItem), so a row they insert holds tuples_stored with the tuple it says
			// the store holds, against the rows the ask holds: none, its row having just been
			// inserted. Those citations are stated with the mention rows' at the end of the batch.
			partIndex, err := refs.ReadItemIndex(ctx, tx, refs.ItemAsk, ask.ID)
			if err != nil {
				return fmt.Errorf("index the parts of ask %s: %w", ask.ID, err)
			}
			reader := refs.NewReader(ctx, tx, stored.Author, "ask", stored.ID, serverURL)
			parts, err := refs.PlanAskParts(partIndex, reader, stored, ask.CreatedAt)
			if err != nil {
				return err
			}
			questionRefs, err := refs.PlanItemReplace(ctx, tx,
				refs.WrittenItem(graphstore.AskNode(ask.ID, r.itemProject(ask.Issue, ask.Document), nil, seedAuthor, ask.CreatedAt)),
				refs.ItemAsk, stored.ID, text.Refs(reader.Mentions(stored.Question)), ask.CreatedAt)
			if err != nil {
				return fmt.Errorf("index the references of ask %s: %w", ask.ID, err)
			}
			questionRefs = questionRefs.SettlingForms()
			questionCited = append(questionCited, questionRefs.Cited().Tuples...)
			if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
				if err := parts.Queue(b); err != nil {
					return err
				}
				questionRefs.Queue(b)
				return nil
			}); err != nil {
				return fmt.Errorf("index the parts of ask %s: %w", ask.ID, err)
			} else if err := stateAtSentinel(ctx, tx, parts.Stated()); err != nil {
				return fmt.Errorf("state the parts of ask %s: %w", ask.ID, err)
			}
			// The index records the parts and leaves their windows to the block-index tick's
			// window arm (refs.queueRecordIndexed), so the seed writes them here as that arm
			// would: a seeded database a server has to window is one it would rewrite.
			if _, err := refs.WriteItemWindows(ctx, tx, refs.ItemAsk, stored.ID); err != nil {
				return fmt.Errorf("write the windows of ask %s: %w", ask.ID, err)
			}
			// Who asked takes part in the ask's thread and keeps its row, and who answered
			// replies in it, as the server's create and answer record them (inboxwrite.Record):
			// the row holds its marker from the insert, at the moment its answer is dated, so the
			// trigger user_threads_ask passes both by. The seed appends no event of its own.
			project := r.itemProject(ask.Issue, ask.Document)
			if err := inboxwrite.Record(ctx, tx, inboxwrite.Opened(ask.ID, seedAuthor, ask.CreatedAt, issueKey, artifactID, project), graphstore.NoEvent); err != nil {
				return fmt.Errorf("record ask %s in its thread: %w", ask.ID, err)
			}
			if answer != nil {
				answered := inboxwrite.Write{
					Arm: inboxwrite.Replies, NodeKind: graphstore.KindAsk, NodeID: ask.ID,
					Author: model.Actor{Kind: "user", ID: answer.User}, Who: seedAuthor, At: answer.At,
					IssueKey: issueKey, ArtifactID: artifactID, Project: project,
				}
				if err := inboxwrite.Record(ctx, tx, answered, graphstore.NoEvent); err != nil {
					return fmt.Errorf("record the answer of ask %s in its thread: %w", ask.ID, err)
				}
			}
			// The event is owned by the ask's own owner and dated at the ask, as one appended
			// with the ask is, and it is published (`notify` false, `published_at` set) like
			// every other event the seed writes.
			opened = append(opened, []any{
				r.askEventIDBase() + int64(index) + 1, issueKey, artifactID, nil, seqs[index],
				"ask.opened", seedActor, payload, false, ask.CreatedAt, ask.CreatedAt,
			})
		}
		if err := writeFollowerNodes(ctx, tx, followerNodes); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ask_followers"},
			[]string{"ask_id", "session_id", "since"}, pgx.CopyFromRows(followers)); err != nil {
			return fmt.Errorf("ask followers: %w", err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"events"}, eventColumns, pgx.CopyFromRows(opened)); err != nil {
			return fmt.Errorf("ask opening events: %w", err)
		}
		cited, err := mentions.write(ctx, tx)
		if err != nil {
			return err
		}
		return stateAtSentinel(ctx, tx, graphstore.Uncaused{Tuples: append(questionCited, cited...)})
	})
}

// stateAtSentinel appends what one of the seed's index passes states in the node and tuple store
// - an item's block nodes, their part_of and links (refs.IndexItem, refs.PlanAskParts), and the
// citations its refs rows are the projection of (mentionRows) - at the sentinel, as every write of
// the seed's is: the seed appends no event of its own. Each pass is stated before the next one of
// the same item runs, so a later pass compares against what the earlier one stated, as two writes
// of the server's do.
func stateAtSentinel(ctx context.Context, tx pgx.Tx, written graphstore.Uncaused) error {
	return graphstore.Caused(ctx, tx, written, graphstore.NoEvent)
}

// itemProject is the project of an item of the plan: its issue's, or its document's where it
// belongs to no issue.
func (r *seedRun) itemProject(issue, document int) string {
	if issue >= 0 {
		return r.plan.projects[r.plan.issues[issue].Project].Key
	}
	if document >= 0 {
		return r.plan.projects[r.plan.documents[document].Project].Key
	}
	return ""
}

// writeFollowerNodes writes the session nodes of the followers one ask batch copies. A session
// that follows an ask is a node of the store from the write that adds it (asks.Follow), and the
// batch's copy of the follower rows reaches no asks.Follow: so the batch writes each follower's
// node beside that copy, at the sentinel, as every write of the seed's is. A follower an earlier
// batch wrote is written again, so each node restates its session (graphstore.Node's Restates):
// the seed owns no session's state or visibility, so a node written again keeps the ones the
// store holds, and a node the store does not hold yet is made as graphstore.SessionNode states
// it. Without it a seeded ask's followed_by edges would answer no node in any read, and the
// census would report every follower. The nodes go in one graphstore.Write, which takes their
// rows in ascending (kind, id) and appends their base attributes after them; the follower rows
// carry no foreign key into the store, so where the copy falls beside this moves no lock of the
// store's order.
func writeFollowerNodes(ctx context.Context, tx pgx.Tx, nodes []graphstore.Node) error {
	for index := range nodes {
		nodes[index].Restates = true
	}
	if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
		return fmt.Errorf("the ask followers' nodes: %w", err)
	}
	return nil
}

// askAnswer is the answer an answered ask holds, in the shape the answer route writes
// (api/ask_transitions.go): the answerer, the option it chose when the ask offers any, the text
// it was given and its time. The part index reads the stored answer as a model.AskAnswer, takes
// it as a part of its own and records its answerer as that part's maker (refs.askPartMakers), so
// an answer of another shape would index as a part no answer of the server's ever is. Its time is
// the ask's own, the one time every row and event of a seeded ask carries.
func (r *seedRun) askAnswer(ask askPlan, options []model.AskOption) model.AskAnswer {
	selected := []string{}
	if len(options) > 0 {
		selected = []string{options[0].Label}
	}
	given := r.plan.vocabulary.phrase(r.random, 6)
	return model.AskAnswer{User: seedAuthor.ID, Selected: selected, Text: &given, At: ask.CreatedAt}
}

// writeComments writes the comments, indexes each one's blocks with the server's own indexer
// (refs.IndexItem) and writes the comment's own mentions. An edited comment is indexed twice, as
// its write and its edit indexed it: the blocks the edit dropped keep their rows with a null
// position.
//
// A reply names the block it answers in replies_to_block, as the create route writes it
// (api/reply_blocks.go): a block of the comment it replies to, or the part of the ask it answers,
// whose node id starts `ask/`. The view reads the two as edges to a comment_block and to an
// ask_block, telling them apart by that prefix. What a comment answers is stated in the store as
// the server's comment write states it (graphstore.CommentPointerFacts), and its row says so in
// tuples_stored, so the view answers those edges from the tuples.
//
// Who wrote a comment is written as the server's comment write writes it (api/comment_create.go).
// The row holds the author as the model encodes it, which is the value the comment's node holds
// as made_by, and the batch states what graphstore.QueueAuthored states of each comment
// (graphstore.AuthorFacts): the author's node, the comment's authored_by tuple to it and the
// aliases the server proved of the author (commentStoreWrite). The store census holds each
// comment row to its node and its authored_by, so a seeded comment answers it as a comment the
// server wrote does. seedActor's display name is a key no model.Actor carries, so a row holding
// seedActor would disagree with its own node.
func (r *seedRun) writeComments(ctx context.Context) error {
	var hasAnswers bool
	if err := r.database.Pool.QueryRow(ctx, `select exists (
		select 1 from information_schema.columns
		where table_schema = current_schema() and table_name = 'comments' and column_name = 'answers'
	)`).Scan(&hasAnswers); err != nil {
		return fmt.Errorf("read whether comments carries answers: %w", err)
	}
	columns := "id, issue_key, artifact_id, author, body, anchor, reply_to, resolved, created_at, ask_id, replies_to_block, edited_at"
	parameters := "$1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9, $10, $11, $12"
	next := 13
	if hasAnswers {
		columns += ", answers"
		parameters += fmt.Sprintf(", $%d", next)
		next++
	}
	// The address of the comment's own node, which the batch writes before its first row, whether
	// the batch stated its pointers there, and that the seed records the comment in its thread, as
	// the server's comment write says of each comment it writes (threads_recorded).
	columns += ", node_kind, node_address, tuples_stored, threads_recorded"
	parameters += fmt.Sprintf(", $%d, $%d, $%d, true", next, next+1, next+2)
	insert := "insert into comments (" + columns + ") values (" + parameters + ")"
	author, err := json.Marshal(seedAuthor)
	if err != nil {
		return fmt.Errorf("encode the comments' author: %w", err)
	}
	// AuthorFacts states nothing of an author that names nobody, so a comment of such an author
	// would name an author on its row and hold no authored_by. The seed refuses that author once,
	// before its first batch, and commentStoreWrite relies on it.
	if _, _, ok := graphstore.AuthorOf(seedAuthor); !ok {
		return fmt.Errorf("the seed's author %s names nobody the store can name", seedAuthor.ID)
	}
	return r.inBatches(ctx, "comments", len(r.plan.comments), batchItems, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		// The nodes the batch's rows project, their author's node, and what each of them
		// states, written before any row names one: the composite foreign key on
		// (node_kind, node_address) is checked inside the statement that writes the row, so a
		// node written after it would be too late. The seed appends no event of its own, so
		// all of it takes the sentinel. Each row then says in its own insert whether its
		// pointers are stated (tuples_stored), as the server's comment write does.
		nodes, tuples, pointersOf, pointersStored := r.commentStoreWrite(from, to, hasAnswers)
		if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
			return fmt.Errorf("write the comment nodes of %d to %d: %w", from, to, err)
		}
		mentions := newMentionRows("comment")
		for index := from; index < to; index++ {
			comment := r.plan.comments[index]
			mention := ""
			if comment.MentionTo >= 0 {
				mention = r.plan.mentionText(comment.MentionTo, comment.MentionNth)
			}
			body := r.plan.itemBody(r.random, comment.Blocks, mention, itemBodyBytes)
			var issueKey, artifactID, anchor *string
			var editedAt any
			if comment.Issue >= 0 {
				issueKey = &r.plan.issues[comment.Issue].Key
			}
			if comment.Document >= 0 {
				artifactID = &r.plan.documents[comment.Document].ID
			}
			if comment.Anchored {
				document := r.plan.documents[comment.Document]
				block := firstParagraphBlock
				if comment.TableRow {
					block = r.tableRow
				}
				value := anchorJSON(document.ID, block, document.Versions, r.plan.vocabulary.phrase(r.random, 4))
				anchor = &value
			}
			pointers := pointersOf[index-from]
			if comment.Edited {
				editedAt = comment.CreatedAt.Add(time.Hour)
			}
			arguments := []any{comment.ID, issueKey, artifactID, author, body, anchor, textOrNull(pointers.ReplyTo), index%5 == 0,
				comment.CreatedAt, textOrNull(pointers.AskID), textOrNull(pointers.RepliesToBlock), editedAt}
			if hasAnswers {
				arguments = append(arguments, textOrNull(pointers.Answers))
			}
			arguments = append(arguments, graphstore.KindComment, comment.ID, pointersStored[index-from])
			if _, err := tx.Exec(ctx, insert, arguments...); err != nil {
				return fmt.Errorf("comment %s: %w", comment.ID, err)
			}
			if comment.Edited {
				// The body before the edit held one block more. Indexing it first and the
				// stored body second is the order the write and the edit ran in, and it leaves
				// the dropped block with a null position.
				longer := body + "\n\n" + r.plan.vocabulary.phrase(r.random, 12) + "."
				if _, indexed, err := refs.IndexItem(ctx, tx, refs.ItemComment, comment.ID, longer, serverURL,
					comment.CreatedAt, text.ExtractAt(longer, serverURL), nil); err != nil {
					return fmt.Errorf("index comment %s before its edit: %w", comment.ID, err)
				} else if err := stateAtSentinel(ctx, tx, indexed); err != nil {
					return fmt.Errorf("state comment %s before its edit: %w", comment.ID, err)
				}
			}
			at := comment.CreatedAt
			if comment.Edited {
				at = comment.CreatedAt.Add(time.Hour)
			}
			located := text.ExtractAt(body, serverURL)
			if _, indexed, err := refs.IndexItem(ctx, tx, refs.ItemComment, comment.ID, body, serverURL, at,
				located, nil); err != nil {
				return fmt.Errorf("index comment %s: %w", comment.ID, err)
			} else if err := stateAtSentinel(ctx, tx, indexed); err != nil {
				return fmt.Errorf("state comment %s: %w", comment.ID, err)
			}
			// The windows the index no longer writes, as the tick's window arm writes them
			// (refs.WriteItemWindows): one set, over the body the comment is left holding.
			if _, err := refs.WriteItemWindows(ctx, tx, refs.ItemComment, comment.ID); err != nil {
				return fmt.Errorf("write the windows of comment %s: %w", comment.ID, err)
			}
			// Who wrote it takes part in its thread and keeps the thread's row, and the comment
			// reaches everyone else in that thread, as the server's comment write records it
			// (inboxwrite.Record): the row holds threads_recorded from its insert, so the trigger
			// user_threads_comment passes it by. A comment whose pointers the batch could not state
			// is recorded as the server records one, its thread's rows alone
			// (inboxwrite.Write.Unstored), naming the head no pointer tuple names. The seed appends
			// no event of its own.
			written := inboxwrite.Comment(comment.ID, seedAuthor, comment.CreatedAt, issueKey, artifactID,
				r.itemProject(comment.Issue, comment.Document))
			if !pointersStored[index-from] {
				switch {
				case pointers.AskID != "":
					written = written.RepliesTo(graphstore.KindAsk, pointers.AskID)
				case pointers.ReplyTo != "":
					written = written.RepliesTo(graphstore.KindComment, pointers.ReplyTo)
				}
				written = written.Unstored()
			}
			if err := inboxwrite.Record(ctx, tx, written, graphstore.NoEvent); err != nil {
				return fmt.Errorf("record comment %s in its thread: %w", comment.ID, err)
			}
			mentions.add(comment.ID, text.Refs(located), comment.CreatedAt)
		}
		cited, err := mentions.write(ctx, tx)
		if err != nil {
			return err
		}
		return stateAtSentinel(ctx, tx, graphstore.Uncaused{Tuples: cited})
	})
}

// commentStoreWrite is what the comments from..to are in the node and tuple store: each
// comment's node - project-visible, live, made by the seed's author at the row's own time - and
// the artifact the comment is anchored to and the artifact that owns it, each stated only where
// the comment has one (graphstore.CommentFacts), what the comment answers
// (graphstore.CommentPointerFacts, over the pointers its row holds, commentPointers), and who
// wrote the comment. The node write states the two base attributes every node carries
// (graphstore.Write). All of it is drawn from the plan alone, so the batch can state it before it
// writes a row. pointers are, for each comment from from on, the pointers its row holds, and
// pointersStored whether they are stated, which is what its row's tuples_stored holds.
//
// Who wrote a comment is what graphstore.AuthorFacts states of it, which is what
// graphstore.QueueAuthored writes for a comment of the server's: the comment's authored_by tuple
// and the aliases the server proved of its author go among the comment's own tuples, and the
// author's node goes among the batch's nodes once, as AuthorFacts makes it for the batch's first
// comment. The batch's one graphstore.Write then takes every node row before it appends a tuple,
// which is the store's lock order. Every batch writes the author's node again, so it restates the
// author (graphstore.Node's Restates), as the backfill's comment write does: no comment owns its
// author's state or visibility, so a later batch keeps the ones the store holds, and only the
// first batch makes the author's node, at the time of the first comment the seed writes, as
// QueueAuthored makes it at the first comment that names it. writeComments refused an author that
// names nobody before its first batch, so every comment here names one.
func (r *seedRun) commentStoreWrite(from, to int, hasAnswers bool) (nodes []graphstore.Node, tuples []graphstore.Tuple, pointers []graphstore.CommentPointers, pointersStored []bool) {
	nodes = make([]graphstore.Node, 0, to-from+1)
	tuples = make([]graphstore.Tuple, 0, 7*(to-from))
	pointers = make([]graphstore.CommentPointers, 0, to-from)
	pointersStored = make([]bool, 0, to-from)
	var authors []graphstore.Node
	for index := from; index < to; index++ {
		comment := r.plan.comments[index]
		// A seeded comment is anchored only to the document it belongs to, which is the
		// document whose address its row's artifact_id names.
		anchor, owner := "", ""
		if comment.Document >= 0 {
			owner = r.plan.documents[comment.Document].RefKey
			if comment.Anchored {
				anchor = owner
			}
		}
		var node graphstore.Node
		node, tuples = graphstore.CommentFacts(tuples, comment.ID, seedAuthor, comment.CreatedAt, graphstore.NoEvent, anchor, owner,
			r.itemProject(comment.Issue, comment.Document), nil)
		nodes = append(nodes, node)
		held := r.commentPointers(comment, hasAnswers)
		var stored bool
		tuples, stored = graphstore.CommentPointerFacts(tuples, comment.ID, held, seedAuthor, comment.CreatedAt, graphstore.NoEvent)
		pointers = append(pointers, held)
		pointersStored = append(pointersStored, stored)
		made, facts, _ := graphstore.AuthorFacts(graphstore.KindComment, comment.ID, seedAuthor, comment.CreatedAt, graphstore.NoEvent)
		if index == from {
			authors = made
		}
		tuples = append(tuples, facts...)
	}
	nodes = append(nodes, restated(authors)...)
	return nodes, tuples, pointers, pointersStored
}

// restated marks each of nodes as restated (graphstore.Node's Restates) and answers them: the
// author's node, which every batch of the seed writes again, and whose state and visibility no
// item it wrote owns, so a later batch keeps the ones the store holds.
func restated(nodes []graphstore.Node) []graphstore.Node {
	for index := range nodes {
		nodes[index].Restates = true
	}
	return nodes
}

// commentPointers is what comment's row holds of what it answers, as writeComments writes it: the
// comment it replies to, the ask its thread hangs under, the block it names - the first block of
// the comment it replies to by block, or the question of its ask - and the comment it answers,
// which is the one whose block it names where it names one and else the one it replies to, as the
// create route keeps the comment a reply names (api's threadHeadOf). On a schema whose comments
// carry no answers column (hasAnswers) the row holds none, so neither does this.
func (r *seedRun) commentPointers(comment commentPlan, hasAnswers bool) graphstore.CommentPointers {
	var pointers graphstore.CommentPointers
	if comment.ReplyTo >= 0 {
		pointers.ReplyTo = r.plan.comments[comment.ReplyTo].ID
	}
	if comment.Ask >= 0 {
		pointers.AskID = r.plan.asks[comment.Ask].ID
	}
	if comment.RepliesTo >= 0 {
		pointers.RepliesToBlock = "comment/" + r.plan.comments[comment.RepliesTo].ID + "#b1"
	}
	if comment.AskPart {
		pointers.RepliesToBlock = "ask/" + r.plan.asks[comment.Ask].ID + "#" + askQuestionBlock
	}
	if hasAnswers {
		pointers.Answers = pointers.ReplyTo
		if comment.RepliesTo >= 0 {
			pointers.Answers = r.plan.comments[comment.RepliesTo].ID
		}
	}
	return pointers
}

// textOrNull is value as a nullable column takes it: null for "", which is how
// graphstore.CommentPointers and graphstore.MessageRow spell a column the row holds null in.
func textOrNull(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// writeMessages writes the issue threads and indexes each message's blocks. Every fiftieth
// message is indexed twice, the first time from a body one block longer: that is a reindex under
// a parser reading the same text as fewer blocks, which leaves a message block with a null
// position - the state the view's filter on message_blocks is written for.
func (r *seedRun) writeMessages(ctx context.Context) error {
	return r.inBatches(ctx, "messages", len(r.plan.messages), batchItems, func(ctx context.Context, tx pgx.Tx, from, to int) error {
		mentions := newMentionRows("message")
		// Each message's node goes in before any row of the batch, in its issue's project, as the
		// server's message create writes it; every seeded message is on an issue. Beside it go
		// its pointers (graphstore.MessageFacts) and who wrote it, which graphstore.AuthorFacts
		// states as graphstore.QueueAuthored does for a message of the server's, the author's node
		// once and restated, as commentStoreWrite gives a batch of comments theirs. Each row then
		// names its node in its insert, as the server's does. The issues' owner locks come first,
		// as for the asks (writeAsks): the window write takes them.
		nodes := make([]graphstore.Node, 0, to-from+1)
		tuples := make([]graphstore.Tuple, 0, 3*(to-from))
		var authors []graphstore.Node
		rows := make([]graphstore.MessageRow, 0, to-from)
		projected := make([]bool, 0, to-from)
		owners := make([]store.Owner, 0, to-from)
		for index := from; index < to; index++ {
			message := r.plan.messages[index]
			row := graphstore.MessageRow{ID: message.ID, IssueKey: r.plan.issues[message.Issue].Key}
			if message.InReplyTo >= 0 {
				row.InReplyTo = r.plan.messages[message.InReplyTo].ID
			}
			if message.RepliesTo >= 0 {
				row.RepliesToBlock = "message/" + r.plan.messages[message.RepliesTo].ID + "#b1"
			}
			nodes = append(nodes, graphstore.MessageNode(message.ID, r.itemProject(message.Issue, -1), nil, seedAuthor, message.CreatedAt))
			var stated bool
			nodes, tuples, stated = graphstore.MessageFacts(nodes, tuples, row, seedAuthor, message.CreatedAt, graphstore.NoEvent)
			if stated {
				made, facts, _ := graphstore.AuthorFacts(graphstore.KindMessage, message.ID, seedAuthor, message.CreatedAt, graphstore.NoEvent)
				if authors == nil {
					authors = made
				}
				tuples = append(tuples, facts...)
			}
			rows = append(rows, row)
			projected = append(projected, stated)
			owners = append(owners, store.IssueOwner(row.IssueKey))
		}
		if err := store.LockOwners(ctx, tx, owners...); err != nil {
			return fmt.Errorf("the messages' owners: %w", err)
		}
		if err := graphstore.Write(ctx, tx, append(nodes, restated(authors)...), tuples); err != nil {
			return fmt.Errorf("the messages' nodes: %w", err)
		}
		for index := from; index < to; index++ {
			message := r.plan.messages[index]
			row := rows[index-from]
			body := r.plan.itemBody(r.random, message.Blocks, r.plan.messageMention(index), itemBodyBytes)
			var nodeKind, nodeAddress *string
			if projected[index-from] {
				kind := graphstore.KindMessage
				nodeKind, nodeAddress = &kind, &row.ID
			}
			if _, err := tx.Exec(ctx, `
				insert into messages (id, issue_key, author, body, created_at, in_reply_to, replies_to_block, node_kind, node_address, threads_recorded)
				values ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, true)
			`, message.ID, row.IssueKey, seedActor, body, message.CreatedAt,
				textOrNull(row.InReplyTo), textOrNull(row.RepliesToBlock), nodeKind, nodeAddress); err != nil {
				return fmt.Errorf("message %s: %w", message.ID, err)
			}
			if message.Reindexed {
				longer := body + "\n\n" + r.plan.vocabulary.phrase(r.random, 12) + "."
				if _, indexed, err := refs.IndexItem(ctx, tx, refs.ItemMessage, message.ID, longer, serverURL,
					message.CreatedAt, text.ExtractAt(longer, serverURL), nil); err != nil {
					return fmt.Errorf("index message %s under the older parser: %w", message.ID, err)
				} else if err := stateAtSentinel(ctx, tx, indexed); err != nil {
					return fmt.Errorf("state message %s under the older parser: %w", message.ID, err)
				}
			}
			located := text.ExtractAt(body, serverURL)
			if _, indexed, err := refs.IndexItem(ctx, tx, refs.ItemMessage, message.ID, body, serverURL,
				message.CreatedAt, located, nil); err != nil {
				return fmt.Errorf("index message %s: %w", message.ID, err)
			} else if err := stateAtSentinel(ctx, tx, indexed); err != nil {
				return fmt.Errorf("state message %s: %w", message.ID, err)
			}
			if _, err := refs.WriteItemWindows(ctx, tx, refs.ItemMessage, message.ID); err != nil {
				return fmt.Errorf("write the windows of message %s: %w", message.ID, err)
			}
			// Who wrote it takes part in its thread and keeps the thread's row, and the message
			// reaches everyone else in that thread, as the server's message write records it
			// (inboxwrite.Record): the row holds threads_recorded from its insert, so the trigger
			// user_threads_message passes it by. A message whose row names no node is recorded as the
			// server records one, its thread's rows alone (inboxwrite.Write.Unstored), climbing from
			// the message it answers, which no replies_to tuple of its own names.
			written := inboxwrite.Message(message.ID, seedAuthor, message.CreatedAt, &row.IssueKey,
				r.itemProject(message.Issue, -1))
			if !projected[index-from] {
				if row.InReplyTo != "" {
					written = written.RepliesTo(graphstore.KindMessage, row.InReplyTo)
				}
				written = written.Unstored()
			}
			if err := inboxwrite.Record(ctx, tx, written, graphstore.NoEvent); err != nil {
				return fmt.Errorf("record message %s in its thread: %w", message.ID, err)
			}
			mentions.add(message.ID, text.Refs(located), message.CreatedAt)
		}
		cited, err := mentions.write(ctx, tx)
		if err != nil {
			return err
		}
		return stateAtSentinel(ctx, tx, graphstore.Uncaused{Tuples: cited})
	})
}

// mentionRows collects the `refs` rows one batch of items writes. The targets are read out of
// each body by text.ExtractAt and refs.TargetOf, the same reading the server's own indexer makes,
// so the rows are the rows a write of that body would leave.
type mentionRows struct {
	kind                string
	ids, kinds, targets []string
	times               []time.Time
	seen                map[[3]string]struct{}
}

func newMentionRows(kind string) *mentionRows {
	return &mentionRows{kind: kind, seen: map[[3]string]struct{}{}}
}

// add collects the rows mentioned leaves for the item id, which is text.ExtractAt's reading of
// the item's body read as references: the caller already made that reading for the indexer, so
// the body is scanned once.
func (m *mentionRows) add(id string, mentioned []text.Ref, at time.Time) {
	for _, ref := range mentioned {
		target, ok := refs.TargetOf(ref)
		if !ok {
			continue
		}
		key := [3]string{id, target.Kind, target.ID}
		if _, held := m.seen[key]; held {
			continue
		}
		m.seen[key] = struct{}{}
		m.ids = append(m.ids, id)
		m.kinds = append(m.kinds, target.Kind)
		m.targets = append(m.targets, target.ID)
		m.times = append(m.times, at)
	}
}

// write inserts the rows and answers the citation the server's own writer of an item's refs rows
// states beside each row it inserts (refs.replaceRefs): a `mentions` tuple, of the row's own kind,
// out of the item's node to the row's far end, dated by the row and made by the seed's author. The
// batch wrote every item's node before its rows, so each row is a citation of the store's and says
// so in its own insert (refs.tuples_stored), but for a far end past the store's address bound,
// which is no tuple and holds false.
func (m *mentionRows) write(ctx context.Context, tx pgx.Tx) ([]graphstore.UncausedTuple, error) {
	if len(m.ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, created_at, tuples_stored)
		select $1, r.id, r.kind, r.target, r.at, octet_length(r.target) <= $6
		from unnest($2::text[], $3::text[], $4::text[], $5::timestamptz[]) as r (id, kind, target, at)
		on conflict (from_kind, from_id, to_kind, to_id) do nothing
		returning from_id, kind, to_kind, to_id, created_at, tuples_stored
	`, m.kind, m.ids, m.kinds, m.targets, m.times, graphstore.MaxAddressBytes)
	if err != nil {
		return nil, fmt.Errorf("write %s mentions: %w", m.kind, err)
	}
	var cited []graphstore.UncausedTuple
	for rows.Next() {
		var tuple graphstore.Tuple
		var stored bool
		if err := rows.Scan(&tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID, &tuple.At, &stored); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the %s mentions written: %w", m.kind, err)
		}
		if !stored {
			continue
		}
		tuple.FromKind, tuple.Who, tuple.Asserted = m.kind, seedAuthor, true
		cited = append(cited, graphstore.UncausedTuple{Tuple: tuple})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("write %s mentions: %w", m.kind, err)
	}
	return cited, nil
}
