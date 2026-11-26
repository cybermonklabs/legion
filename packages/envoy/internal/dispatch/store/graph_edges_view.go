package store

import (
	"fmt"
	"strings"
)

// GraphEdgesArms names one set of the arms graph_edges is stated with from
// the issue-tree spec's split of the issue relations on: each migration that states the view
// holds, after its header, the statement GraphEdgesView gives for its set, and nothing else
// (TestEveryGraphEdgesMigrationHoldsTheViewItsSourceGives). Migrations are embedded files that
// call no Go, so this function is the one source of their text, and the boot step that drops an
// arm states the view by calling it. A later set is the one before it with the change its own
// migration makes, so the sets are ordered by the migrations that state them.
type GraphEdgesArms int

const (
	// GraphEdgesIssueRelations is the set 20261117140000_issue_tree_relations states. The arms
	// over issues (child_of), issue_links (blocked_by) and artifacts (attached_to) answer only the
	// rows that name no node, which no writer of this release wrote, and the store answers the
	// rest: the tuple arm renders an issue's child_of, its needs to an issue under the kind
	// blocked_by, which a process of the release before reads that wait by, and its duplicate_of
	// and filed_from, which no row projects; and an arm over the artifacts that carry their node
	// renders each one's attached_to while its part_of to that issue stands. The child_of arm also
	// answers a marked row whose parent the release before moved in place (IssueParentMoved).
	GraphEdgesIssueRelations GraphEdgesArms = iota + 1
	// GraphEdgesComponents is the set 20261118023219_issue_tree_components states:
	// GraphEdgesIssueRelations with the three arms over a component's relations split the same
	// way. The arms over components (part_of), component_depends (depends_on) and
	// issue_component_members (affects) answer only the rows that name no node, the part_of arm
	// also a marked row whose parent no longer matches its node's part_of (ComponentParentMoved),
	// and the tuple arm renders a component's part_of and depends_on and an issue's affects while
	// the projection row that names each carries its node.
	GraphEdgesComponents
	// GraphEdgesArtifactRefs is the set 20261119100000_issue_tree_artifact_refs states:
	// GraphEdgesComponents with the arm over refs split the same way for the references a document
	// makes, which the version index and rebuild-refs now state as tuples. The arm over refs answers
	// a row whose source is no document, a document whose row names no node, or a far end past the
	// store's bound (ReferenceNotStated), and the tuple arm renders a document's mentions while the
	// refs row that names the edge stands, naming the document by its uuid.
	GraphEdgesArtifactRefs
	// GraphEdgesTupleArmFilter is the set 20261121090000_issue_tree_graph_edges_after_tuple_arm_filter
	// states: GraphEdgesArtifactRefs with the tuple arm's sub-select keeping only the kinds the arm
	// renders before its `distinct on` sorts the tuples, the test spelled over the tuple's own
	// columns as the `when` arms of the `projects` case (tupleArmFilter), and the outer
	// `edge.projects is not null` test gone, as 20261120122700_graph_edges_tuple_arm_filter has it
	// for its own arms. It is stated after 20261121034242_graph_edges_asks_and_tuple_arm_filter,
	// which restates the view from the arms of 20261116094101 alone, and stated again, last, by
	// 20261123180000_issue_tree_graph_edges_after_store_class_last, after
	// 20261121104002_graph_edges_store_class_last restates it from its own arms, so a database that
	// ran any of them holds every arm of this set again.
	GraphEdgesTupleArmFilter
	// GraphEdgesBlockNodeListed is the set 20261125150001_issue_tree_graph_edges_block_node_listed
	// states: GraphEdgesTupleArmFilter with the tuple arm testing a pointer's target through
	// block_node_listed, which 20261125150000_issue_tree_block_node_listed creates with the body of
	// block_ref_listed, in place of block_ref_listed (R26 of the issue-tree spec,
	// `r-block-type-supertag`). The arm over the block_refs rows that name no node keeps
	// block_ref_listed, since only that row tests a target past the store's address bound. A boot
	// step restates block_node_listed and block_pointer_targets to read a block's type from its
	// node's supertag once the fleet and the store allow it
	// (graphcensus.RunTypedBlockSupertagsStep), which no statement of the view moves.
	GraphEdgesBlockNodeListed
	// GraphEdgesFourArmsDeleted is the set the boot step that deletes the four arms the graph-store
	// spec moved states (graphcensus.RunViewArmsStep), and no migration states: it is
	// GraphEdgesBlockNodeListed without the arm over document_blocks (a block's part_of its
	// document), the arm over block_refs (a block's links) and the two arms over comments
	// (anchored_to and owned_by), and without the tuple arm's join to those three tables. The tuple
	// arm then answers a block's in_document, a block's link of a kind block_refs projects
	// (BlockLinkKindsSQL) and a comment's anchored_to and owned_by from the tuples alone, so no
	// row of the three tables decides an edge. The step states it only where every row of those
	// tables carries its node and the store's census of their kinds reads no difference, so a
	// database that holds a row of them naming no node when the step runs keeps
	// GraphEdgesBlockNodeListed. A row written after the step that names no node, which only an
	// address past the store's bound (graphstore.MaxAddressBytes) still makes, is answered by no arm
	// of this set; the store census counts it as addresses_past_bound. The arms over asks,
	// ask_followers, issues, issue_links, artifacts, components, component_depends,
	// issue_component_members and refs stand as GraphEdgesBlockNodeListed has them.
	GraphEdgesFourArmsDeleted
)

// GraphEdgesView is the statement that states graph_edges with the arms of set arms. A set that
// neither a migration nor the boot step of GraphEdgesFourArmsDeleted states is the caller's mistake,
// and it panics naming it rather than stating a view nobody declared.
//
// Every arm over a projected table that a writer of the store now writes answers only the rows of
// that table that name no node (`node_kind is null`), and the store answers the rest: the tuple
// arm answers an edge of such a kind only while the projection row that names that edge carries
// its node, so a row a process of the release before deletes takes its edge with it, and a row it
// writes again, naming no node, is answered by the old arm alone. Each correlated probe of a
// projection row stands under a CASE and ends in `offset 0`, which Postgres keeps as a fence and
// probes once per edge through the row's own index, where it would otherwise read the whole table
// as a hashed `= any` that enable_seqscan does not steer (the graph-store spec's commit 0f8b389c).
//
// An issue's relations, as GraphEdgesIssueRelations has them: its child_of is answered while the
// issues row carries the issue's node and names that parent, its needs to an issue while the
// issue_links row of the pair carries it, under the kind blocked_by both arms name through this
// release, and its duplicate_of and filed_from from the store alone, since no row projects
// either. The release before's PATCH sets issues.parent_key and leaves the node columns as they
// were, so a marked row can name a parent its node's child_of does not: such a row counts as
// unmarked while that is so (IssueParentMoved, R9 of the issue-tree spec), and the child_of arm
// answers it, through a probe that stands under an OR and ends in `offset 0` as every probe here
// does, until the backfill states the child_of that names its parent. A document's attached_to its
// issue is the part_of the artifact's node states, which the arm over artifacts that carry their
// node answers by the artifact's uuid and at its row's created_at, as the arm over the rows that
// carry none does: an arm's from_id names an artifact by its uuid, which is what every reader of
// the view resolves it by, and the store addresses the node by its ref_key.
//
// A component's relations, as GraphEdgesComponents has them: its part_of is answered while the
// components row carries the component's node and names that parent, its depends_on while the
// component_depends row of the pair carries it, and an issue's affects while the
// issue_component_members row of the issue and the component carries the issue's node. A marked
// components row whose parent no longer matches its node's part_of counts as unmarked
// (ComponentParentMoved), as an issues row does (IssueParentMoved).
//
// A document's references, as GraphEdgesArtifactRefs has them: its mention of a node is answered
// while the document's artifacts row carries its node and the refs row of the pair stands, and by
// the arm over refs otherwise, and for a far end past the store's bound, of which no tuple is
// stated (ReferenceNotStated). The store addresses the document's node by its ref_key, and the
// tuple arm reads its mentions through a join to that row (artifactRefTuples), which names the
// document by its uuid, as the arm over refs and every reader of the view does, and still binds a
// read of one source or one far end to an index of each relation it reads.
//
// From GraphEdgesTupleArmFilter on, the tuple arm's sub-select keeps only the tuples whose
// `projects` is not null, by the test tupleArmFilter spells from the `when` arms of that case:
// both columns the test reads, the tuple's from_kind and kind, are columns of the `distinct on`
// key, so it keeps or drops a whole group and never changes which tuple of a kept group is the
// newest, and a read keyed on one end of an edge sorts only that node's tuples of those kinds.
//
// From GraphEdgesFourArmsDeleted on, the view holds no arm over document_blocks, block_refs or
// comments' anchored_to and owned_by, and the tuple arm joins none of those tables: a block's
// in_document and a comment's anchored_to and owned_by are answered by their tuples while the
// node they come from stands, and a block's link by its tuple of a kind a block_refs row projects
// (BlockLinkKindsSQL), the one test that replaces the join to the row of the link. Without that test
// the arm would answer every tuple out of a block - its text_parent, its authored_by, its kept_by -
// as a link, none of which a block_refs row ever held. The step that states this set proves, before
// it does, that every row of the three tables names its node, so the arms it drops answer nothing
// then; a row written after it that names no node, past the store's address bound, has no edge.
func GraphEdgesView(arms GraphEdgesArms) string {
	if arms < GraphEdgesIssueRelations || arms > GraphEdgesFourArmsDeleted {
		panic(fmt.Sprintf("graph_edges is stated with no arm set %d", arms))
	}
	references, nodeColumn, tuples, nodes := graphEdgesRefs, "", graphEdgesTuples, graphEdgesNodes
	components, classes, probes := issueRelationsComponentArms, "", ""
	if arms >= GraphEdgesComponents {
		components, classes, probes = unmarkedComponentArms, componentClasses, componentProbes
	}
	if arms >= GraphEdgesArtifactRefs {
		references, nodeColumn, tuples, nodes = artifactRefsArm, artifactRefNodeColumn, artifactRefTuples, artifactRefNodes
		classes += artifactRefClass
		probes += artifactRefProbe
	}
	projects := graphEdgesStoreClasses + classes + graphEdgesOwnClasses
	newest, tested := graphEdgesNewest, graphEdgesProbesNotNull
	if arms >= GraphEdgesTupleArmFilter {
		newest, tested = graphEdgesNotValue+tupleArmFilter(projects)+graphEdgesNewestOrder, graphEdgesProbesHead
	}
	pointers := graphEdgesPointersByRow
	if arms >= GraphEdgesBlockNodeListed {
		pointers = graphEdgesPointersByNode
	}
	head, blocks, probesHead, tail := graphEdgesHead, graphEdgesBlocks, graphEdgesProbes, graphEdgesTail
	if arms >= GraphEdgesFourArmsDeleted {
		head, blocks, probesHead, tail = graphEdgesHeadWithoutComments, graphEdgesBlocksWithoutRows, graphEdgesProbesWithoutRows, graphEdgesTailWithoutRows
	}
	return graphEdgesCreate + references + head + components + blocks + nodeColumn + graphEdgesTupleKinds +
		projects + graphEdgesProjectsEnd + tuples + newest + nodes + tested + pointers + probesHead + probes + tail
}

// tupleArmFilter is the tuple arm's test that a tuple's `projects` is not null, spelled over the
// tuple's own columns: each `when` arm of classes, the `projects` case as a set states it, as one
// alternative of a disjunction, each on a line of its own and its own continuation lines joined
// onto it.
func tupleArmFilter(classes string) string {
	var alternatives []string
	condition := ""
	for _, line := range strings.Split(strings.TrimSuffix(classes, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if rest, opens := strings.CutPrefix(line, "when "); opens {
			line = rest
		} else {
			line = condition + " " + line
		}
		head, _, closes := strings.Cut(line, " then '")
		if !closes {
			condition = line
			continue
		}
		alternatives = append(alternatives, "("+head+")")
		condition = ""
	}
	return `      and (` + strings.Join(alternatives, "\n           or ") + `)
`
}

// IssueParentMoved is the test that the issues row aliased i, which names its node, names a parent
// its node does not hang under: the newest child_of tuple out of that node that names the row's
// parent_key does not assert it, or there is none. Only the release before writes such a row: its
// issue PATCH sets parent_key in place and leaves node_kind and node_address as they were, and
// appends no tuple. Such a row counts as unmarked (R9 of the issue-tree spec): graph_edges' old
// child_of arm answers it, refs.BacklinkCounts counts it the same way, and the backfill states the
// child_of that names its parent (graphcensus' object pass). It is the complement, over a marked
// row, of the tuple arm's own test of the same edge, the newest tuple of the relation asserting it
// while the row names that parent, so every parent is answered by exactly one of the two arms. The
// probe reads graph_tuples_current, whose columns are the five it binds and then the sequence, and
// it ends in `offset 0`: it stands under an OR, where Postgres would otherwise read graph_tuples
// whole as a hashed subplan (the graph-store spec's commit 0f8b389c).
const IssueParentMoved = `not exists (
      select 1 from (
        select tuple.asserted from graph_tuples tuple
        where tuple.from_kind = i.node_kind and tuple.from_id = i.node_address and tuple.kind = 'child_of'
          and tuple.to_kind = 'issue' and tuple.to_id = i.parent_key
        order by tuple.seq desc
        limit 1
      ) newest
      where newest.asserted
      offset 0)`

// ComponentParentMoved is IssueParentMoved for a component's parent: the test that the components
// row aliased c, which names its node, names a parent its node does not hang under, the newest
// part_of tuple out of that node that names the row's parent not asserting it, or there being
// none. No importer of this release or of the release before moves a row's parent in place - each
// deletes a source's components and inserts them again, so a row the release before writes names
// no node - but a row updated in place by anything else, leaving its node columns as they were,
// counts as unmarked while that is so (R9 of the issue-tree spec): graph_edges' old part_of arm
// over components answers it, refs.BacklinkCounts counts it the same way, and the backfill states
// the part_of that names its parent (graphcensus' component pass). It is the complement, over a
// marked row, of the tuple arm's own test of the same edge, so every parent is answered by exactly
// one of the two arms. It reads graph_tuples_current and ends in `offset 0`, as IssueParentMoved
// does and for the same reason.
const ComponentParentMoved = `not exists (
      select 1 from (
        select tuple.asserted from graph_tuples tuple
        where tuple.from_kind = c.node_kind and tuple.from_id = c.node_address and tuple.kind = 'part_of'
          and tuple.to_kind = 'component' and tuple.to_id = c.project_key || '/' || c.parent
        order by tuple.seq desc
        limit 1
      ) newest
      where newest.asserted
      offset 0)`

// issueRelationsComponentArms are GraphEdgesIssueRelations' arms over a component's part_of
// (components.parent), its depends_on (component_depends) and an issue's affects
// (issue_component_members), the three over every row. From GraphEdgesComponents on the view takes
// the three over the rows no writer of this release wrote instead (unmarkedComponentArms). Each is
// dated as it always was: a component's edges at its snapshot's imported_at, an issue's affects at
// its issue_components row's updated_at.
const issueRelationsComponentArms = `  select 'component', c.project_key || '/' || c.id, 'part_of', 'component', c.project_key || '/' || c.parent, s.imported_at, null
  from components c join architecture_snapshots s on s.id = c.snapshot_id
  where c.parent is not null
  union all
  select 'component', d.project_key || '/' || d.from_id, 'depends_on', 'component', d.project_key || '/' || d.to_id, s.imported_at, null
  from component_depends d
  join components c on c.project_key = d.project_key and c.id = d.from_id
  join architecture_snapshots s on s.id = c.snapshot_id
  union all
  select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id, c.updated_at, null
  from issue_component_members m join issue_components c using (issue_key)
`

// unmarkedComponentArms are the arms over a component's three relations over the rows no writer
// of this release wrote, the part_of arm also over a marked row whose parent was moved in place
// (ComponentParentMoved): every set's from GraphEdgesComponents on. GraphEdgesFourArmsDeleted
// keeps them as GraphEdgesBlockNodeListed has them: the three arms over a component's relations
// are the root's to delete.
const unmarkedComponentArms = `  select 'component', c.project_key || '/' || c.id, 'part_of', 'component', c.project_key || '/' || c.parent, s.imported_at, null
  from components c join architecture_snapshots s on s.id = c.snapshot_id
  where c.parent is not null
    and (c.node_kind is null or ` + ComponentParentMoved + `)
  union all
  select 'component', d.project_key || '/' || d.from_id, 'depends_on', 'component', d.project_key || '/' || d.to_id, s.imported_at, null
  from component_depends d
  join components c on c.project_key = d.project_key and c.id = d.from_id
  join architecture_snapshots s on s.id = c.snapshot_id
  where d.node_kind is null
  union all
  select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id, c.updated_at, null
  from issue_component_members m join issue_components c using (issue_key)
  where m.node_kind is null
`

// componentClasses are the tuple arm's classes of a component's three relations, which
// GraphEdgesComponents renders and GraphEdgesIssueRelations does not: a component's part_of its
// parent component, its depends_on another component and an issue's affects a component.
const componentClasses = `             when tuple.from_kind = 'component' and tuple.kind = 'part_of' and tuple.to_kind = 'component' then 'component_parent'
             when tuple.from_kind = 'component' and tuple.kind = 'depends_on' and tuple.to_kind = 'component' then 'component_depends'
             when tuple.from_kind = 'issue' and tuple.kind = 'affects' and tuple.to_kind = 'component' then 'component_member'
`

// componentProbes are the probes of componentClasses: each answers its edge while the projection
// row that names it carries the node the tuple comes from, the components row of the component
// while it names that parent, the component_depends row of the pair and the
// issue_component_members row of the issue and the component. Each reads its table through the
// expression index on the address it binds (migrations 0037 and 0038) or its primary key, and
// ends in `offset 0`.
const componentProbes = `          when 'component_parent' then exists (
            select 1 from components projected
            where projected.project_key || '/' || projected.id = edge.from_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id and projected.project_key || '/' || projected.parent = edge.to_id
            offset 0)
          when 'component_depends' then exists (
            select 1 from component_depends projected
            where projected.project_key || '/' || projected.from_id = edge.from_id
              and projected.project_key || '/' || projected.to_id = edge.to_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id
            offset 0)
          when 'component_member' then exists (
            select 1 from issue_component_members projected
            where projected.issue_key = edge.from_id and projected.project_key || '/' || projected.component_id = edge.to_id
              and projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
`

// graphEdgesCreate is the head of the statement every set states the view with.
const graphEdgesCreate = `create or replace view graph_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
`

// graphEdgesRefs is the arm over refs every set before GraphEdgesArtifactRefs states: every row,
// a mention out of any source.
const graphEdgesRefs = `  select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from refs
`

// ReferenceNotStated is the test that the refs row aliased r is one the store states no tuple of,
// which graph_edges' arm over refs answers from GraphEdgesArtifactRefs on: a row whose source is
// not a document, which a comment, a message and an ask write; a document's whose artifacts row
// names no node, which a process of the release before wrote and the backfill states once it names
// the node (graphcensus' object pass and its pass over the references); and a far end past the
// store's bound on an address (referenceAddressBound), which no tuple can hold. Every other row is
// the store's to answer, through its mentions tuple (refs' artifactMentions), and
// refs.BacklinkCounts counts the rows the same way. The probe binds the document by its uuid as
// text, the column the row names it by, which artifacts_id_text indexes, and ends in `offset 0`:
// it stands under an OR, where Postgres would otherwise read artifacts whole as a hashed subplan.
const ReferenceNotStated = `(r.from_kind <> 'artifact' or octet_length(r.to_id) > ` + referenceAddressBound + `
      or not exists (
        select 1 from artifacts mentioning
        where mentioning.id::text = r.from_id and mentioning.node_kind is not null
        offset 0))`

// referenceAddressBound is graphstore.MaxAddressBytes, the store's bound on one address, written
// out as text: graphstore imports this package, so it imports no part of the store
// (graphstore's TestTheArmOverRefsTakesTheStoresAddressBound).
const referenceAddressBound = "1024"

// artifactRefsArm is GraphEdgesArtifactRefs' arm over refs: the rows the store states no tuple of
// (ReferenceNotStated).
const artifactRefsArm = `  select r.from_kind, r.from_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from refs r
  where ` + ReferenceNotStated + `
`

// graphEdgesHead is the view from the arm over refs to the arms over a component's relations,
// which every set before GraphEdgesFourArmsDeleted states as GraphEdgesView has them, and
// graphEdgesHeadWithoutComments the same without the two arms over comments' anchored_to and
// owned_by, which GraphEdgesFourArmsDeleted on does.
const graphEdgesHead = graphEdgesHeadToCommentAnchoredTo + commentAnchoredToArm + graphEdgesHeadToCommentOwnedBy +
	commentOwnedByArm + graphEdgesHeadRest

const graphEdgesHeadWithoutComments = graphEdgesHeadToCommentAnchoredTo + graphEdgesHeadToCommentOwnedBy + graphEdgesHeadRest

// graphEdgesHeadToCommentAnchoredTo is the head from the arm over refs's union to the arm over an
// ask's anchored_to, which stands in every set.
const graphEdgesHeadToCommentAnchoredTo = `  union all
  select 'issue', i.key, 'child_of', 'issue', i.parent_key, i.created_at, null
  from issues i
  where i.parent_key is not null
    and (i.node_kind is null or ` + IssueParentMoved + `)
  union all
  select 'issue', issue_key, 'blocked_by', 'issue', target_key, created_at, null
  from issue_links where kind = 'blocked_by' and node_kind is null
  union all
  select 'artifact', id::text, 'attached_to', 'issue', issue_key, created_at, null
  from artifacts where issue_key is not null and node_kind is null
  union all
  select 'artifact', a.id::text, 'attached_to', 'issue', a.issue_key, a.created_at, null
  from artifacts a
  join graph_nodes node on node.kind = a.node_kind and node.id = a.node_address
  where a.issue_key is not null and node.state <> 'gone'
    and exists (
      select 1 from (
        select tuple.asserted from graph_tuples tuple
        where tuple.from_kind = a.node_kind and tuple.from_id = a.node_address and tuple.kind = 'part_of'
          and tuple.to_kind = 'issue' and tuple.to_id = a.issue_key
        order by tuple.seq desc
        limit 1
      ) newest
      where newest.asserted)
  union all
  select 'ask', k.id::text, 'anchored_to', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
  where k.node_kind is null
`

// commentAnchoredToArm is the arm over comments' anchored_to, over the rows that name no node: the
// first of the four arms GraphEdgesFourArmsDeleted drops.
const commentAnchoredToArm = `  union all
  select 'comment', c.id::text, 'anchored_to', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid
  where c.node_kind is null
`

// graphEdgesHeadToCommentOwnedBy is the arm over an ask's owned_by, which stands in every set.
const graphEdgesHeadToCommentOwnedBy = `  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = k.artifact_id
  where k.node_kind is null
`

// commentOwnedByArm is the arm over comments' owned_by, over the rows that name no node: the
// second of the four arms GraphEdgesFourArmsDeleted drops.
const commentOwnedByArm = `  union all
  select 'comment', c.id::text, 'owned_by', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = c.artifact_id
  where c.node_kind is null
`

// graphEdgesHeadRest is the head from the arms over a reply to the end of the arms over asks'
// followers, which stands in every set.
const graphEdgesHeadRest = `  union all
  select 'comment', id::text, 'replies_to', 'comment', reply_to::text, created_at, null
  from comments where reply_to is not null
  union all
  select 'comment', id::text, 'answers', 'comment', answers::text, created_at, null
  from comments where answers is not null
  union all
  select 'comment', id::text, 'replies_to', 'ask', ask_id::text, created_at, null
  from comments where ask_id is not null
  union all
  select 'message', id::text, 'replies_to', 'message', in_reply_to::text, created_at, null
  from messages where in_reply_to is not null
  union all
  select 'ask', ask_id::text, 'followed_by', 'session', session_id, since, null
  from ask_followers
  where node_kind is null
  union all
`

// graphEdgesBlocks is the view from the arms over a block's links to the tuple arm's class of an
// issue's wait, the last class every set before GraphEdgesFourArmsDeleted states, and
// graphEdgesBlocksWithoutRows the same without the arm over block_refs and the arm over
// document_blocks, which GraphEdgesFourArmsDeleted on does.
const graphEdgesBlocks = graphEdgesBlocksHead + blockRefsArm + documentBlocksArm + graphEdgesBlocksRest

const graphEdgesBlocksWithoutRows = graphEdgesBlocksHead + graphEdgesBlocksRest

const graphEdgesBlocksHead = `  union all
`

// blockRefsArm is the arm over block_refs, over the rows that name no node: the third of the four
// arms GraphEdgesFourArmsDeleted drops. It lists a pointer's row only while block_ref_listed holds.
const blockRefsArm = `  select 'block', from_node, kind, to_kind, to_id, created_at, source_seq
  from block_refs
  where (to_types is null or block_ref_listed(to_types, to_id))
    and node_kind is null
  union all
`

// documentBlocksArm is the arm over document_blocks, over the rows that name no node: the fourth
// of the four arms GraphEdgesFourArmsDeleted drops.
const documentBlocksArm = `  select 'block', d.node_id, 'part_of', 'artifact', a.ref_key, d.created_at, null
  from document_blocks d join artifacts a on a.id = d.artifact_id
  where d.node_kind is null
  union all
`

// graphEdgesBlocksRest is the arms over the blocks of a comment, a message and an ask, which stand
// in every set, to the head of the tuple arm.
const graphEdgesBlocksRest = `  select 'comment_block', b.node_id, 'part_of', 'comment', b.comment_id::text, b.created_at, null
  from comment_blocks b where b.position is not null
  union all
  select 'message_block', b.node_id, 'part_of', 'message', b.message_id::text, b.created_at, null
  from message_blocks b where b.position is not null
  union all
  select 'ask_block', b.node_id, 'part_of', 'ask', b.ask_id::text, b.created_at, null
  from ask_blocks b where b.position is not null
  union all
  select 'comment_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from comment_block_refs r
  join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'message_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from message_block_refs r
  join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'ask_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from ask_block_refs r
  join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'comment', c.id::text, 'replies_to', 'comment_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block not like 'ask/%'
  union all
  select 'comment', c.id::text, 'replies_to', 'ask_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block like 'ask/%'
  union all
  select 'message', m.id::text, 'replies_to', 'message_block', m.replies_to_block, m.created_at, null
  from messages m where m.replies_to_block is not null
  union all
  select edge.from_kind, edge.from_id, edge.kind, edge.to_kind, edge.to_id, edge.created_at, edge.source_seq
  from (
    select distinct on (tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
`

// artifactRefNodeColumn is the column of GraphEdgesArtifactRefs' tuple arm that names the node a
// tuple comes from, which the arm joins graph_nodes on (artifactRefNodes): the store addresses a
// document by its ref_key, while the arm names it by its uuid (artifactRefTuples).
const artifactRefNodeColumn = `           tuple.node_id,
`

// graphEdgesTupleKinds is the tuple arm from the kind it renders to the head of its `projects`
// case, whose classes every set states from graphEdgesStoreClasses on.
const graphEdgesTupleKinds = `           case
             when tuple.kind = 'in_document' then 'part_of'
             when tuple.from_kind = 'issue' and tuple.kind = 'needs' and tuple.to_kind = 'issue' then 'blocked_by'
             else tuple.kind
           end as kind,
           tuple.to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.kind as stated,
           tuple.asserted,
           case
`

// storeClassKinds are the relations of the class `store` out of any node but a finding, which the
// tuple arm's arm over a finding (the `lens_block` arm of graphEdgesTailRest) answers out of a
// finding too, beside its part_of and its authored_by: one list, so the two arms name the same
// kinds.
const storeClassKinds = `'changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by'`

// graphEdgesStoreClasses are the tuple arm's classes from a block's membership to an issue's wait,
// the classes every set states before a set's own.
const graphEdgesStoreClasses = `             when tuple.from_kind = 'block' and tuple.kind = 'in_document' then 'block'
             when not (tuple.from_kind = 'block' and tuple.from_id like 'lens/%')
               and (tuple.kind in (` + storeClassKinds + `)
                    or (tuple.kind in ('part_of', 'authored_by')
                        and tuple.from_kind in ('interaction', 'lens', 'risk', 'constraint'))) then 'store'
             when tuple.from_kind = 'issue' and tuple.kind in ('duplicate_of', 'filed_from') then 'store'
             when tuple.from_kind = 'issue' and tuple.kind = 'child_of' and tuple.to_kind = 'issue' then 'issue_parent'
             when tuple.from_kind = 'issue' and tuple.kind = 'needs' and tuple.to_kind = 'issue' then 'issue_link'
`

// artifactRefClass is GraphEdgesArtifactRefs' class of a document's mention of a node, whose
// projection row is the refs row of the pair (artifactRefTuples, artifactRefProbe).
const artifactRefClass = `             when tuple.from_kind = 'artifact' and tuple.kind = 'mentions' then 'artifact_ref'
`

// artifactRefProbe is the class's test of its projection row, which the relation the arm reads a
// document's mentions from has made already: it reads each mention only beside the refs row of
// the pair (artifactRefTuples), so every one that reaches the test stands.
const artifactRefProbe = `          when 'artifact_ref' then true
`

// graphEdgesOwnClasses are the tuple arm's classes from a block's link to the last, which every
// set states after a set's own.
const graphEdgesOwnClasses = `             when tuple.from_kind = 'block' and tuple.kind <> 'part_of' then 'block_ref'
             when tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by') then 'comment'
             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
`

// graphEdgesProjectsEnd closes the tuple arm's `projects` case.
const graphEdgesProjectsEnd = `           end as projects
`

// graphEdgesTuples is the relation the tuple arm reads its tuples from in every set before
// GraphEdgesArtifactRefs: the log itself.
const graphEdgesTuples = `    from graph_tuples tuple
`

// artifactRefTuples is the relation GraphEdgesArtifactRefs' tuple arm reads its tuples from: the
// log, with each of a document's mentions read through the artifacts row whose node it comes
// from, which names the document by its uuid and its node by its ref_key (node_id), and beside
// the refs row of the pair. The artifacts row is joined only while it names its node, so a
// document whose row names none is the arm over refs' alone (ReferenceNotStated). The refs row is
// probed per tuple through refs' primary key and the probe ends in `offset 0`, as every probe of
// a projection row does; a mention whose row is gone - one a process of the release before
// deleted with no retraction - is read nowhere, and one whose row stands is dated and sequenced by
// that row, its created_at and its source_seq, as the arm over refs answers it: a row rebuild-refs
// inserted with no source_seq takes the one a later version's event stamps on it, which its tuple,
// appended before, cannot. The two parts of the log are read apart rather than through one
// expression over both, so a read of one source or one far end is pushed into each part and binds
// an index there: graph_tuples' own for every other tuple, and artifacts_id_text, then
// graph_tuples_current, for a document's mentions read by the document. The second part names its
// from_kind as the constant it is, 'artifact', so a read that names the kinds of the nodes it reads
// out of (graphcensus' graphEdgesOut) cuts that part from its plan where artifact is none of them,
// as it cuts every other arm whose source kind is a constant.
const artifactRefTuples = `    from (
      select tuple.from_kind, tuple.from_id, tuple.from_id as node_id, tuple.kind, tuple.to_kind, tuple.to_id,
             tuple.at, tuple.legacy_source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      where tuple.from_kind <> 'artifact' or tuple.kind <> 'mentions'
      union all
      select 'artifact', mentioning.id::text, tuple.from_id, tuple.kind, tuple.to_kind, tuple.to_id,
             projected.created_at, projected.source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      join artifacts mentioning on mentioning.ref_key = tuple.from_id and mentioning.node_kind is not null
      cross join lateral (
        select stated.created_at, stated.source_seq from refs stated
        where stated.from_kind = tuple.from_kind and stated.from_id = mentioning.id::text
          and stated.to_kind = tuple.to_kind and stated.to_id = tuple.to_id
        offset 0) projected
      where tuple.from_kind = 'artifact' and tuple.kind = 'mentions'
    ) tuple
`

// graphEdgesNewest is the tuple arm's pick of the newest tuple of each relation in every set
// before GraphEdgesTupleArmFilter: its test that the far end is no value
// (graphEdgesNotValue), then its order (graphEdgesNewestOrder).
const graphEdgesNewest = graphEdgesNotValue + graphEdgesNewestOrder

// graphEdgesNotValue is the tuple arm's test that a tuple's far end is no value, which the
// filter of GraphEdgesTupleArmFilter follows (tupleArmFilter).
const graphEdgesNotValue = `    where tuple.to_kind <> 'value'
`

// graphEdgesNewestOrder is the tuple arm's order, newest tuple of each relation first, and the
// end of its sub-select.
const graphEdgesNewestOrder = `    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) edge
`

// graphEdgesNodes is the tuple arm's join to the node each tuple comes from in every set before
// GraphEdgesArtifactRefs, whose from_id is that node's id.
const graphEdgesNodes = `  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
`

// artifactRefNodes is GraphEdgesArtifactRefs' join to the node each tuple comes from, by the
// node's own id (artifactRefNodeColumn).
const artifactRefNodes = `  join graph_nodes node on node.kind = edge.from_kind and node.id = edge.node_id
`

// graphEdgesProbesNotNull is the head of the tuple arm's tests of an edge in every set before
// GraphEdgesTupleArmFilter, which keeps no row whose `projects` is null after the sub-select has
// sorted it.
const graphEdgesProbesNotNull = `  where edge.asserted and node.state <> 'gone' and edge.projects is not null
`

// graphEdgesProbesHead is the head of the tuple arm's tests of an edge from
// GraphEdgesTupleArmFilter on, whose sub-select has kept only the rows whose `projects` is not
// null (tupleArmFilter).
const graphEdgesProbesHead = `  where edge.asserted and node.state <> 'gone'
`

// graphEdgesPointersByRow and graphEdgesPointersByNode are the tuple arm's test that a pointer is
// listed, after its tests' head: through block_ref_listed in every set before
// GraphEdgesBlockNodeListed, and through block_node_listed from it on, whose body a boot step
// restates to read the target's type from its node (graphcensus.RunTypedBlockSupertagsStep). The
// type of the pointer's source is block_pointer_targets' to read in either.
const (
	graphEdgesPointersByRow = `    and (edge.projects <> 'block_ref' or edge.stated = 'mentions'
         or block_ref_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id))
`
	graphEdgesPointersByNode = `    and (edge.projects <> 'block_ref' or edge.stated = 'mentions'
         or block_node_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id))
`
)

// graphEdgesProbes is the tuple arm from its test that a pointer is listed to the probe of an
// issue's wait, the last probe every set states before a set's own, with the probe of a block's row
// in every set before GraphEdgesFourArmsDeleted, and graphEdgesProbesWithoutRows the same with that
// probe answering true for the class `block`, as GraphEdgesFourArmsDeleted on does: the tuple of a
// block's in_document is answered while the block's node stands, which the join to the node and
// the test of its state have made already.
const graphEdgesProbes = graphEdgesProbesOpen + documentBlocksProbe + graphEdgesProbesIssues

const graphEdgesProbesWithoutRows = graphEdgesProbesOpen + documentBlocksRowless + graphEdgesProbesIssues

// graphEdgesProbesOpen opens the case that tests an edge by the class of its tuple.
const graphEdgesProbesOpen = `    and case edge.projects
          when 'store' then true
`

// documentBlocksProbe is the class `block`'s test of its projection row, the document_blocks row
// of the block that names its node.
const documentBlocksProbe = `          when 'block' then exists (
            select 1 from document_blocks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
`

// documentBlocksRowless is the class `block`'s test from GraphEdgesFourArmsDeleted on: none.
const documentBlocksRowless = `          when 'block' then true
`

// graphEdgesProbesIssues is the probes of an issue's parent and of an issue's wait.
const graphEdgesProbesIssues = `          when 'issue_parent' then exists (
            select 1 from issues projected
            where projected.key = edge.from_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id and projected.parent_key = edge.to_id
            offset 0)
          when 'issue_link' then exists (
            select 1 from issue_links projected
            where projected.issue_key = edge.from_id and projected.kind = 'blocked_by'
              and projected.target_key = edge.to_id and projected.node_kind = edge.from_kind
              and projected.node_address = edge.from_id
            offset 0)
`

// graphEdgesTail is the view after the probes of the tuple arm's classes of a set's own, from the
// probe of a block's link on, with the probes of the block_refs row of a link and the comments row
// of a comment in every set before GraphEdgesFourArmsDeleted, and graphEdgesTailWithoutRows the
// same with neither: from that set on a block's link is answered while its kind is one a block_refs
// row projects (BlockLinkKindsSQL), and a comment's anchored_to and owned_by while its node stands.
const graphEdgesTail = blockRefsProbe + commentsProbe + graphEdgesTailRest

const graphEdgesTailWithoutRows = blockLinksProbe + commentsRowless + graphEdgesTailRest

// blockRefsProbe is the class `block_ref`'s test of its projection row, the block_refs row of the
// block, kind and far end of the link.
const blockRefsProbe = `          when 'block_ref' then exists (
            select 1 from block_refs projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.kind = edge.stated and projected.to_kind = edge.to_kind
              and projected.to_id = edge.to_id
            offset 0)
`

// commentsProbe is the class `comment`'s test of its projection row, the comments row that names
// the comment's node.
const commentsProbe = `          when 'comment' then exists (
            select 1 from comments projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
`

// BlockLinkKindsSQL is the select of every kind of link out of a block that a block_refs row
// projects, one row each, which the class `block_ref` answers from GraphEdgesFourArmsDeleted on in
// place of its join to a block_refs row: the tuples out of a block of any other kind - text_parent,
// authored_by, kept_by, moved_to - are no link of the view, as no row of block_refs ever named one.
// It is a mention, the name of every `block[]` attribute the store's type nodes declare at any
// state (refs.BlockRefKindsSQL's own select) and a decision's three ties, shaped, from_ask and
// decided_by (refs' typedKinds), so refs.BacklinkCounts, which reads a block's links as the view
// does, takes the same select. The attribute's kind is pmdoc.BlockAttributeBlockList, written out
// as text since this package imports no part of pmdoc. The select names no column of the tuple, so
// Postgres reads it once for the statement.
const BlockLinkKindsSQL = `select 'mentions'
          union select 'shaped'
          union select 'from_ask'
          union select 'decided_by'
          union select attribute.attributes->>'name' from graph_nodes attribute
          where attribute.kind = 'attribute' and attribute.attributes->>'kind' = 'block[]'`

// blockLinksProbe is the class `block_ref`'s test from GraphEdgesFourArmsDeleted on: the kind of
// the tuple is one a block_refs row projects. The class holds a block's tuples of any kind but
// its part_of and an ask's shaped, and a pointer's target is listed by the test that follows it.
const blockLinksProbe = `          when 'block_ref' then (edge.stated in (` + BlockLinkKindsSQL + `))
`

// commentsRowless is the class `comment`'s test from GraphEdgesFourArmsDeleted on: none.
const commentsRowless = `          when 'comment' then true
`

// graphEdgesTailRest is the view from the probe of an ask to its end, which stands in every set.
const graphEdgesTailRest = `          when 'ask' then exists (
            select 1 from asks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'ask_follower' then exists (
            select 1 from ask_followers projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.session_id = edge.to_id
            offset 0)
        end
  union all
  select 'lens_block', finding.from_id, finding.kind, finding.to_kind, finding.to_id, finding.created_at, finding.source_seq
  from (
    select distinct on (tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id)
           tuple.from_kind,
           tuple.from_id,
           tuple.kind,
           tuple.to_kind,
           tuple.to_id,
           tuple.at as created_at,
           tuple.legacy_source_seq as source_seq,
           tuple.asserted
    from graph_tuples tuple
    where tuple.from_kind = 'block' and tuple.from_id like 'lens/%'
      and tuple.kind in ('part_of', 'authored_by', ` + storeClassKinds + `)
      and tuple.to_kind <> 'value'
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) finding
  join graph_nodes node on node.kind = finding.from_kind and node.id = finding.from_id
  where finding.asserted and node.state <> 'gone';
`
