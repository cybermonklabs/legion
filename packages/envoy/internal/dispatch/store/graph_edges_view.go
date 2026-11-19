package store

import "fmt"

// GraphEdgesArms names one set of the arms graph_edges is stated with from
// dispatch://INFRA-1253's split of the issue relations on: each migration that states the view
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
)

// GraphEdgesView is the statement that states graph_edges with the arms of set arms. A set no
// migration states is the caller's mistake, and it panics naming it rather than stating a view
// nobody declared.
//
// Every arm over a projected table that a writer of the store now writes answers only the rows of
// that table that name no node (`node_kind is null`), and the store answers the rest: the tuple
// arm answers an edge of such a kind only while the projection row that names that edge carries
// its node, so a row a process of the release before deletes takes its edge with it, and a row it
// writes again, naming no node, is answered by the old arm alone. Each correlated probe of a
// projection row stands under a CASE and ends in `offset 0`, which Postgres keeps as a fence and
// probes once per edge through the row's own index, where it would otherwise read the whole table
// as a hashed `= any` that enable_seqscan does not steer (dispatch://INFRA-1149's 0f8b389c).
//
// An issue's relations, as GraphEdgesIssueRelations has them: its child_of is answered while the
// issues row carries the issue's node and names that parent, its needs to an issue while the
// issue_links row of the pair carries it, under the kind blocked_by both arms name through this
// release, and its duplicate_of and filed_from from the store alone, since no row projects
// either. The release before's PATCH sets issues.parent_key and leaves the node columns as they
// were, so a marked row can name a parent its node's child_of does not: such a row counts as
// unmarked while that is so (IssueParentMoved, R9 of dispatch://INFRA-1253), and the child_of arm
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
func GraphEdgesView(arms GraphEdgesArms) string {
	components, stated := componentArms[arms]
	if !stated {
		panic(fmt.Sprintf("graph_edges is stated with no arm set %d", arms))
	}
	references, nodeColumn, tuples, nodes := graphEdgesRefs, "", graphEdgesTuples, graphEdgesNodes
	classes, probes := "", ""
	if arms >= GraphEdgesComponents {
		classes, probes = componentClasses, componentProbes
	}
	if arms >= GraphEdgesArtifactRefs {
		references, nodeColumn, tuples, nodes = artifactRefsArm, artifactRefNodeColumn, artifactRefTuples, artifactRefNodes
		classes += artifactRefClass
		probes += artifactRefProbe
	}
	return graphEdgesCreate + references + graphEdgesHead + components + graphEdgesBlocks + nodeColumn + graphEdgesTupleKinds +
		classes + graphEdgesOwnClasses + tuples + graphEdgesNewest + nodes + graphEdgesProbes + probes + graphEdgesTail
}

// IssueParentMoved is the test that the issues row aliased i, which names its node, names a parent
// its node does not hang under: the newest child_of tuple out of that node that names the row's
// parent_key does not assert it, or there is none. Only the release before writes such a row: its
// issue PATCH sets parent_key in place and leaves node_kind and node_address as they were, and
// appends no tuple. Such a row counts as unmarked (R9 of dispatch://INFRA-1253): graph_edges' old
// child_of arm answers it, refs.BacklinkCounts counts it the same way, and the backfill states the
// child_of that names its parent (graphcensus' object pass). It is the complement, over a marked
// row, of the tuple arm's own test of the same edge, the newest tuple of the relation asserting it
// while the row names that parent, so every parent is answered by exactly one of the two arms. The
// probe reads graph_tuples_current, whose columns are the five it binds and then the sequence, and
// it ends in `offset 0`: it stands under an OR, where Postgres would otherwise read graph_tuples
// whole as a hashed subplan (dispatch://INFRA-1149's 0f8b389c).
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
// counts as unmarked while that is so (R9 of dispatch://INFRA-1253): graph_edges' old part_of arm
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

// componentArms are, for each set, the arms over a component's part_of (components.parent), its
// depends_on (component_depends) and an issue's affects (issue_component_members): the three over
// every row in GraphEdgesIssueRelations, and from GraphEdgesComponents on the three over the rows
// no writer of this release wrote (unmarkedComponentArms). Each is dated as it always was: a
// component's edges at its snapshot's imported_at, an issue's affects at its issue_components
// row's updated_at.
var componentArms = map[GraphEdgesArms]string{
	GraphEdgesIssueRelations: `  select 'component', c.project_key || '/' || c.id, 'part_of', 'component', c.project_key || '/' || c.parent, s.imported_at, null
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
`,
	GraphEdgesComponents:   unmarkedComponentArms,
	GraphEdgesArtifactRefs: unmarkedComponentArms,
}

// unmarkedComponentArms are the arms over a component's three relations over the rows no writer
// of this release wrote, the part_of arm also over a marked row whose parent was moved in place
// (ComponentParentMoved).
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
// which every set states as GraphEdgesView has them.
const graphEdgesHead = `  union all
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
  union all
  select 'comment', c.id::text, 'anchored_to', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid
  where c.node_kind is null
  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = k.artifact_id
  where k.node_kind is null
  union all
  select 'comment', c.id::text, 'owned_by', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = c.artifact_id
  where c.node_kind is null
  union all
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
// issue's wait, the last class every set states.
const graphEdgesBlocks = `  union all
  select 'block', from_node, kind, to_kind, to_id, created_at, source_seq
  from block_refs
  where (to_types is null or block_ref_listed(to_types, to_id))
    and node_kind is null
  union all
  select 'block', d.node_id, 'part_of', 'artifact', a.ref_key, d.created_at, null
  from document_blocks d join artifacts a on a.id = d.artifact_id
  where d.node_kind is null
  union all
  select 'comment_block', b.node_id, 'part_of', 'comment', b.comment_id::text, b.created_at, null
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

// graphEdgesTupleKinds is the tuple arm from the kind it renders to its class of an issue's wait,
// the last class every set states before a set's own.
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
             when tuple.from_kind = 'block' and tuple.kind = 'in_document' then 'block'
             when not (tuple.from_kind = 'block' and tuple.from_id like 'lens/%')
               and (tuple.kind in ('changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by')
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

// graphEdgesOwnClasses is the tuple arm from the class of a block's link to the end of its
// classes, which every set states after a set's own.
const graphEdgesOwnClasses = `             when tuple.from_kind = 'block' and tuple.kind <> 'part_of' then 'block_ref'
             when tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by') then 'comment'
             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
           end as projects
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
// graph_tuples_current, for a document's mentions read by the document.
const artifactRefTuples = `    from (
      select tuple.from_kind, tuple.from_id, tuple.from_id as node_id, tuple.kind, tuple.to_kind, tuple.to_id,
             tuple.at, tuple.legacy_source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      where tuple.from_kind <> 'artifact' or tuple.kind <> 'mentions'
      union all
      select tuple.from_kind, mentioning.id::text, tuple.from_id, tuple.kind, tuple.to_kind, tuple.to_id,
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

// graphEdgesNewest is the tuple arm's pick of the newest tuple of each relation.
const graphEdgesNewest = `    where tuple.to_kind <> 'value'
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
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

// graphEdgesProbes is the tuple arm from its tests of an edge to the probe of an issue's wait,
// the last probe every set states before a set's own.
const graphEdgesProbes = `  where edge.asserted and node.state <> 'gone' and edge.projects is not null
    and (edge.projects <> 'block_ref' or edge.stated = 'mentions'
         or block_ref_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id))
    and case edge.projects
          when 'store' then true
          when 'block' then exists (
            select 1 from document_blocks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'issue_parent' then exists (
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
// probe of a block's link on.
const graphEdgesTail = `          when 'block_ref' then exists (
            select 1 from block_refs projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.kind = edge.stated and projected.to_kind = edge.to_kind
              and projected.to_id = edge.to_id
            offset 0)
          when 'comment' then exists (
            select 1 from comments projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'ask' then exists (
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
      and tuple.kind in ('part_of', 'authored_by', 'changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by')
      and tuple.to_kind <> 'value'
    order by tuple.to_kind, tuple.to_id, tuple.kind, tuple.from_kind, tuple.from_id, tuple.seq desc
  ) finding
  join graph_nodes node on node.kind = finding.from_kind and node.id = finding.from_id
  where finding.asserted and node.state <> 'gone';
`
