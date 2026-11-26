package docs

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// issueRoom is a converted issue of the test store (createIssueRoom): its tree artifact, whose
// Yjs document is the issue's room, and the three documents converted into it.
type issueRoom struct {
	issue                    string
	tree                     string
	spec, verification, plan string
}

// createIssueRoom writes, by SQL, an issue DOC-<number> converted into its room (R5 and R6 of
// the issue-tree spec): its spec, verification-md and plan-md documents, each with a first
// version and no stored room history of its own; its tree artifact, of kind tree, with an empty
// first version as every document is created with; its issue_trees row; a document_rooms row from
// each document to the tree; and the room's first stored update, as a conversion's seed writes
// it, holding planSeed in the plan's fragment. api's createIssueTree writes the tree artifact with
// its node, which no test of this package can call (api imports docs), and no read here reads the
// tree's node.
func createIssueRoom(t *testing.T, database *store.Store, number int) issueRoom {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin create the issue room: %v", err)
	}
	defer tx.Rollback(ctx)
	room := issueRoom{issue: "DOC-" + strconv.Itoa(number)}
	if _, err := tx.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice')
		on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("create test project: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'DOC', $2, 'Issue room', '{"kind":"user","id":"alice"}', 'U')
	`, room.issue, number); err != nil {
		t.Fatalf("create test issue: %v", err)
	}
	create := func(slug, name, kind string, primary bool) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, 'DOC', $2, $3, $4, $5, '{"kind":"user","id":"alice"}')
			returning id::text
		`, room.issue, slug, name, kind, primary).Scan(&id); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, '', '[{"kind":"user","id":"alice"}]')
		`, id); err != nil {
			t.Fatalf("create the first version of %s: %v", slug, err)
		}
		return id
	}
	room.spec = create("spec", "spec.md", "doc", true)
	room.verification = create("verification-md", "verification.md", "doc", false)
	room.plan = create("plan-md", "plan.md", "doc", false)
	room.tree = create("tree", "tree", "tree", false)
	if _, err := tx.Exec(ctx, `insert into issue_trees (issue_key, artifact_id) values ($1, $2)`, room.issue, room.tree); err != nil {
		t.Fatalf("record the issue's tree: %v", err)
	}
	for _, document := range []string{room.spec, room.verification, room.plan} {
		if _, err := tx.Exec(ctx, `insert into document_rooms (artifact_id, room) values ($1, $2)`, document, room.tree); err != nil {
			t.Fatalf("move %s into the issue room: %v", document, err)
		}
	}
	seed := crdt.New()
	seed.Transact(writeParagraph(seed, room.plan, "Plan seed."))
	if _, err := tx.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)
	`, room.tree, crdt.EncodeStateAsUpdateV1(seed, nil)); err != nil {
		t.Fatalf("seed the issue room: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the issue room: %v", err)
	}
	return room
}

// planSeed is what createIssueRoom seeds the plan's fragment of the issue room with.
const planSeed = "Plan seed.\n"

// writeParagraph is one keystroke's transaction that types text into a new last paragraph of the
// fragment named fragment, as a browser's editor bound to that fragment types it.
func writeParagraph(document *crdt.Doc, fragment, text string) func(*crdt.Transaction) {
	held := document.GetXmlFragment(fragment)
	return func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		held.InsertElement(txn, held.Len(), paragraph)
		run := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, run)
		run.Insert(txn, 0, text, nil)
	}
}

// fragmentMarkdown renders the fragment named fragment of a peer's copy of its room, as the
// browser's editor bound to it shows it.
func fragmentMarkdown(fragment string) func(*docstest.Peer) string {
	return func(peer *docstest.Peer) string {
		held := peer.Doc.GetXmlFragment(fragment)
		var rendered string
		peer.Doc.Transact(func(txn *crdt.Transaction) {
			tree, err := pmdoc.ReadInTransaction(txn, held)
			if err != nil {
				return
			}
			rendered, _ = pmdoc.Render(tree)
		}, nil)
		return rendered
	}
}

// roomUpdateCount is how many doc_updates rows artifactID's room holds.
func roomUpdateCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&rows); err != nil {
		t.Fatalf("count the stored updates of %s: %v", artifactID, err)
	}
	return rows
}

// waitForStoredUpdates waits until artifactID's room holds more than above stored updates: a
// peer's update has reached the room and its append has landed, so the room owes a settlement.
func waitForStoredUpdates(t *testing.T, database *store.Store, artifactID string, above int) {
	t.Helper()
	deadline := time.Now().Add(roomWait(t))
	for roomUpdateCount(t, database, artifactID) <= above {
		if time.Now().After(deadline) {
			t.Fatalf("the room %s holds no more than %d stored updates: the peer's update never landed", artifactID, above)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two people editing two documents of one converted issue edit one room (R5): a socket opened on
// the spec's uuid and one on the Verification document's are both admitted to the issue's room,
// which holds one place under the room cap, and each frame they read names the document they asked
// for (docstest.Peer passes over any other, as a browser's provider does). Each writes into its own
// document's fragment, both writes stand - each peer's copy holds the other's, each document reads
// back its own, the room's stored history is the tree artifact's and holds both - and the room's
// settlement writes one version of each document whose fragment changed, holding that fragment
// alone, and none of the tree artifact, whose own fragment holds nothing; the room's blocks name
// their own documents in issue_room_blocks.
func TestTwoSocketsThroughTwoDocumentsOfAnIssueEditOneRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)

	spec := connectPeer(t, server.URL, room.spec, "alice")
	verification := connectPeer(t, server.URL, room.verification, "bob")
	if _, err := spec.Send(writeParagraph(spec.Doc, room.spec, "Spec words.")); err != nil {
		t.Fatalf("write into the spec's fragment: %v", err)
	}
	if _, err := verification.Send(writeParagraph(verification.Doc, room.verification, "Check words.")); err != nil {
		t.Fatalf("write into the Verification document's fragment: %v", err)
	}
	for _, read := range []struct {
		peer     *docstest.Peer
		fragment string
		want     string
	}{
		{spec, room.verification, "Check words.\n"},
		{verification, room.spec, "Spec words.\n"},
		{spec, room.spec, "Spec words.\n"},
		{verification, room.verification, "Check words.\n"},
	} {
		if err := read.peer.WaitForText(fragmentMarkdown(read.fragment), read.want, roomWait(t)); err != nil {
			t.Fatalf("a peer's copy of the issue room: %v", err)
		}
	}

	if service.srv.GetDoc(room.tree) == nil {
		t.Fatalf("no room of the tree artifact %s is resident: the sockets were not admitted to the issue's room", room.tree)
	}
	for _, document := range []string{room.spec, room.verification} {
		if service.srv.GetDoc(document) != nil {
			t.Errorf("a room of the document %s is resident beside the issue's room", document)
		}
	}
	service.roomSlots.mu.Lock()
	places := make([]string, 0, len(service.roomSlots.rooms))
	for name := range service.roomSlots.rooms {
		places = append(places, name)
	}
	service.roomSlots.mu.Unlock()
	if !slices.Equal(places, []string{room.tree}) {
		t.Errorf("the room cap holds places for %v, want the issue's room %s alone", places, room.tree)
	}
	for document, want := range map[string]string{room.spec: "Spec words.\n", room.verification: "Check words.\n", room.plan: planSeed} {
		if got, err := service.Text(ctx, document); err != nil || got != want {
			t.Errorf("the text of %s is %q (%v), want %q, its own fragment of the room", document, got, err, want)
		}
	}

	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}
	if rows := roomUpdateCount(t, database, room.tree); rows == 0 {
		t.Errorf("the tree artifact's room holds no stored update: the writes were not stored under the issue's room")
	}
	for _, document := range []string{room.spec, room.verification} {
		if rows := roomUpdateCount(t, database, document); rows != 0 {
			t.Errorf("the document %s's own room holds %d stored updates, want none", document, rows)
		}
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle the issue's room: %v", err)
	}
	for document, want := range map[string]struct {
		number   int
		markdown string
	}{
		room.spec: {2, "Spec words.\n"}, room.verification: {2, "Check words.\n"}, room.plan: {2, planSeed}, room.tree: {1, ""},
	} {
		var number int
		var markdown string
		if err := database.Pool.QueryRow(ctx, `
			select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
		`, document).Scan(&number, &markdown); err != nil {
			t.Fatalf("read the latest version of %s: %v", document, err)
		}
		if number != want.number || markdown != want.markdown {
			t.Errorf("the latest version of %s is %d holding %q, want %d holding %q: its own fragment", document, number, markdown, want.number, want.markdown)
		}
	}
	rows, err := database.Pool.Query(ctx, `
		select r.document_id::text from issue_room_blocks r
		join document_blocks b on b.artifact_id = r.tree_artifact_id and b.block_id = r.block_id
		where r.tree_artifact_id = $1 order by r.document_id`, room.tree)
	if err != nil {
		t.Fatalf("read the room's blocks: %v", err)
	}
	var documents []string
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			t.Fatalf("read a room block: %v", err)
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the room's blocks: %v", err)
	}
	want := []string{room.spec, room.verification, room.plan}
	slices.Sort(want)
	if !slices.Equal(documents, want) {
		t.Errorf("the room's blocks name the documents %v, want one block of each document holding one %v", documents, want)
	}
}

// A socket held on a document's own room when that document's document_rooms row lands is refused
// its next write (ErrRoomMoved): nothing more is stored in the old room's rows, the socket's room
// is closed, and the browser's next socket on the same uuid is admitted to the issue's room.
func TestAHeldSocketOnAnOldRoomIsRefusedOnceItsDocumentMoves(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	document := createIssueDocument(t, database, 1, "Before.")
	seedServiceText(t, service, document, "Before.\n")
	held := connectPeer(t, server.URL, document, "alice")
	if err := held.WaitForText(peerMarkdown, "Before.\n", roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if err := service.waitForDurableAppends(ctx, document); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}
	before := roomUpdateCount(t, database, document)

	var tree string
	if err := database.Pool.QueryRow(ctx, `
		with tree as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('DOC-1', 'DOC', 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
			returning id
		), held as (
			insert into issue_trees (issue_key, artifact_id) select 'DOC-1', id from tree
		), moved as (
			insert into document_rooms (artifact_id, room) select $1::uuid, id from tree
		)
		select id::text from tree`, document).Scan(&tree); err != nil {
		t.Fatalf("move the document into its issue's room: %v", err)
	}

	if _, err := held.Send(writeParagraph(held.Doc, fragmentName, "After.")); err != nil {
		t.Fatalf("type on the held socket: %v", err)
	}
	select {
	case <-held.Ended:
	case <-time.After(roomWait(t)):
		t.Fatal("the held socket on the old room was not closed after its write met the moved room")
	}
	if after := roomUpdateCount(t, database, document); after != before {
		t.Errorf("the old room holds %d stored updates after the move, want the %d it held before", after, before)
	}
	again := connectPeer(t, server.URL, document, "alice")
	if err := again.WaitForText(fragmentMarkdown(document), "", roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if service.srv.GetDoc(tree) == nil {
		t.Errorf("the socket opened on %s after the move was not admitted to the issue's room %s", document, tree)
	}
}

// A schema version change goes through the graph-store spec's schema switch (c-one-room): with a
// client held on the issue's room through each of its ways in - the tree artifact's uuid and each
// converted document's - the installed version moves, every document socket of the room closes with
// the code a browser reads as "the schema moved", each client connects again and is admitted to the
// issue's room at the installed version, and no editor of the room is left read-only.
func TestASchemaSwitchClosesEverySocketOfTheIssueRoom(t *testing.T) {
	ctx := context.Background()
	pmdoctest.UseBlockSchema(t, installedSchema(t))
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	built := pmdoc.BuiltSchemaVersion()
	if err := schemanodes.InstallFromStore(ctx, database.Pool); err != nil {
		t.Fatalf("install the store's schema at boot: %v", err)
	}
	ways := []string{room.tree, room.spec, room.verification, room.plan}
	held := make([]*docstest.Peer, len(ways))
	for index, way := range ways {
		held[index] = connectPeer(t, server.URL, way, "alice")
		if err := held[index].WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
			t.Fatal(err)
		}
	}
	if service.srv.GetDoc(room.tree) == nil {
		t.Fatalf("the sockets were not admitted to the issue's room %s", room.tree)
	}

	wake := make(chan struct{}, 1)
	installing, stopInstalling := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		(&schemanodes.Installer{
			Store:    database.Pool,
			Wake:     wake,
			OnSwitch: func() { service.CloseDocumentSockets() },
		}).Run(installing)
	}()
	testcleanup.Add(t, func() {
		stopInstalling()
		<-stopped
	})
	moveStoreAboveTheBuild(t, database, built)
	wake <- struct{}{}

	for index, peer := range held {
		select {
		case <-peer.Ended:
		case <-time.After(roomWait(t)):
			t.Fatalf("the socket held on %s was not closed by the schema switch", ways[index])
		}
		if code := peer.CloseCode(); code != documentSchemaCloseCode {
			t.Errorf("the socket held on %s ended with %d, want the close %d the browser reads as a schema switch", ways[index], code, documentSchemaCloseCode)
		}
	}
	if pmdoc.SchemaVersion() != built {
		t.Fatalf("the installed schema is version %d, want this build's %d", pmdoc.SchemaVersion(), built)
	}
	for _, way := range ways {
		again := connectPeer(t, server.URL, way, "alice")
		if err := again.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
			t.Fatal(err)
		}
		scope, err := again.Authenticate(strconv.Itoa(pmdoc.SchemaVersion()), roomWait(t))
		if err != nil {
			t.Fatalf("authenticate the socket opened again on %s: %v", way, err)
		}
		if scope != "read-write" {
			t.Errorf("the socket opened again on %s at the installed version is %q, want an editor that writes", way, scope)
		}
	}
	if service.srv.GetDoc(room.tree) == nil {
		t.Errorf("the sockets opened again were not admitted to the issue's room %s", room.tree)
	}
	for _, document := range []string{room.spec, room.verification, room.plan} {
		if service.srv.GetDoc(document) != nil {
			t.Errorf("a room of the document %s is resident beside the issue's room", document)
		}
	}
}

// The conversion's commit closes the old rooms on every live process (AnnounceDocumentRooms): a
// socket held on a document's own room on each of two processes ends once the transaction that
// writes the document's document_rooms row and announces it commits, and the rows that room
// stored stay as they were.
func TestAMoveAnnouncedAtItsCommitClosesTheOldRoomOnEveryProcess(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	taskA, serverA := newTestServiceInstance(t, database)
	taskB, serverB := newTestServiceInstance(t, second)
	runRoomSync(t, newRoomSync(taskA))
	runRoomSync(t, newRoomSync(taskB))
	document := createIssueDocument(t, database, 1, "Before.")
	seedServiceText(t, taskA, document, "Before.\n")
	before := roomUpdateCount(t, database, document)
	peers := []*docstest.Peer{
		connectPeer(t, serverA.URL, document, "alice"),
		connectPeer(t, serverB.URL, document, "bob"),
	}
	for _, peer := range peers {
		if err := peer.WaitForText(peerMarkdown, "Before.\n", roomWait(t)); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the move: %v", err)
	}
	defer tx.Rollback(ctx)
	var tree string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('DOC-1', 'DOC', 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
		returning id::text`).Scan(&tree); err != nil {
		t.Fatalf("create the issue's tree: %v", err)
	}
	if _, err := tx.Exec(ctx, `insert into document_rooms (artifact_id, room) values ($1, $2)`, document, tree); err != nil {
		t.Fatalf("move the document: %v", err)
	}
	if err := AnnounceDocumentRooms(ctx, tx, tree, document); err != nil {
		t.Fatalf("announce the move: %v", err)
	}
	select {
	case <-peers[0].Ended:
		t.Fatal("the socket closed before the move's transaction committed")
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the move: %v", err)
	}
	for index, peer := range peers {
		select {
		case <-peer.Ended:
		case <-time.After(roomWait(t)):
			t.Fatalf("the socket on process %d's old room did not close once the move committed", index)
		}
	}
	// The close ends each socket and then lets the room go once ygo's last caller in it leaves, so
	// the room can still be resident just after its sockets end (waitForRoomGone): each process is
	// waited on until it holds no room of the document's old id.
	for _, task := range []*Service{taskA, taskB} {
		waitFor(t, roomWait(t), "a process to hold no old room of "+document+" after the move", func() bool {
			return task.srv.GetDoc(document) == nil
		})
	}
	if after := roomUpdateCount(t, database, document); after != before {
		t.Errorf("the old room holds %d stored updates after the move, want the %d it held", after, before)
	}
	again := connectPeer(t, serverA.URL, document, "alice")
	if err := again.WaitForText(fragmentMarkdown(document), "", roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(again.UpgradeHeader.Get("Upgrade"), "websocket") {
		t.Errorf("the socket opened again was not upgraded")
	}
	if taskA.srv.GetDoc(tree) == nil {
		t.Errorf("the socket opened again on %s was not admitted to the issue's room %s", document, tree)
	}
}

// roomBlock is one block of an issue room as the store holds it (readRoomBlocks): the document
// its issue_room_blocks row names, the version its document_blocks row and its newest text row
// record, and the far end of each in_document its node states now.
type roomBlock struct {
	document             string
	changed, textVersion int
	inDocument           []string
}

// readRoomBlocks reads every document_blocks row of the tree artifact tree, by block id.
func readRoomBlocks(t *testing.T, database *store.Store, tree string) map[string]roomBlock {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select b.block_id, b.node_id, coalesce(r.document_id::text, ''), b.changed_version,
		       coalesce((select t.version from document_block_texts t
		                 where t.artifact_id = b.artifact_id and t.block_id = b.block_id and t.text is not null
		                 order by t.version desc limit 1), 0)
		from document_blocks b
		left join issue_room_blocks r on r.tree_artifact_id = b.artifact_id and r.block_id = b.block_id
		where b.artifact_id = $1`, tree)
	if err != nil {
		t.Fatalf("read the room's blocks: %v", err)
	}
	type row struct {
		id, node string
		block    roomBlock
	}
	var read []row
	for rows.Next() {
		var held row
		if err := rows.Scan(&held.id, &held.node, &held.block.document, &held.block.changed, &held.block.textVersion); err != nil {
			t.Fatalf("read a room block: %v", err)
		}
		read = append(read, held)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the room's blocks: %v", err)
	}
	blocks := make(map[string]roomBlock, len(read))
	for _, held := range read {
		stated, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, held.node, []string{refs.TupleInDocument})
		if err != nil {
			t.Fatalf("read the document %s is in: %v", held.node, err)
		}
		for _, tuple := range stated {
			held.block.inDocument = append(held.block.inDocument, tuple.ToKind+":"+tuple.ToID)
		}
		blocks[held.id] = held.block
	}
	return blocks
}

// requireRoomBlocks holds every block of room to the documents and versions it is in: each block
// of a document's fragment has its issue_room_blocks row naming that document, one in_document
// to that document's node, and records, as its changed_version and its newest text, the version
// versions names of that document, never a number of the tree artifact's own history; each
// document holds as many blocks as holds names; and no document_blocks row is keyed by a
// converted document.
func requireRoomBlocks(t *testing.T, database *store.Store, room issueRoom, what string, versions, holds map[string]int) {
	t.Helper()
	refKeys := map[string]string{}
	for _, document := range []string{room.tree, room.spec, room.verification, room.plan} {
		var refKey string
		if err := database.Pool.QueryRow(context.Background(), `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
			t.Fatalf("read the ref_key of %s: %v", document, err)
		}
		refKeys[document] = refKey
	}
	held := map[string]int{}
	for id, block := range readRoomBlocks(t, database, room.tree) {
		held[block.document]++
		want, known := versions[block.document]
		if !known {
			t.Errorf("%s: block %s of the room names the document %q, which is no document of the room", what, id, block.document)
			continue
		}
		if block.changed != want || block.textVersion != want {
			t.Errorf("%s: block %s of %s records version %d and its newest text at version %d, want its document's version %d",
				what, id, refKeys[block.document], block.changed, block.textVersion, want)
		}
		if wantIn := []string{"artifact:" + refKeys[block.document]}; !slices.Equal(block.inDocument, wantIn) {
			t.Errorf("%s: block %s states in_document %v, want %v, the document its issue_room_blocks row names", what, id, block.inDocument, wantIn)
		}
	}
	for document, want := range holds {
		if held[document] != want {
			t.Errorf("%s: the room holds %d blocks of %s, want %d", what, held[document], refKeys[document], want)
		}
	}
	var converted int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from document_blocks where artifact_id = any($1::uuid[])`,
		[]string{room.spec, room.verification, room.plan}).Scan(&converted); err != nil {
		t.Fatalf("count the blocks keyed by a converted document: %v", err)
	}
	if converted != 0 {
		t.Errorf("%s: %d document_blocks rows are keyed by a converted document, want none: the room's blocks are keyed by its tree", what, converted)
	}
}

// requireNoMarkOfTheRoom fails where any artifact of room still owes its block index, by either
// mark the reconciliation lists a document by.
func requireNoMarkOfTheRoom(t *testing.T, database *store.Store, room issueRoom, what string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select a.slug from document_block_versions b join artifacts a on a.id = b.artifact_id
		where b.artifact_id = any($1::uuid[]) and (b.version is null or b.store_owed_at is not null)
		order by a.slug`, []string{room.tree, room.spec, room.verification, room.plan})
	if err != nil {
		t.Fatalf("read the room's marks: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatalf("read a mark of the room: %v", err)
		}
		t.Errorf("%s: %s still owes its block index, want every mark of the room cleared", what, slug)
	}
}

// requireRoomCensus holds the block census to zero differences over the room: the block kinds and
// room_block, the projection of each room block's in_document, each checking rows.
func requireRoomCensus(t *testing.T, database *store.Store, what string) {
	t.Helper()
	census := takeStoreCensus(t, database)
	checked := map[string]graphcensus.KindReport{}
	for _, kind := range census.Kinds {
		switch kind.Kind {
		case refs.BlockNodeKind, "block_ref", "block_rank", "block_text", "room_block":
			checked[kind.Kind] = kind
			for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
				t.Errorf("%s: the census reads a difference of kind %s at %s: %s", what, kind.Kind, difference.At, difference.Cause)
			}
			if kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
				t.Errorf("%s: the census reads %d and %d differences of kind %s, want none", what, kind.RowDifferences, kind.NodeDifferences, kind.Kind)
			}
		}
	}
	for _, name := range []string{refs.BlockNodeKind, "block_rank", "block_text", "room_block"} {
		if kind, held := checked[name]; !held || kind.Rows == 0 || kind.Nodes == 0 {
			t.Errorf("%s: the census checked %d rows and %d nodes of kind %s (held %t), want the room's blocks", what, kind.Rows, kind.Nodes, name, held)
		}
	}
}

// A block of an issue room knows its document, and a reconciliation of the room reads the whole
// room (T12a of the issue-tree plan, the migrations paragraph of its spec version 106).
// The room's settlement writes one version of each document whose fragment's rendering changed,
// with that document's own artifact.version event, and indexes the room once under its tree
// artifact: each block has its issue_room_blocks row naming its fragment's document, an
// in_document to that document's node, and records that document's version (the spec's 3, beside
// the others' 2), every mark of the room is cleared, and the block census reads zero. A mark on
// one converted document, and then a mark on the tree artifact itself, each reconcile the room
// whole, under the tree artifact: a block of another fragment whose row was lost comes back, no
// fragment's block goes, no row is keyed by a converted document, and every mark of the room is
// cleared.
//
// Each pass over the room records each document it covers stated at the version it covers, by
// this build's rule (refs.BlockIndexMarkingRule), in the statement of its fence. A retired
// document of the room is a document the pass covers no version of: once the backfill has marked
// it as a document no pass of this rule stated, the room's reconciliation clears its mark with the
// marks of the room and states it at the version it leaves it at, so neither the backfill nor a
// tick marks it again.
func TestARoomBlockKnowsItsDocumentAndAReconciliationReadsTheRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	// The spec holds a second version from before the room's first settlement, so its blocks'
	// version tells the spec's history from the others' and from the tree's.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, '', '[{"kind":"user","id":"alice"}]')`, room.spec); err != nil {
		t.Fatalf("give the spec a second version: %v", err)
	}

	writers := []struct {
		way, fragment, text string
	}{
		{room.spec, room.spec, "Spec words."},
		{room.verification, room.verification, "Check words."},
		{room.tree, fragmentName, "Tree words."},
	}
	for _, writer := range writers {
		peer := connectPeer(t, server.URL, writer.way, "alice")
		if _, err := peer.Send(writeParagraph(peer.Doc, writer.fragment, writer.text)); err != nil {
			t.Fatalf("write into the fragment %s: %v", writer.fragment, err)
		}
		if err := peer.WaitForText(fragmentMarkdown(writer.fragment), writer.text+"\n", roomWait(t)); err != nil {
			t.Fatalf("a peer's copy of the issue room: %v", err)
		}
	}
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle the issue's room: %v", err)
	}

	// The settlement: one version of each document whose fragment changed, each its own
	// fragment's text, recorded by its own event.
	versions := map[string]int{room.spec: 3, room.verification: 2, room.plan: 2, room.tree: 2}
	texts := map[string]string{room.spec: "Spec words.\n", room.verification: "Check words.\n", room.plan: planSeed, room.tree: "Tree words.\n"}
	for document, number := range versions {
		var markdown string
		var events, indexed int
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select v.markdown from artifact_versions v where v.artifact_id = $1 and v.number = $2), '<none>'),
			       (select count(*) from events e where e.type = 'artifact.version'
			          and e.payload->>'artifact_id' = $1::text and e.payload->'version'->>'number' = $2::text),
			       (select count(*) from document_indexed_versions i where i.artifact_id = $1 and i.version = $2)`,
			document, number).Scan(&markdown, &events, &indexed); err != nil {
			t.Fatalf("read version %d of %s: %v", number, document, err)
		}
		if markdown != texts[document] || events != 1 || indexed != 1 {
			t.Errorf("version %d of %s holds %q with %d artifact.version events and %d indexed rows, want %q, one event and one row",
				number, document, markdown, events, indexed, texts[document])
		}
	}
	holds := map[string]int{room.spec: 1, room.verification: 1, room.plan: 1, room.tree: 1}
	requireRoomBlocks(t, database, room, "the settlement", versions, holds)
	requireNoMarkOfTheRoom(t, database, room, "the settlement")
	requireRoomCensus(t, database, "the settlement")

	// lose drops the row of the block of document's fragment from the room's index, as an index
	// that never read that fragment would leave it, and marks owed alone; the reconciliation that
	// follows has to read the whole room to bring the row back.
	lose := func(document, owed string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			with lost as (
				delete from issue_room_blocks where tree_artifact_id = $1 and document_id = $2 returning block_id
			)
			delete from document_blocks b using lost where b.artifact_id = $1 and b.block_id = lost.block_id`,
			room.tree, document); err != nil {
			t.Fatalf("drop the block of %s from the room's index: %v", document, err)
		}
		if _, err := database.Pool.Exec(ctx, `update document_block_versions set version = null where artifact_id = $1`, owed); err != nil {
			t.Fatalf("mark %s owed its block index: %v", owed, err)
		}
	}
	// reconcile runs the block-index reconciliation's ticks until nothing is owed: a version write
	// owes its document a re-index with the whole grammar (refs_grammar_owed), which a tick's
	// texts arm turns into a mark the next tick's documents batch reconciles, here the room's.
	reconcile := func(what string) {
		t.Helper()
		indexed := 0
		for range 5 {
			tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, nil)
			if err != nil {
				t.Fatalf("%s: reconcile the block index: %v", what, err)
			}
			if tick.LiveAhead != 0 || tick.Unreadable != 0 {
				t.Fatalf("%s: the tick found %d live ahead and %d unreadable, want the room indexed", what, tick.LiveAhead, tick.Unreadable)
			}
			indexed += tick.Documents
			if tick.OwedDocuments == 0 && tick.OwedTexts == 0 {
				break
			}
		}
		if indexed == 0 {
			t.Errorf("%s: the reconciliation indexed nothing, want the room indexed", what)
		}
	}

	lose(room.plan, room.spec)
	reconcile("a mark on the spec")
	requireRoomBlocks(t, database, room, "a mark on the spec", versions, holds)
	requireNoMarkOfTheRoom(t, database, room, "a mark on the spec")
	requireRoomCensus(t, database, "a mark on the spec")

	lose(room.verification, room.tree)
	reconcile("a mark on the tree artifact")
	requireRoomBlocks(t, database, room, "a mark on the tree artifact", versions, holds)
	requireNoMarkOfTheRoom(t, database, room, "a mark on the tree artifact")
	requireRoomCensus(t, database, "a mark on the tree artifact")
	for document, version := range versions {
		requireStatedByThisRule(t, database, document, version, "the room's passes")
	}

	func() {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the retirement: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := RetireDocument(ctx, tx, room.verification, model.Actor{Kind: "user", ID: "alice"}, "The checks moved to the plan."); err != nil {
			t.Fatalf("retire the Verification document: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the retirement: %v", err)
		}
	}()
	// The row as the backfill leaves a document no pass of this rule has stated: marked, and
	// stated by no pass.
	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set store_owed_at = now(), stated_version = null, stated_rule = null where artifact_id = $1`,
		room.verification); err != nil {
		t.Fatalf("mark the retired Verification document as the backfill marks it: %v", err)
	}
	reconcile("a mark on the retired document")
	requireNoMarkOfTheRoom(t, database, room, "a mark on the retired document")
	requireStatedByThisRule(t, database, room.verification, versions[room.verification], "a mark on the retired document")
}

// joinIssueRoom writes, by SQL, a document of room's issue under slug, with an empty first version
// as every document is created with, and its document_rooms row naming room's tree, and announces
// nothing: a join whose notice no process heard (AnnounceDocumentRooms).
func joinIssueRoom(t *testing.T, database *store.Store, room issueRoom, slug string) string {
	t.Helper()
	ctx := context.Background()
	var document string
	if err := database.Pool.QueryRow(ctx, `
		with document as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, 'DOC', $2, $2 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
			returning id
		), first as (
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select id, 1, '', '[{"kind":"user","id":"alice"}]' from document
		), joined as (
			insert into document_rooms (artifact_id, room) select id, $3::uuid from document
		)
		select id::text from document`, room.issue, slug, room.tree).Scan(&document); err != nil {
		t.Fatalf("join %s to the issue room: %v", slug, err)
	}
	return document
}

// requireStatedByThisRule fails unless the document_block_versions row of artifactID records a
// pass of this build's rule at version: stated_version version and stated_rule
// refs.BlockIndexMarkingRule.
func requireStatedByThisRule(t *testing.T, database *store.Store, artifactID string, version int, what string) {
	t.Helper()
	var statedVersion, statedRule string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(stated_version::text, 'null'), coalesce(stated_rule::text, 'null')
		from document_block_versions where artifact_id = $1::uuid
	`, artifactID).Scan(&statedVersion, &statedRule); err != nil {
		t.Fatalf("%s: read the pass that last stated %s: %v", what, artifactID, err)
	}
	if statedVersion != strconv.Itoa(version) || statedRule != strconv.Itoa(refs.BlockIndexMarkingRule) {
		t.Errorf("%s: document %s reads stated_version %s and stated_rule %s, want version %d by rule %d",
			what, artifactID, statedVersion, statedRule, version, refs.BlockIndexMarkingRule)
	}
}

// An update built on a document's own room is refused once the document moved into its issue's
// room (ErrRoomMoved), whoever appends it through AppendUpdateTx - a settlement's repair of the
// old room, a live write, a seed: nothing of it lands in the issue room's history, nor in the old
// room's. An append addressed to the issue room by its tree artifact's own id is stored there.
func TestAnUpdateBuiltOnAMovedDocumentsOwnRoomIsRefused(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	room := createIssueRoom(t, database, 1)
	persist := NewPgVersioned(database)
	appendTo := func(artifactID, text string) error {
		t.Helper()
		doc := crdt.New()
		doc.Transact(writeParagraph(doc, fragmentName, text))
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the append: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := persist.AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil), true); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	tree, spec := roomUpdateCount(t, database, room.tree), roomUpdateCount(t, database, room.spec)

	if err := appendTo(room.spec, "Old room words."); !errors.Is(err, ErrRoomMoved) {
		t.Fatalf("an append of an update built on the spec's own room answered %v, want ErrRoomMoved", err)
	}
	if after := roomUpdateCount(t, database, room.tree); after != tree {
		t.Errorf("the issue room holds %d stored updates after the refused append, want the %d it held", after, tree)
	}
	if after := roomUpdateCount(t, database, room.spec); after != spec {
		t.Errorf("the spec's own room holds %d stored updates after the refused append, want the %d it held", after, spec)
	}
	if err := appendTo(room.tree, "Tree words."); err != nil {
		t.Fatalf("append to the issue room by its tree artifact's id: %v", err)
	}
	if after := roomUpdateCount(t, database, room.tree); after != tree+1 {
		t.Errorf("the issue room holds %d stored updates after an append addressed to it, want %d", after, tree+1)
	}
}

// A settlement of an issue room reads the room's documents again under its owner row, and finds
// the layout its room instance loaded stale where a document joined and no notice of the join
// reached this process (AnnounceDocumentRooms): it writes nothing, closes the room and leaves the
// settlement owed, and the next settlement, of the room loaded over the new layout, versions the
// joined document's fragment and indexes its block as that document's, at its own version.
func TestARoomSettlementOverAStaleLayoutClosesTheRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	peer := connectPeer(t, server.URL, room.tree, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	joined := joinIssueRoom(t, database, room, "notes")
	// The room instance holds the joined document's fragment, as it holds every fragment of its
	// Yjs document; the layout it loaded does not name it. The edit of the spec's fragment beside it
	// is one the loaded layout reads, which leaves the room owing its settlement.
	if _, err := peer.Send(writeParagraph(peer.Doc, joined, "Joined words.")); err != nil {
		t.Fatalf("write into the joined document's fragment: %v", err)
	}
	if _, err := peer.Send(writeParagraph(peer.Doc, room.spec, "Spec words.")); err != nil {
		t.Fatalf("write into the spec's fragment: %v", err)
	}
	waitForStoredUpdates(t, database, room.tree, 2)
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}

	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err == nil {
		t.Fatalf("the settlement over the stale layout left nothing owed, want it to write nothing and leave the room owed")
	}
	select {
	case <-peer.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the settlement over the stale layout did not close the issue room")
	}
	settled := false
	for attempt := 0; attempt < 5 && !settled; attempt++ {
		settled = service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))) == nil
	}
	if !settled {
		t.Fatalf("the issue room still owes its settlement after the room loaded over the new layout")
	}
	held := 0
	for id, block := range readRoomBlocks(t, database, room.tree) {
		if block.document != joined {
			continue
		}
		held++
		if block.changed != 2 || block.textVersion != 2 {
			t.Errorf("block %s of the joined document records version %d and its text at %d, want the joined document's version 2", id, block.changed, block.textVersion)
		}
	}
	if held != 1 {
		t.Errorf("the room holds %d blocks of the joined document, want its one paragraph", held)
	}
}

// seedJoinedDocument opens a transaction joined to service and writes in it, as an upload to a
// converted issue writes them, a new document of room's issue under slug with its document_rooms
// row naming room's tree, the issue's row taken first as every writer of that row takes it, and the
// document's first text, markdown, seeded into its fragment of the room (SeedText). It answers the
// document and the transaction's ledger, not committed; the test's cleanup discards both.
func seedJoinedDocument(t *testing.T, service *Service, room issueRoom, slug, markdown string) (string, *Ledger) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the join's transaction: %v", err)
	}
	testcleanup.Add(t, func() { _ = tx.Rollback(ctx) })
	joined, ledger := service.Join(ctx, tx)
	testcleanup.Add(t, ledger.Discard)
	if _, err := tx.Exec(ctx, `select 1 from issues where key = $1 for no key update`, room.issue); err != nil {
		t.Fatalf("take the issue's row: %v", err)
	}
	var document string
	if err := tx.QueryRow(ctx, `
		with document as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, 'DOC', $2, $2 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
			returning id
		), joined as (
			insert into document_rooms (artifact_id, room) select id, $3::uuid from document
		)
		select id::text from document`, room.issue, slug, room.tree).Scan(&document); err != nil {
		t.Fatalf("join %s to the issue room: %v", slug, err)
	}
	if _, err := service.SeedText(joined, document, markdown, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("seed %s's fragment of the issue room: %v", slug, err)
	}
	return document, ledger
}

// A transaction that seeds a new document's first text into its issue's room - an upload of a new
// document to a converted issue, which writes the document's document_rooms row first - changes the
// room's layout, and its commit closes this process's instance of the room, loaded over the
// documents before it, before the writer answers: the writer's next write loads the room over the
// new layout rather than meeting that instance's close, which a settlement that found the layout
// stale makes (errRoomLayoutMoved) and which refuses every write it meets (errRoomReloading). No
// notice of the join reaches this process here (the join announces nothing), so the close is the
// commit's own.
func TestADocumentSeededIntoAResidentIssueRoomClosesTheRoomAtItsCommit(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	peer := connectPeer(t, server.URL, room.tree, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	joined, ledger := seedJoinedDocument(t, service, room, "notes", "Joined words.\n")
	if err := ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit the join: %v", err)
	}
	if live := service.srv.GetDoc(room.tree); live != nil && !slices.Contains(service.layoutOf(live), joined) {
		t.Fatalf("the commit that seeded %s into the issue room left resident the room loaded over %v", joined, service.layoutOf(live))
	}
	select {
	case <-peer.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the browser on the room loaded over the documents before the join stayed connected")
	}
	if _, err := joinedReplaceText(service, joined, "Joined words, written again.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("the next write to the joined document: %v", err)
	}
}

// A settlement that read the issue room's documents once a join's transaction committed finds
// the layout of the instance it settles stale and reserves that instance's close
// (reserveRoomCloseLocked), which refuses every write until it returns (errRoomReloading). The
// join's commit, which closes that instance itself, waits the reservation out before the writer
// answers, so the writer's next write is not refused.
func TestAJoinsCommitWaitsOutACloseReservedOnItsRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	// No settlement timer of the room fires during the test: the reservation is the test's own.
	service.settle = time.Hour
	room := createIssueRoom(t, database, 1)
	peer := connectPeer(t, server.URL, room.tree, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	joined, ledger := seedJoinedDocument(t, service, room, "notes", "Joined words.\n")
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit the join's transaction: %v", err)
	}

	reserved, release := make(chan struct{}), make(chan struct{})
	var reservedOnce sync.Once
	service.afterRoomCloseReserved = func(string) {
		reservedOnce.Do(func() { close(reserved) })
		<-release
	}
	state := service.lockState(room.tree)
	reservation, ok := service.reserveRoomCloseLocked(room.tree, state, service.srv.GetDoc(room.tree))
	service.unlockState(room.tree, state)
	if !ok {
		t.Fatal("reserve the close of the issue room's instance loaded over the documents before the join")
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_, _ = service.closeReservedRoom(ctx, reservation)
	}()
	select {
	case <-reserved:
	case <-time.After(roomWait(t)):
		t.Fatal("the reserved close never started")
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ledger.publish()
		ledger.closeJoinedRooms(ctx)
	}()
	select {
	case <-finished:
		close(release)
		t.Fatal("the join's commit answered while a close reserved on its room still refused every write")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(roomWait(t)):
		t.Fatal("the join's commit did not answer once the reserved close returned")
	}
	<-closed
	if _, err := joinedReplaceText(service, joined, "Joined words, written again.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("the next write to the joined document: %v", err)
	}
}

// A join whose notice no process heard - announced while the hub had no connection - is found by
// the moved-rooms lane's pass after its next LISTEN (recheckMovedRooms): a resident issue room whose
// stored documents differ from the layout its instance loaded is closed, so its next load reads
// them, and a resident issue room whose documents stand as it loaded them stays open.
func TestARecheckClosesAResidentIssueRoomWhoseDocumentsChanged(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	moved, kept := createIssueRoom(t, database, 1), createIssueRoom(t, database, 2)
	peers := map[string]*docstest.Peer{}
	for _, room := range []issueRoom{moved, kept} {
		peers[room.tree] = connectPeer(t, server.URL, room.tree, "alice")
		if err := peers[room.tree].WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
			t.Fatal(err)
		}
	}
	joinIssueRoom(t, database, moved, "notes")

	service.recheckMovedRooms(ctx)

	select {
	case <-peers[moved.tree].Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the recheck left open the issue room a document joined")
	}
	select {
	case <-peers[kept.tree].Ended:
		t.Fatalf("the recheck closed the issue room whose documents stand as it loaded them")
	case <-time.After(200 * time.Millisecond):
	}
	if service.srv.GetDoc(kept.tree) == nil {
		t.Errorf("the issue room whose documents stand as it loaded them is no longer resident")
	}
}

// An issue's tree that no document has joined yet is the only fragment of its room, and a mark on
// it reconciles it as its own document (indexOwedDocument): the tick indexes it and clears the
// mark, where a room with no joined fragment to part leaves the tree owed on every tick, and
// records it stated at the version it indexed by this build's rule (refs.BlockIndexMarkingRule).
// A tree that holds no version yet, marked as the backfill marks a document no pass of this rule
// stated, has nothing to index: the tick clears its marks and states it at version 0, so it is no
// longer owed and neither the backfill nor a tick marks it again.
func TestAnIssuesTreeNoDocumentJoinedReconcilesAsItsOwnDocument(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	var tree, bare string
	if err := database.Pool.QueryRow(ctx, `
		with project as (
			insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice')
			on conflict (key) do nothing
		), issue as (
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('DOC-1', 'DOC', 1, 'Issue room', '{"kind":"user","id":"alice"}', 'U'),
			       ('DOC-2', 'DOC', 2, 'Issue room with no version', '{"kind":"user","id":"alice"}', 'V')
		), tree as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('DOC-1', 'DOC', 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}'),
			       ('DOC-2', 'DOC', 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
			returning id, issue_key
		), first as (
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select id, 1, '', '[{"kind":"user","id":"alice"}]' from tree where issue_key = 'DOC-1'
		), held as (
			insert into issue_trees (issue_key, artifact_id) select issue_key, id from tree
		), marked as (
			insert into document_block_versions (artifact_id, store_owed_at) select id, now() from tree where issue_key = 'DOC-2'
		)
		select (select id::text from tree where issue_key = 'DOC-1'), (select id::text from tree where issue_key = 'DOC-2')`).Scan(&tree, &bare); err != nil {
		t.Fatalf("create the issues' trees: %v", err)
	}
	owed := func(artifactID string) bool {
		t.Helper()
		var owes bool
		if err := database.Pool.QueryRow(ctx, `
			select coalesce((select version is null or store_owed_at is not null from document_block_versions where artifact_id = $1), false)
		`, artifactID).Scan(&owes); err != nil {
			t.Fatalf("read the mark of %s: %v", artifactID, err)
		}
		return owes
	}
	if !owed(tree) || !owed(bare) {
		t.Fatalf("the tree's first version left it owing %t and the marked tree with no version %t, want both owed: the case proves nothing",
			owed(tree), owed(bare))
	}
	indexed := 0
	for range 3 {
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, nil)
		if err != nil {
			t.Fatalf("reconcile the block index: %v", err)
		}
		indexed += tick.Documents
	}
	if owed(tree) || indexed == 0 {
		t.Errorf("after three ticks the tree owes its block index %t, with %d documents indexed, want it indexed and its mark cleared", owed(tree), indexed)
	}
	requireStatedByThisRule(t, database, tree, 1, "after the tree's ticks")
	if owed(bare) {
		t.Errorf("after three ticks the tree that holds no version still owes its block index, want its marks cleared: it has nothing to index")
	}
	requireStatedByThisRule(t, database, bare, 0, "after the ticks over the tree that holds no version")
}

// A document moved into its issue's room leaves the compaction sweep's list: the sweep deletes its
// entry and folds none of the rows its own room left, which stay as they stood, due or not. The
// issue room's own appends name the tree artifact in the list, which is the room's to fold.
func TestTheCompactionSweepDropsTheEntryOfAMovedDocument(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	testcleanup.Add(t, func() { _ = service.Shutdown(context.Background()) })
	due := storeAgedRows(t, database, 3, time.Hour)
	young := storeAgedRows(t, database, 3, 0)
	dueRows, youngRows := storedRows(t, database, due), storedRows(t, database, young)
	if _, err := database.Pool.Exec(ctx, `
		with tree as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('DOC-1', 'DOC', 'tree', 'tree', 'tree', false, '{"kind":"user","id":"alice"}')
			returning id
		)
		insert into document_rooms (artifact_id, room) select moved.id, tree.id from tree, unnest($1::uuid[]) as moved (id)
	`, []string{due, young}); err != nil {
		t.Fatalf("move the documents into their issue's room: %v", err)
	}
	requireOwed(t, database, "the documents an append gave a second row", due, young)

	service.compactionSweepTick(ctx, excerptLeases(t, database))

	requireOwed(t, database, "no document: a moved document's own room has nothing the sweep folds")
	requireRowsAsStored(t, database, due, dueRows)
	requireRowsAsStored(t, database, young, youngRows)
}

// A room settlement's block events name the version of the block's own document: an ask block of
// the spec's fragment that settlement cannot read, an option of it left without a label, is
// marked invalid by a block.invalid at the spec's version the settlement writes (its 3), never at
// a number of the tree artifact's history (its 1), whose own fragment the settlement left alone.
func TestARoomSettlementNamesABlockEventWithItsOwnDocumentsVersion(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, '', '[{"kind":"user","id":"alice"}]')`, room.spec); err != nil {
		t.Fatalf("give the spec a second version: %v", err)
	}
	ask, err := pmdoc.Parse(":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	if err != nil {
		t.Fatalf("parse the ask block: %v", err)
	}
	ask.Children[0].Children[1].Children[0].Children[0].Children = nil
	spec := connectPeer(t, server.URL, room.spec, "alice")
	if err := spec.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	fragment := spec.Doc.GetXmlFragment(room.spec)
	var written error
	if _, err := spec.Send(func(txn *crdt.Transaction) { written = pmdoc.Update(txn, fragment, ask) }); err != nil || written != nil {
		t.Fatalf("write the ask block into the spec's fragment: %v, %v", err, written)
	}
	waitForStoredUpdates(t, database, room.tree, 1)
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle the issue's room: %v", err)
	}
	var version string
	if err := database.Pool.QueryRow(ctx, `
		select payload->>'version' from events where type = 'block.invalid' and payload->>'block_id' = 'ask-block'
		order by id desc limit 1`).Scan(&version); err != nil {
		t.Fatalf("read the block.invalid of the ask block: %v", err)
	}
	if version != "3" {
		t.Errorf("the block.invalid of the spec's ask block names version %s, want the spec's version 3 the settlement wrote", version)
	}
}

// A room settlement credits each version with the authors of the updates that changed its own
// document's fragment: alice's edit of the spec, made while she alone was connected, credits the
// spec's version to her alone, though bob, who connected after it and edited the Verification
// document, is credited on that document's version; and the plan's fragment, which only the
// conversion's seed wrote, credits nobody.
func TestARoomSettlementCreditsEachVersionWithItsOwnFragmentsAuthors(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	alice := connectPeer(t, server.URL, room.spec, "alice")
	if _, err := alice.Send(writeParagraph(alice.Doc, room.spec, "Spec words.")); err != nil {
		t.Fatalf("write into the spec's fragment: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}
	bob := connectPeer(t, server.URL, room.verification, "bob")
	if err := bob.WaitForText(fragmentMarkdown(room.spec), "Spec words.\n", roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Send(writeParagraph(bob.Doc, room.verification, "Check words.")); err != nil {
		t.Fatalf("write into the Verification document's fragment: %v", err)
	}
	if err := alice.WaitForText(fragmentMarkdown(room.verification), "Check words.\n", roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle the issue's room: %v", err)
	}
	ids := func(document string) []string {
		t.Helper()
		var held []string
		for _, author := range versionAuthors(t, database, document, 2) {
			held = append(held, author.ID)
		}
		slices.Sort(held)
		return held
	}
	if got := ids(room.spec); !slices.Equal(got, []string{"alice"}) {
		t.Errorf("the spec's version 2 lists %v, want alice alone, the only author of an update of its fragment", got)
	}
	if got := ids(room.verification); !slices.Contains(got, "bob") {
		t.Errorf("the Verification document's version 2 lists %v, want bob among them", got)
	}
	if got := ids(room.plan); len(got) != 0 {
		t.Errorf("the plan's version 2 lists %v, want nobody: no update of anyone's changed its fragment", got)
	}
}

// A room settlement's retraction names the version of the block's own document: an ask block of
// the spec's fragment opens an ask (a block of an issue room is the tree's, as its ask row says),
// and once a browser takes the block out of the fragment the settlement retracts the ask "removed
// from the document in version" the spec's version it writes (its 4), never at a number of the
// tree artifact's history (its 1), whose own fragment no settlement changed.
func TestARoomSettlementRetractsAnAskAtItsOwnDocumentsVersion(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 2, '', '[{"kind":"user","id":"alice"}]')`, room.spec); err != nil {
		t.Fatalf("give the spec a second version: %v", err)
	}
	ask, err := pmdoc.Parse(":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	if err != nil {
		t.Fatalf("parse the ask block: %v", err)
	}
	spec := connectPeer(t, server.URL, room.spec, "alice")
	if err := spec.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	fragment := spec.Doc.GetXmlFragment(room.spec)
	settle := func(what string, change *pmdoc.Node) {
		t.Helper()
		before := roomUpdateCount(t, database, room.tree)
		var written error
		if _, err := spec.Send(func(txn *crdt.Transaction) { written = pmdoc.Update(txn, fragment, change) }); err != nil || written != nil {
			t.Fatalf("%s: %v, %v", what, err, written)
		}
		waitForStoredUpdates(t, database, room.tree, before)
		if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
			t.Fatalf("%s: wait for the room's appends: %v", what, err)
		}
		if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
			t.Fatalf("%s: settle the issue's room: %v", what, err)
		}
	}
	settle("write the ask block into the spec's fragment", ask)
	var blockArtifact, state string
	if err := database.Pool.QueryRow(ctx, `select block_artifact_id::text, state from asks where block_id = 'ask-block'`).Scan(&blockArtifact, &state); err != nil {
		t.Fatalf("read the ask the block opened: %v", err)
	}
	if blockArtifact != room.tree || state != "open" {
		t.Fatalf("the ask block's ask stands under %s in state %s, want an open ask under the tree artifact %s", blockArtifact, state, room.tree)
	}
	gone, err := pmdoc.Parse("The ask went.\n")
	if err != nil {
		t.Fatalf("parse the paragraph: %v", err)
	}
	settle("take the ask block out of the spec's fragment", gone)
	var reason string
	if err := database.Pool.QueryRow(ctx, `select resolution->>'reason' from asks where block_id = 'ask-block'`).Scan(&reason); err != nil {
		t.Fatalf("read the ask's resolution: %v", err)
	}
	if want := SettlementRetractionReason + " 4"; reason != want {
		t.Errorf("the retraction of the spec's ask reads %q, want %q: the spec's version 4 the settlement wrote", reason, want)
	}
}

// An issue room's update writes its authors under each document whose fragment it changed
// (pendingAuthorsIn), in the one statement every append writes them with (pendingAuthorsStatement):
// a run that names documents for some rows and none for others writes those under the room, and a
// run that names none at all, as every update of a room that is its own document, writes every
// row under the room.
func TestAnIssueRoomUpdatesPendingAuthorsAreWrittenUnderItsDocuments(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	room := createIssueRoom(t, database, 1)
	alice, bob, carol := model.Actor{Kind: "user", ID: "alice"}, model.Actor{Kind: "user", ID: "bob"}, model.Actor{Kind: "user", ID: "carol"}
	write := func(authors []pendingAuthor) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if err := upsertPendingAuthors(ctx, tx, room.tree, authors); err != nil {
			t.Fatalf("write the pending authors: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the pending authors: %v", err)
		}
	}
	write(append(pendingAuthorsIn(map[string]model.Actor{actorKey(alice): alice}, 2, []string{room.spec, room.verification}),
		pendingAuthorsAt(map[string]model.Actor{actorKey(bob): bob}, 3)...))
	write(pendingAuthorsIn(map[string]model.Actor{actorKey(carol): carol}, 4, nil))

	rows, err := database.Pool.Query(ctx, `
		select artifact_id::text || ' ' || actor_id || ' ' || written_through from doc_pending_authors order by written_through, artifact_id, actor_id`)
	if err != nil {
		t.Fatalf("read the pending authors: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the pending authors: %v", err)
	}
	want := []string{room.spec + " alice 2", room.verification + " alice 2"}
	slices.Sort(want)
	want = append(want, room.tree+" bob 3", room.tree+" carol 4")
	if !slices.Equal(got, want) {
		t.Errorf("the pending authors stand as\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	owed, err := readRoomPendingAuthors(ctx, database.Pool, []string{room.tree, room.verification, room.plan, room.spec})
	if err != nil {
		t.Fatalf("read the room's pending authors: %v", err)
	}
	if len(owed[room.spec]) != 1 || len(owed[room.verification]) != 1 || len(owed[room.tree]) != 2 || len(owed[room.plan]) != 0 {
		t.Errorf("the room's pending authors by document are %v, want alice under the spec and the Verification document and bob and carol under the tree", owed)
	}
}

// A version of a document of an issue room is read from the room (captureLiveTextAndAuthors), so
// the record its commit holds locked is the room's, and the commit lets it go as the room's: a
// named version of the plan, committed or refused at its commit, leaves the record of the room
// the server holds registered under the room and not released, and the next lookup of the room
// finds it. Let go under the document's own id, which names no room the server holds, the record
// was forgotten while the room still held it and stayed in the service's map, and every later
// lookup of the room - the next version's read, the room's persistence - spun on it forever.
func TestAVersionOfADocumentOfAnIssueRoomLeavesTheRoomsRecordToTheRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := database.Pool.Exec(ctx, `
		create function test_refuse_version() returns trigger language plpgsql as $$
		begin
		  if current_setting('test.refuse_version', true) = 'on' then
		    raise exception 'the test refuses this commit';
		  end if;
		  return null;
		end $$;
		create constraint trigger zz_refuse_version after insert on artifact_versions
		deferrable initially deferred for each row execute function test_refuse_version();
	`); err != nil {
		t.Fatalf("install the trigger that refuses a version's commit: %v", err)
	}
	for _, refused := range []bool{false, true} {
		what := "a committed named version of the plan"
		if refused {
			what = "a named version of the plan refused at its commit"
		}
		committed := func() error {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("%s: begin: %v", what, err)
			}
			defer tx.Rollback(ctx)
			if refused {
				if _, err := tx.Exec(ctx, `select set_config('test.refuse_version', 'on', true)`); err != nil {
					t.Fatalf("%s: refuse the commit: %v", what, err)
				}
			}
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			if _, err := service.NamedVersion(joined, room.plan, "named", alice); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			return ledger.Commit(ctx)
		}()
		if refused != (committed != nil) {
			t.Fatalf("%s: the commit answered %v", what, committed)
		}
		if service.srv.GetDoc(room.tree) == nil {
			t.Fatalf("%s: the version's read left no room of the issue on the server: this test proves nothing", what)
		}
		value, held := service.rooms.Load(room.tree)
		if !held || value.(*roomState).released {
			t.Fatalf("%s: the record of the issue's room, which the server still holds, is held %t (released: %t), want it held and not released",
				what, held, held && value.(*roomState).released)
		}
		looked := make(chan struct{})
		go func() {
			defer close(looked)
			if state := service.lockExistingState(room.tree); state != nil {
				service.unlockState(room.tree, state)
			}
		}()
		select {
		case <-looked:
		case <-time.After(roomWait(t)):
			t.Fatalf("%s: a lookup of the issue's room did not return within %v", what, roomWait(t))
		}
	}
}

// A version of a document of an issue room whose transaction rendered no tree of it - an upload
// whose replacement changed nothing, or one whose written tree did not render as the version's
// markdown - is indexed only where the document's own fragment of the room reads as that version,
// as each other fragment of the room is (indexDocumentInRoom): the plan's fragment holds the
// conversion's seed, its version 2 says other words, so the room is left owed to the block-index
// reconciliation and no block of the plan is indexed at version 2.
func TestAVersionNoWriteRenderedIsIndexedOnlyWhereItsFragmentReadsAsIt(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	const written = "The version's own words.\n"
	func() {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		joined, ledger := service.Join(ctx, tx)
		defer ledger.Discard()
		if _, _, err := lockArtifactOwner(ctx, tx, room.plan); err != nil {
			t.Fatalf("take the plan's owner row: %v", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 2, $2, '[{"kind":"user","id":"alice"}]')`, room.plan, written); err != nil {
			t.Fatalf("write the plan's version 2: %v", err)
		}
		if _, _, err := service.IndexDocumentReferences(joined, room.plan, 2, written); err != nil {
			t.Fatalf("index the plan's version 2: %v", err)
		}
		if err := ledger.Commit(ctx); err != nil {
			t.Fatalf("commit the plan's version 2: %v", err)
		}
	}()
	var indexed *int
	var blocks int
	if err := database.Pool.QueryRow(ctx, `
		select (select version from document_block_versions where artifact_id = $1),
		       (select count(*) from issue_room_blocks where tree_artifact_id = $2 and document_id = $1)
	`, room.plan, room.tree).Scan(&indexed, &blocks); err != nil {
		t.Fatalf("read the plan's block index: %v", err)
	}
	if indexed != nil || blocks != 0 {
		t.Errorf("the plan's blocks stand indexed at %v with %d block(s) of the room, cut from a fragment holding %q, want the room left owed and no block of the plan indexed at version 2, whose markdown is %q",
			describeVersion(indexed), blocks, planSeed, written)
	}
}

// describeVersion reads a document_block_versions version: the number, or "none" for a document
// owed its block index.
func describeVersion(version *int) string {
	if version == nil {
		return "none"
	}
	return strconv.Itoa(*version)
}

// A converted document's retirement closes its issue's room on every process, since that room is
// where the document is edited (AnnounceDocumentRooms beside AnnounceDocumentRemoved), and the
// retired document's fragment takes no version from then on: the spec's socket, the one open
// on the room, is closed at the retirement's commit, and a paragraph a peer of the room then
// types into the retired Verification document's fragment is versioned under no document by the
// room's settlement, while the spec's fragment it changed beside it is.
func TestARetiredDocumentOfAnIssueRoomClosesTheRoomAndTakesNoVersion(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	runRoomSync(t, newRoomSync(service))
	room := createIssueRoom(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
	before := connectPeer(t, server.URL, room.spec, "alice")
	if err := before.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	func() {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := RetireDocument(ctx, tx, room.verification, alice, "The checks moved to the plan."); err != nil {
			t.Fatalf("retire the Verification document: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the retirement: %v", err)
		}
	}()
	select {
	case <-before.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the spec's socket on the issue's room is still open %v after the Verification document's retirement, want the room closed", roomWait(t))
	}

	peer := connectPeer(t, server.URL, room.spec, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	stored := roomUpdateCount(t, database, room.tree)
	retired, own := writeParagraph(peer.Doc, room.verification, "Typed after the retirement."), writeParagraph(peer.Doc, room.spec, "The spec's own words.")
	if _, err := peer.Send(func(txn *crdt.Transaction) {
		retired(txn)
		own(txn)
	}); err != nil {
		t.Fatalf("type into the room: %v", err)
	}
	waitForStoredUpdates(t, database, room.tree, stored)
	if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
		t.Fatalf("wait for the room's appends: %v", err)
	}
	if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("settle the issue's room: %v", err)
	}
	latest := func(document string) (int, string) {
		t.Helper()
		var number int
		var markdown string
		if err := database.Pool.QueryRow(ctx, `
			select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1`, document).Scan(&number, &markdown); err != nil {
			t.Fatalf("read the latest version of %s: %v", document, err)
		}
		return number, markdown
	}
	if number, markdown := latest(room.verification); number != 1 {
		t.Errorf("the retired Verification document stands at version %d holding %q, want its version 1 alone", number, markdown)
	}
	if number, markdown := latest(room.spec); number != 2 || markdown != "The spec's own words.\n" {
		t.Errorf("the spec stands at version %d holding %q, want version 2 holding the words typed into its fragment", number, markdown)
	}
}

// A retired document of an issue room leaves the room (roomLayoutSQL): the room's layout names no
// fragment of it, the room reloads without one, and a paragraph a peer of the room types into the
// retired Verification document's fragment is versioned and indexed under no document, while the
// spec's fragment typed beside it is versioned and indexed. The room's block index keeps what it
// recorded of the retired document's blocks before the retirement - their rows, their record in the
// room, their texts and their nodes - as they stood (refs.PlanBlocks).
func TestARetiredDocumentLeavesItsIssueRoomAndKeepsItsIndex(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	runRoomSync(t, newRoomSync(service))
	room := createIssueRoom(t, database, 1)
	alice := model.Actor{Kind: "user", ID: "alice"}
	// settle sends peer's write into the room and settles the room once the update is stored.
	settle := func(peer *docstest.Peer, write func(*crdt.Transaction)) {
		t.Helper()
		stored := roomUpdateCount(t, database, room.tree)
		if _, err := peer.Send(write); err != nil {
			t.Fatalf("type into the room: %v", err)
		}
		waitForStoredUpdates(t, database, room.tree, stored)
		if err := service.waitForDurableAppends(ctx, room.tree); err != nil {
			t.Fatalf("wait for the room's appends: %v", err)
		}
		if err := service.settleOwedNow(ctx, room.tree, time.Now().Add(roomWait(t))); err != nil {
			t.Fatalf("settle the issue's room: %v", err)
		}
	}
	// retiredIndex is what the room's block index holds of the Verification document's blocks:
	// each block's room record, row, texts and node state, one line a block.
	retiredIndex := func() []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			select r.block_id || ' row=' || coalesce(b.text_sha256 || '@' || b.changed_version, 'none')
			       || ' texts=' || coalesce((select string_agg(x.version || ':' || coalesce(x.text, '<none>'), ',' order by x.version)
			                                 from document_block_texts x where x.artifact_id = r.tree_artifact_id and x.block_id = r.block_id), '')
			       || ' node=' || coalesce((select n.state from graph_nodes n where n.kind = 'block' and n.id = tree.ref_key || '#' || r.block_id), 'none')
			from issue_room_blocks r
			join artifacts tree on tree.id = r.tree_artifact_id
			left join document_blocks b on b.artifact_id = r.tree_artifact_id and b.block_id = r.block_id
			where r.tree_artifact_id = $1 and r.document_id = $2
			order by r.block_id`, room.tree, room.verification)
		if err != nil {
			t.Fatalf("read the room's index of the Verification document: %v", err)
		}
		held, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the room's index of the Verification document: %v", err)
		}
		return held
	}
	before := connectPeer(t, server.URL, room.spec, "alice")
	if err := before.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	settle(before, writeParagraph(before.Doc, room.verification, "The check the retirement keeps."))
	kept := retiredIndex()
	if len(kept) != 1 || !strings.Contains(kept[0], "node=live") {
		t.Fatalf("before the retirement the room indexes %v of the Verification document, want its one block, live", kept)
	}
	func() {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := RetireDocument(ctx, tx, room.verification, alice, "The checks moved to the plan."); err != nil {
			t.Fatalf("retire the Verification document: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the retirement: %v", err)
		}
	}()
	select {
	case <-before.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("the spec's socket on the issue's room is still open %v after the Verification document's retirement, want the room closed", roomWait(t))
	}
	if layout, err := readRoomLayout(ctx, database.Pool, room.tree); err != nil || slices.Contains(layout, room.verification) {
		t.Errorf("after the retirement the room's layout is %v (%v), want no fragment of the retired Verification document %s", layout, err, room.verification)
	}

	peer := connectPeer(t, server.URL, room.spec, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatal(err)
	}
	if live := service.srv.GetDoc(room.tree); live == nil || slices.Contains(service.layoutOf(live), room.verification) {
		t.Errorf("the room reloaded after the retirement holds the fragments of %v, want none of the retired Verification document %s",
			service.layoutOf(live), room.verification)
	}
	retired, own := writeParagraph(peer.Doc, room.verification, "Typed after the retirement."), writeParagraph(peer.Doc, room.spec, "The spec's own words.")
	settle(peer, func(txn *crdt.Transaction) {
		retired(txn)
		own(txn)
	})
	latest := func(document string) (int, string) {
		t.Helper()
		var number int
		var markdown string
		if err := database.Pool.QueryRow(ctx, `
			select number, markdown from artifact_versions where artifact_id = $1 order by number desc limit 1`, document).Scan(&number, &markdown); err != nil {
			t.Fatalf("read the latest version of %s: %v", document, err)
		}
		return number, markdown
	}
	if number, markdown := latest(room.verification); number != 2 || markdown != "The check the retirement keeps.\n" {
		t.Errorf("the retired Verification document stands at version %d holding %q, want the version 2 it held at its retirement", number, markdown)
	}
	if number, markdown := latest(room.spec); number != 2 || markdown != "The spec's own words.\n" {
		t.Errorf("the spec stands at version %d holding %q, want version 2 holding the words typed into its fragment", number, markdown)
	}
	var typed, ownWords int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks where artifact_id = $1 and excerpt like 'Typed after the retirement%')
		     + (select count(*) from document_block_texts where artifact_id = $1 and text like '%Typed after the retirement%')
		     + (select count(*) from issue_room_blocks r join document_blocks b on b.artifact_id = r.tree_artifact_id and b.block_id = r.block_id
		        where r.tree_artifact_id = $1 and b.excerpt like 'Typed after the retirement%'),
		       (select count(*) from document_blocks where artifact_id = $1 and excerpt like 'The spec''s own words%')
	`, room.tree).Scan(&typed, &ownWords); err != nil {
		t.Fatalf("read the room's index: %v", err)
	}
	if typed != 0 || ownWords != 1 {
		t.Errorf("the room indexes %d row(s) of the paragraph typed into the retired fragment and %d block(s) of the spec's own words, want none and one", typed, ownWords)
	}
	if got := retiredIndex(); !slices.Equal(got, kept) {
		t.Errorf("after the retirement the room indexes the Verification document's blocks as\n%v\nwant them as the retirement left them:\n%v", got, kept)
	}
}
