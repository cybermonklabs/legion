package graphcensus

import "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// runProjections are the census's kinds of the runs of the command bar (projections). A run is an
// interaction node, which going_requests projects, and each of its frames a frame value of that
// node, which a going_events row projects (dispatch://INFRA-1950/spec#b-r-interaction): the
// interaction kind holds a row to its node and its authored_by, and the frame kind every frame a
// run's rows hold to every frame value its node holds. A run whose row a process of the release
// before deleted is a node the backfill states gone (backfillGoneRuns), which no row may project
// and which presentNodes reads past, so it is no difference either way. A deployment can hold no
// run, so neither kind refuses on zero rows (projection.mayBeEmpty).
var runProjections = []projection{{
	name:     graphstore.KindInteraction,
	nodeKind: graphstore.KindInteraction,
	table:    "going_requests",
	note: "one node per row; the digest is the row's author, project, words, created_at and ends_at and the author" +
		" graphstore.AuthorOf reads off it, against the node's made_by, project, words, made_at and ends_at and the" +
		" authored_by tuple it states now; a run whose row is gone is a gone node, no difference",
	rows: `
		select 'interaction ' || r.id::text, '` + graphstore.KindInteraction + `', r.id::text,
		       md5(r.author::text || '|' || r.project || '|' || r.words || '|' || r.created_at::text
		           || '|' || r.ends_at::text || '|' || ` + statedLinks(authoredLink("r.author")) + `)
		from going_requests r`,
	nodes: `
		select n.id, md5(n.made_by::text || '|' || coalesce(n.project_key, '')
		                 || '|' || coalesce(n.attributes->>'` + graphstore.AttributeWords + `', '')
		                 || '|' || n.made_at::text
		                 || '|' || coalesce((n.attributes->>'` + graphstore.AttributeEndsAt + `')::timestamptz::text, '')
		                 || '|' || ` + currentLinks(graphstore.KindInteraction, kindIn("t.kind", runLinkKinds)) + `)
		from graph_nodes n where n.kind = '` + graphstore.KindInteraction + `'`,
	mayBeEmpty: true,
}, {
	name:     graphstore.AttributeFrame,
	nodeKind: graphstore.KindInteraction,
	table:    "going_events",
	note: "a row is one frame of a run and its node is the run's interaction, so several rows share one node: the" +
		" node side is every interaction not gone that holds a frame value, and the digest is every frame the" +
		" rows of that run hold, each its sequence, name and data as graphstore.FrameValueSQL writes it, against" +
		" every frame value its node holds",
	rows: `
		select '` + graphstore.AttributeFrame + ` ' || e.request_id::text || ' ' || e.sequence, '` + graphstore.KindInteraction + `',
		       e.request_id::text, run.digest
		from going_events e
		join (
			select f.request_id,
			       md5(string_agg(` + graphstore.FrameValueSQL("f.sequence", "f.name", "f.data") + `, E'\n' order by f.sequence)) as digest
			from going_events f
			group by f.request_id
		) run on run.request_id = e.request_id`,
	// A frame value is never retracted, and two writes of one frame state the same text, which
	// the distinct reads once; two values at one sequence are two frames, and the digest says so.
	nodes: `
		select held.id, md5(held.frames)
		from (
			select n.id, (
				select string_agg(framed.value, E'\n' order by framed.sequence)
				from (
					select distinct t.to_id as value, (t.to_id::jsonb->>'sequence')::integer as sequence
					from graph_tuples t
					where t.from_kind = '` + graphstore.KindInteraction + `' and t.from_id = n.id
					  and t.kind = '` + graphstore.AttributeFrame + `' and t.to_kind = '` + graphstore.KindValue + `'
					  and t.asserted
				) framed
			) as frames
			from graph_nodes n where n.kind = '` + graphstore.KindInteraction + `'
		) held
		where held.frames is not null`,
	mayBeEmpty: true,
}}

// runBackfills are the backfill's passes over what a process of the release before writes of a
// run of the command bar during a roll or after a rollback (backfillPasses): a run it recorded with
// no interaction node, a frame it appended with no frame value, and a run whose row it deleted an
// hour old, whose node it states gone (backfillRuns, backfillRunFrames, backfillGoneRuns).
var runBackfills = []backfillPass{oneKind(backfillRuns), oneKind(backfillRunFrames), oneKind(backfillGoneRuns)}
