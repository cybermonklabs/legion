package store

import (
	"fmt"
	"strings"
)

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
	// GraphEdgesTupleArmFilter is the set 20261121090000_issue_tree_graph_edges_after_tuple_arm_filter
	// states: GraphEdgesArtifactRefs with the tuple arm's sub-select keeping only the kinds the arm
	// renders before its `distinct on` sorts the tuples, the test spelled over the tuple's own
	// columns as the `when` arms of the `projects` case (tupleArmFilter), and the outer
	// `edge.projects is not null` test gone, as the sibling branch's
	// 20261120122700_graph_edges_tuple_arm_filter has it for its own arms. It is stated last, after
	// 20261121034242_graph_edges_asks_and_tuple_arm_filter, which restates the view from the arms of
	// 20261116094101 alone, so a database that ran either holds every arm of this branch again.
	GraphEdgesTupleArmFilter
	// GraphEdgesOneTupleArm is the set 20261123120000_graph_edges_one_tuple_arm states:
	// GraphEdgesTupleArmFilter with the two arms that read the store beside the tuple arm folded
	// into it, so every arm left over the store is the one select over the tuples. A finding of a
	// lens's relations, which the arm over graph_tuples after the tuple arm answered as a
	// lens_block, are a part of the tuple arm's sub-select of their own, named lens_block there,
	// under a class that consults no projection row (lensClass). A document's attached_to its
	// issue, which the arm over the artifacts that carry their node answered from the node's
	// part_of, is a part of its own too, read through the artifacts row that names the node and the
	// issue, as the arm read it, and answered at that row's created_at with no sequence
	// (attachedClass). Neither changes a row the view answers: each part keeps the tuples its arm
	// kept, and the `distinct on` reads the same groups, since no two parts share a source.
	GraphEdgesOneTupleArm
)

// graphEdgesNewestSet is the newest set a migration states, which the boot step restates the view
// from less each child's arms it has deleted (GraphEdgesViewRetired).
const graphEdgesNewestSet = GraphEdgesOneTupleArm

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
//
// From GraphEdgesTupleArmFilter on, the tuple arm's sub-select keeps only the tuples whose
// `projects` is not null, by the test tupleArmFilter spells from the `when` arms of that case:
// both columns the test reads, the tuple's from_kind and kind, are columns of the `distinct on`
// key, so it keeps or drops a whole group and never changes which tuple of a kept group is the
// newest, and a read keyed on one end of an edge sorts only that node's tuples of those kinds.
//
// From GraphEdgesOneTupleArm on, the statement also forgets every child whose arms the boot step
// deleted (graph_edges_retirements, migration 20261122200000): a migration stating the view with
// every arm puts back every arm the step deleted, so the count must consult every child's markers
// again (GraphEdgesArmsRetired) and the step must delete them again at its next boot.
func GraphEdgesView(arms GraphEdgesArms) string {
	if arms >= GraphEdgesOneTupleArm {
		return graphEdgesView(arms, 0) + graphEdgesRetirementsForgotten
	}
	return graphEdgesView(arms, 0)
}

// graphEdgesRetirementsForgotten is the statement a migration that states the view with every arm
// ends with (GraphEdgesView).
const graphEdgesRetirementsForgotten = `delete from graph_edges_retirements;
`

// GraphEdgesRetired names the children whose old arms of graph_edges the boot step has deleted
// (startup's retirements of the arms), one bit each: no migration states the view without them,
// because a migration runs whatever build a rollback would reach, while the step deletes a
// child's arms only once the build a rollback reaches writes everything those arms answer into the
// store - that child's kinds as tuples and, for dispatch://INFRA-1253's, a document's row values on
// its node - and the store is proven to answer every edge those arms answered
// (dispatch://INFRA-1098's T7b).
type GraphEdgesRetired uint8

const (
	// GraphEdgesAsksRetired is dispatch://INFRA-1150's part: the arms over asks (an ask's
	// anchored_to and owned_by) and over ask_followers (followed_by), and the tuple arm's probes
	// of those two tables, whose classes then consult no projection row (`store`).
	GraphEdgesAsksRetired GraphEdgesRetired = 1 << iota
	// GraphEdgesIssueTreeRetired is dispatch://INFRA-1253's part: the arms over issues
	// (child_of), issue_links (blocked_by), the artifacts that name no node (attached_to),
	// components (part_of), component_depends (depends_on) and issue_component_members (affects),
	// the tuple arm's probes of those five tables, whose classes then consult no projection row
	// (`store`), and the artifact half of the arm over refs: the rows of a document whose row
	// names no node and the rows of a far end past the store's bound, which no writer of refs
	// writes any more (ReferenceOfNoDocument). With them go the tuple arm's reads of the artifacts
	// row and the refs row behind a document's mentions and its attached_to: the arm reads the
	// document's uuid and its row's created_at off the document's node (artifactStoreTuples,
	// graphstore's ArtifactRow), and a mention's date off its tuple, which every writer of it
	// dates as its refs row (graphstore.QueueMentionsDated).
	GraphEdgesIssueTreeRetired
)

// GraphEdgesChild is one child whose old arms the boot step deletes, by the name its row of
// graph_edges_retirements (migration 20261122200000) carries.
type GraphEdgesChild struct {
	Retired GraphEdgesRetired
	Name    string
}

// GraphEdgesChildren are the children whose old arms the boot step deletes, each with the name
// the persisted fact of its deletion carries: a row of graph_edges_retirements, which the step
// writes in the transaction that restates graph_edges without that child's arms, and only there.
// It is the one fact that says whether a child's arms stand: the step reads it to know which arms
// the view still holds (GraphEdgesViewRetired), and refs.BacklinkCounts reads it in every count
// statement to know whether to consult that child's markers (GraphEdgesArmsRetired), so the count
// and the view agree on every database, one whose step kept a child's arms included.
var GraphEdgesChildren = []GraphEdgesChild{
	{GraphEdgesAsksRetired, "asks"},
	{GraphEdgesIssueTreeRetired, "issue_tree"},
}

// GraphEdgesArmsRetired is the test that the boot step has deleted the arms of child, one of
// GraphEdgesChildren: graph_edges_retirements holds the row naming it. It names no column of the
// statement it stands in, so Postgres reads it once for the statement, through the table's primary
// key. A child that is none of GraphEdgesChildren is the caller's mistake, and it panics naming
// it.
func GraphEdgesArmsRetired(child GraphEdgesRetired) string {
	for _, known := range GraphEdgesChildren {
		if known.Retired == child {
			return `exists (select 1 from graph_edges_retirements where child = '` + known.Name + `')`
		}
	}
	panic(fmt.Sprintf("graph_edges has no child %d whose arms the boot step deletes", child))
}

// GraphEdgesViewRetired is the statement that states graph_edges with the arms of the newest set
// (graphEdgesNewestSet) less every arm, and every probe of the tuple arm, of each child retired
// names. Every arm and probe of a child retired does not name is stated as that set states it. A
// child's arms answer only the rows its marker does not mark, and its probes keep a tuple only
// while the row that names it carries its node, so over a database where every row those arms
// read is marked and every current tuple of their kinds is named by such a row, the view this
// states answers what the newest set's answers, row for row.
func GraphEdgesViewRetired(retired GraphEdgesRetired) string {
	return graphEdgesView(graphEdgesNewestSet, retired)
}

// graphEdgesView is the statement that states graph_edges with the arms of set arms, less those of
// each child retired names (GraphEdgesViewRetired).
func graphEdgesView(arms GraphEdgesArms, retired GraphEdgesRetired) string {
	components, stated := componentArms[arms]
	if !stated {
		panic(fmt.Sprintf("graph_edges is stated with no arm set %d", arms))
	}
	asks, issueTree := retired&GraphEdgesAsksRetired != 0, retired&GraphEdgesIssueTreeRetired != 0
	references, nodeColumn, tuples, nodes := graphEdgesRefs, "", graphEdgesTuples, graphEdgesNodes
	issueArms, issueClassed, issueProbing := issueRelationArms, issueClasses, issueProbes
	if issueTree {
		issueArms, issueClassed, issueProbing, components = "", storeClassed(issueClasses), "", ""
	} else {
		components = graphEdgesUnion + components
	}
	askAnchored, askOwned, askFollowers, askClassed, askProbing := askAnchoredArm, askOwnedArm, askFollowerArm, askClasses, askProbes
	if asks {
		askAnchored, askOwned, askFollowers, askClassed, askProbing = "", "", "", storeClassed(askClasses), ""
	}
	classes, probes := "", ""
	if arms >= GraphEdgesComponents {
		classes, probes = componentClasses, componentProbes
		if issueTree {
			classes, probes = storeClassed(componentClasses), ""
		}
	}
	attached, lens, tail := attachedArtifactArm, "", graphEdgesTail
	switch {
	case arms >= GraphEdgesOneTupleArm:
		references, nodeColumn, tuples, nodes = artifactRefsArm, oneTupleArmNodeColumns, oneTupleArmTuples(artifactRowTuples), oneTupleArmNodes
		attached, lens, tail = "", lensClass, graphEdgesEnd
		if issueTree {
			references, tuples = refsOfItemsArm, oneTupleArmTuples(artifactStoreTuples)
			classes += storeClassed(artifactRefClass + attachedClass)
		} else {
			classes += artifactRefClass + attachedClass
			probes += artifactRefProbe + attachedProbe
		}
	case arms >= GraphEdgesArtifactRefs:
		references, nodeColumn, tuples, nodes = artifactRefsArm, artifactRefNodeColumn, artifactRefTuples(artifactRefMarked), artifactRefNodes
		if issueTree {
			references, tuples = refsOfItemsArm, artifactRefTuples("")
		}
		classes += artifactRefClass
		probes += artifactRefProbe
	}
	projects := lens + graphEdgesStoreClasses + issueClassed + classes + graphEdgesOwnClasses + askClassed + graphEdgesShapedClass
	newest, tested := graphEdgesNewest, graphEdgesProbesNotNull
	if arms >= GraphEdgesTupleArmFilter {
		newest, tested = graphEdgesNotValue+tupleArmFilter(projects)+graphEdgesNewestOrder, graphEdgesProbesHead
	}
	return graphEdgesCreate + references + issueArms + attached + askAnchored + commentAnchoredArm + askOwned +
		commentOwnedArm + pointerArms + askFollowers + components + graphEdgesBlocks + nodeColumn + graphEdgesTupleKinds +
		projects + graphEdgesProjectsEnd + tuples + newest + nodes + tested + graphEdgesProbes + issueProbing + probes +
		blockRefProbes + askProbing + tail
}

// storeClassed is classes, `when` arms of the tuple arm's `projects` case each on one line, with
// each one's class written `store`, the class that consults no projection row: the class of a
// relation whose probe the boot step has deleted with its child's arms (GraphEdgesViewRetired).
// The tests of each arm are kept, so the tuple arm's filter (tupleArmFilter) keeps the same
// tuples.
func storeClassed(classes string) string {
	var classed strings.Builder
	for _, line := range strings.SplitAfter(classes, "\n") {
		if head, _, found := strings.Cut(line, " then '"); found {
			line = head + " then 'store'\n"
		}
		classed.WriteString(line)
	}
	return classed.String()
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
	GraphEdgesComponents:     unmarkedComponentArms,
	GraphEdgesArtifactRefs:   unmarkedComponentArms,
	GraphEdgesTupleArmFilter: unmarkedComponentArms,
	GraphEdgesOneTupleArm:    unmarkedComponentArms,
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

// ReferenceOfNoDocument is the test ReferenceNotStated makes once the boot step has deleted
// dispatch://INFRA-1253's half of the arm over refs (GraphEdgesIssueTreeRetired): the refs row
// aliased r is one the store states no tuple of because its source is not a document, which is
// dispatch://INFRA-1152's half of the arm. A document whose row names no node is no longer one of
// them: the step deletes that half only where every document's row that a refs row names carries
// its node. Nor is a far end past the store's bound: no writer of refs writes one any more
// (refs.ReferenceTarget), and the step deletes that branch only where no refs row holds one, so
// live production, which no test reads, loses no edge to it in silence. refs.BacklinkCounts counts
// the rows the same way.
const ReferenceOfNoDocument = `(r.from_kind <> 'artifact')`

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

// refsOfItemsArm is the arm over refs once dispatch://INFRA-1253's half of it is deleted: the rows
// of a comment, a message and an ask (ReferenceOfNoDocument).
const refsOfItemsArm = `  select r.from_kind, r.from_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from refs r
  where ` + ReferenceOfNoDocument + `
`

// graphEdgesUnion opens every arm after the first.
const graphEdgesUnion = `  union all
`

// issueRelationArms are dispatch://INFRA-1253's arms over an issue's relations and a document's
// attachment, every set from GraphEdgesIssueRelations states them, over the rows that name no node:
// an issue's child_of, also over a marked row whose parent was moved in place (IssueParentMoved),
// its blocked_by, and a document's attached_to its issue. The boot step deletes them
// (GraphEdgesIssueTreeRetired).
const issueRelationArms = `  union all
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
`

// attachedArtifactArm is the arm over the artifacts that carry their node, which answers each
// one's attached_to its issue from its node's part_of tuple, naming the artifact by its uuid, as
// every reader of the view resolves it, where the store addresses its node by its ref_key. Every
// set from GraphEdgesIssueRelations to GraphEdgesTupleArmFilter states it; GraphEdgesOneTupleArm
// reads the same part_of in the tuple arm (oneTupleArmTuples).
const attachedArtifactArm = `  union all
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
`

// askAnchoredArm is dispatch://INFRA-1150's arm over an ask's anchored_to, over the rows that name
// no node, which the boot step deletes (GraphEdgesAsksRetired).
const askAnchoredArm = `  union all
  select 'ask', k.id::text, 'anchored_to', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
  where k.node_kind is null
`

// commentAnchoredArm is the arm over a comment's anchored_to, over the rows that name no node: one
// of the four arms dispatch://INFRA-1149 moved, which dispatch://INFRA-1253 deletes.
const commentAnchoredArm = `  union all
  select 'comment', c.id::text, 'anchored_to', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid
  where c.node_kind is null
`

// askOwnedArm is dispatch://INFRA-1150's arm over an ask's owned_by, over the rows that name no
// node, which the boot step deletes (GraphEdgesAsksRetired).
const askOwnedArm = `  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = k.artifact_id
  where k.node_kind is null
`

// commentOwnedArm is the arm over a comment's owned_by, over the rows that name no node: one of
// the four arms dispatch://INFRA-1149 moved, which dispatch://INFRA-1253 deletes.
const commentOwnedArm = `  union all
  select 'comment', c.id::text, 'owned_by', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = c.artifact_id
  where c.node_kind is null
`

// pointerArms are dispatch://INFRA-1152's arms over a comment's three pointers and a message's
// reply.
const pointerArms = `  union all
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
`

// askFollowerArm is dispatch://INFRA-1150's arm over an ask's followed_by, over the rows that
// name no node, which the boot step deletes (GraphEdgesAsksRetired).
const askFollowerArm = `  union all
  select 'ask', ask_id::text, 'followed_by', 'session', session_id, since, null
  from ask_followers
  where node_kind is null
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

// oneTupleArmNodeColumns are the columns of GraphEdgesOneTupleArm's tuple arm that name the node
// a tuple comes from, which the arm joins graph_nodes on (oneTupleArmNodes): its id, as
// artifactRefNodeColumn names it, and its kind, since a finding of a lens is a block node the arm
// names a lens_block (oneTupleArmTuples).
const oneTupleArmNodeColumns = `           tuple.node_id,
           tuple.node_kind,
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

// graphEdgesStoreClasses are the tuple arm's classes from a block's membership to an issue's
// duplicate_of and filed_from, the classes every set states before a set's own.
const graphEdgesStoreClasses = `             when tuple.from_kind = 'block' and tuple.kind = 'in_document' then 'block'
             when not (tuple.from_kind = 'block' and tuple.from_id like 'lens/%')
               and (tuple.kind in ('changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by')
                    or (tuple.kind in ('part_of', 'authored_by')
                        and tuple.from_kind in ('interaction', 'lens', 'risk', 'constraint'))) then 'store'
             when tuple.from_kind = 'issue' and tuple.kind in ('duplicate_of', 'filed_from') then 'store'
`

// issueClasses are the tuple arm's classes of an issue's child_of and its wait on an issue, every
// set states, whose probes (issueProbes) read dispatch://INFRA-1253's issues and issue_links rows.
const issueClasses = `             when tuple.from_kind = 'issue' and tuple.kind = 'child_of' and tuple.to_kind = 'issue' then 'issue_parent'
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

// attachedClass is GraphEdgesOneTupleArm's class of a document's attached_to its issue, the part_of
// its node states, whose projection row is the document's artifacts row, the relation the arm reads
// it through (oneTupleArmTuples, attachedProbe).
const attachedClass = `             when tuple.from_kind = 'artifact' and tuple.kind = 'attached_to' then 'artifact_attached'
`

// attachedProbe is the class's test of its projection row, which the relation the arm reads a
// document's attached_to from has made already: it reads each part_of only beside the artifacts row
// that names the node and its issue (artifactRowTuples), so every one that reaches the test stands.
const attachedProbe = `          when 'artifact_attached' then true
`

// lensClass is GraphEdgesOneTupleArm's class of the relations out of a finding of a lens, which
// the arm reads as a lens_block (oneTupleArmTuples): the kinds the arm over them read until that
// set, which consult no projection row. It is the first class, as the arm read them apart from
// every other class.
const lensClass = `             when tuple.from_kind = 'lens_block' and tuple.kind in ('part_of', 'authored_by', 'changed_by_run', 'has_kind', 'triggered_by', 'constrained_by', 'prevented_by') then 'store'
`

// graphEdgesOwnClasses are the tuple arm's classes of a block's link and of a comment's anchored_to
// and owned_by, which every set states after a set's own.
const graphEdgesOwnClasses = `             when tuple.from_kind = 'block' and tuple.kind <> 'part_of' then 'block_ref'
             when tuple.from_kind = 'comment' and tuple.kind in ('anchored_to', 'owned_by') then 'comment'
`

// askClasses are the tuple arm's classes of an ask's anchored_to, owned_by and followed_by, whose
// probes (askProbes) read dispatch://INFRA-1150's asks and ask_followers rows.
const askClasses = `             when tuple.from_kind = 'ask' and tuple.kind in ('anchored_to', 'owned_by') then 'ask'
             when tuple.from_kind = 'ask' and tuple.kind = 'followed_by' then 'ask_follower'
`

// graphEdgesShapedClass is the tuple arm's class of an answered ask's shaped, its last.
const graphEdgesShapedClass = `             when tuple.from_kind = 'ask' and tuple.kind = 'shaped' then 'block_ref'
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
// the refs row of the pair. The artifacts row is joined only where marked holds of it, the test
// that it names its node (artifactRefMarked), so a document whose row names none is the arm over
// refs' alone (ReferenceNotStated); once the boot step has deleted that half of the arm over refs
// (GraphEdgesIssueTreeRetired) the test is gone with it, and marked is empty. The refs row is
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
func artifactRefTuples(marked string) string {
	return `    from (
      select tuple.from_kind, tuple.from_id, tuple.from_id as node_id, tuple.kind, tuple.to_kind, tuple.to_id,
             tuple.at, tuple.legacy_source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      where tuple.from_kind <> 'artifact' or tuple.kind <> 'mentions'
      union all
      select 'artifact', mentioning.id::text, tuple.from_id, tuple.kind, tuple.to_kind, tuple.to_id,
             projected.created_at, projected.source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      join artifacts mentioning on mentioning.ref_key = tuple.from_id` + marked + `
      cross join lateral (
        select stated.created_at, stated.source_seq from refs stated
        where stated.from_kind = tuple.from_kind and stated.from_id = mentioning.id::text
          and stated.to_kind = tuple.to_kind and stated.to_id = tuple.to_id
        offset 0) projected
      where tuple.from_kind = 'artifact' and tuple.kind = 'mentions'
    ) tuple
`
}

// artifactRefMarked is artifactRefTuples' test that the artifacts row a document's mention is read
// through names its node, dispatch://INFRA-1253's marker of a document.
const artifactRefMarked = ` and mentioning.node_kind is not null`

// oneTupleArmTuples is the relation GraphEdgesOneTupleArm's tuple arm reads its tuples from: the
// log in parts, each naming its source and the node it comes from (node_id, node_kind). The first
// is every tuple but a document's mentions and its part_of and a finding of a lens's, each of its
// own node; then artifact, the parts of a document's mentions and its attached_to, the part_of its
// node states, renamed as the view has always answered it; and last a finding of a lens's
// relations, named lens_block, out of the finding's block node, as the arm over them named them
// until this set (lensClass). Each part names its from_kind as a constant where it has one, so a
// read that names the kinds of the nodes it reads out of (graphcensus' graphEdgesOut) cuts a
// part from its plan where its kind is none of them, and a read of one source or one far end is
// pushed into each part and binds an index there.
func oneTupleArmTuples(artifact string) string {
	return `    from (
      select tuple.from_kind, tuple.from_id, tuple.from_id as node_id, tuple.from_kind as node_kind, tuple.kind, tuple.to_kind,
             tuple.to_id, tuple.at, tuple.legacy_source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      where (tuple.from_kind <> 'artifact' or tuple.kind not in ('mentions', 'part_of'))
        and not (tuple.from_kind = 'block' and tuple.from_id like 'lens/%')` + artifact + `
      union all
      select 'lens_block', tuple.from_id, tuple.from_id, 'block', tuple.kind, tuple.to_kind, tuple.to_id,
             tuple.at, tuple.legacy_source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      where tuple.from_kind = 'block' and tuple.from_id like 'lens/%'
    ) tuple
`
}

// artifactRowTuples are oneTupleArmTuples' parts of a document's mentions and its attached_to while
// dispatch://INFRA-1253's arms stand, each read through the artifacts row whose node it comes
// from, while that row names its node, as artifactRefTuples reads a mention: a mention beside the
// refs row of the pair, dated and sequenced by that row, and an attached_to only while the row
// names the issue the part_of reaches, dated at the row's created_at with no sequence, as the arm
// over the artifacts that carry their node answered it (attachedArtifactArm). Each names the
// document by the row's uuid, so a read of a document's edges binds artifacts_id_text.
const artifactRowTuples = `
      union all
      select 'artifact', mentioning.id::text, tuple.from_id, 'artifact', tuple.kind, tuple.to_kind, tuple.to_id,
             projected.created_at, projected.source_seq, tuple.asserted, tuple.seq
      from graph_tuples tuple
      join artifacts mentioning on mentioning.ref_key = tuple.from_id and mentioning.node_kind is not null
      cross join lateral (
        select stated.created_at, stated.source_seq from refs stated
        where stated.from_kind = tuple.from_kind and stated.from_id = mentioning.id::text
          and stated.to_kind = tuple.to_kind and stated.to_id = tuple.to_id
        offset 0) projected
      where tuple.from_kind = 'artifact' and tuple.kind = 'mentions'
      union all
      select 'artifact', attached.id::text, tuple.from_id, 'artifact', 'attached_to', tuple.to_kind, tuple.to_id,
             attached.created_at, null, tuple.asserted, tuple.seq
      from graph_tuples tuple
      join artifacts attached on attached.ref_key = tuple.from_id and attached.node_kind is not null
                              and attached.issue_key = tuple.to_id
      where tuple.from_kind = 'artifact' and tuple.kind = 'part_of' and tuple.to_kind = 'issue'`

// artifactStoreTuples are oneTupleArmTuples' parts of a document's mentions and its attached_to
// once the boot step has deleted dispatch://INFRA-1253's arms: read off the document's node
// alone, which names the document by its row's uuid and holds that row's created_at
// (graphstore.ArtifactRow), a mention dated by its own tuple, whose writers date it as the refs row
// of the pair (graphstore.QueueMentionsDated), and an attached_to at the row's created_at with no
// sequence, as the arm over the artifacts row answered it. A read of a document's edges by its
// uuid binds graph_nodes_artifact_id (migration 20261123185607), then graph_tuples_current.
const artifactStoreTuples = `
      union all
      select 'artifact', ` + ArtifactNodeUUID + `, tuple.from_id, 'artifact', tuple.kind, tuple.to_kind, tuple.to_id,
             tuple.at, tuple.legacy_source_seq, tuple.asserted, tuple.seq
      from graph_nodes holder
      join graph_tuples tuple on tuple.from_kind = holder.kind and tuple.from_id = holder.id and tuple.kind = 'mentions'
      where holder.kind = 'artifact' and ` + ArtifactNodeUUID + ` is not null
      union all
      select 'artifact', ` + ArtifactNodeUUID + `, tuple.from_id, 'artifact', 'attached_to', tuple.to_kind, tuple.to_id,
             ` + ArtifactNodeCreatedAt + `, null, tuple.asserted, tuple.seq
      from graph_nodes holder
      join graph_tuples tuple on tuple.from_kind = holder.kind and tuple.from_id = holder.id and tuple.kind = 'part_of'
                             and tuple.to_kind = 'issue'
      where holder.kind = 'artifact' and ` + ArtifactNodeUUID + ` is not null`

// ArtifactNodeUUID is the SQL expression artifactStoreTuples names a document by, over the
// document's node aliased holder: the row's uuid its node holds (graphstore.ArtifactRow), held to
// graphstore.AttributeArtifactID by graphstore's TestTheViewReadsTheRowADocumentsNodeNames, since
// graphstore imports this package. Migration 20261123185607 indexes the same expression.
const ArtifactNodeUUID = `holder.attributes->>'artifact_id'`

// ArtifactNodeCreatedAt is the SQL expression artifactStoreTuples dates a document's attached_to
// by, over the document's node aliased holder: the row's created_at its node holds, held to
// graphstore.AttributeCreatedAt as ArtifactNodeUUID is.
const ArtifactNodeCreatedAt = `(holder.attributes->>'created_at')::timestamptz`

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

// oneTupleArmNodes is GraphEdgesOneTupleArm's join to the node each tuple comes from, by the
// node's own kind and id (oneTupleArmNodeColumns).
const oneTupleArmNodes = `  join graph_nodes node on node.kind = edge.node_kind and node.id = edge.node_id
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

// graphEdgesProbes is the tuple arm from its tests of an edge, after their head, to the probe of
// a block's membership of its document, the probes every set states before an issue's.
const graphEdgesProbes = `    and (edge.projects <> 'block_ref' or edge.stated = 'mentions'
         or block_ref_listed(block_pointer_targets(edge.from_id, edge.stated), edge.to_id))
    and case edge.projects
          when 'store' then true
          when 'block' then exists (
            select 1 from document_blocks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
`

// issueProbes are the probes of issueClasses: an issue's child_of is answered while the issues row
// carries the issue's node and names that parent, its wait while the issue_links row of the pair
// carries it.
const issueProbes = `          when 'issue_parent' then exists (
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

// blockRefProbes are the probes of a block's link and of a comment's anchored_to and owned_by,
// which every set states after a set's own.
const blockRefProbes = `          when 'block_ref' then exists (
            select 1 from block_refs projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.kind = edge.stated and projected.to_kind = edge.to_kind
              and projected.to_id = edge.to_id
            offset 0)
          when 'comment' then exists (
            select 1 from comments projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
`

// askProbes are the probes of askClasses: an ask's anchored_to and owned_by are answered while its
// asks row carries its node, and its followed_by while the follower row of that session does.
const askProbes = `          when 'ask' then exists (
            select 1 from asks projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
            offset 0)
          when 'ask_follower' then exists (
            select 1 from ask_followers projected
            where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
              and projected.session_id = edge.to_id
            offset 0)
`

// graphEdgesTail is the view after the tuple arm's probes in every set before
// GraphEdgesOneTupleArm: the end of its case, and the arm over the relations out of a finding of a
// lens.
const graphEdgesTail = `        end
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

// graphEdgesEnd is the view after the tuple arm's probes from GraphEdgesOneTupleArm on, which
// reads a finding of a lens's relations in the tuple arm (oneTupleArmTuples): the end of its case.
const graphEdgesEnd = `        end;
`
