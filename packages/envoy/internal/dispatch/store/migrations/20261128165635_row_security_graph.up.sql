-- 20261128165635_row_security_graph.up.sql — row security on the graph, references, workflow steps and priorities (classes G, GT, R, WF and PR of plan D4, INFRA-6168).
--
-- Class G: a graph node passes dispatch_node_visible over its own row, the rule of its kind
-- (the owner-run function names each kind's rule). Class GT: a tuple through each node it joins,
-- each judged by dispatch_tuple_end_visible, which reads that node as the owner; an attribute's
-- value (to_kind value) is no node. Neither policy reads the other's table under row security.
-- Class R: a reference through its from item and its to item, each read by its kind from its own
-- table (an issue by key, a document by id on the from side and by ref_key on the to side, an ask,
-- a comment and a message by id, a document's block by node id), and a kind that holds no project
-- passes an open viewer. Class WF: a workflow node through dispatch_workflow_node_visible (the
-- issue it is part of, read as the owner, else its maker alone), and a workflow tuple through its
-- node. Class PR: a priority node passes an open viewer, since a priority belongs to no project,
-- and a priority tuple each of its ends: a priority an open viewer, an issue through issues, and
-- an attribute's value is no end.
--
-- Each table below gets row security, not forced, and one policy named viewer, for all commands
-- and to public, whose USING and WITH CHECK are the same three arms: the statement does not run as
-- the request role (dispatch_request_role), or the viewer is open while no project is private
-- (the R5 fast path), or the class's predicate. Only dispatch_app meets the predicate. The table's
-- owner bypasses row security, which is not forced, so the migrations, the system pool and the
-- release before this one, which run as the owner, read and write every row as before, and every
-- other role (the read login) passes the first arm. The first two arms and the viewer the access
-- clauses read ((select dispatch_viewer())) each read only the settings and are read once per
-- statement. A policy names no role, so it is made before dispatch_app exists (the last migration
-- of this release makes it). store.RowSecurityClasses names each table's class and reason, and
-- store.TestEveryTableIsClassedForRowSecurity holds them to the catalog.
--
-- ENABLE ROW LEVEL SECURITY and CREATE POLICY change the catalog alone: each takes ACCESS
-- EXCLUSIVE on its table for the moment it runs, bounded by the runner's lock_timeout and tried
-- again as every lock wait is, and reads no row, so the census admits the migration at any table
-- size (pgmigrate's catalog-only kinds). One class group per migration keeps the locks each holds
-- few and short against the release before this one's traffic. No row is read, rewritten or
-- refused, and the census answers 0. Rollback is an image rollback: the release before this one
-- runs as the owner and meets no policy.

alter table public.graph_nodes enable row level security;
create policy viewer on public.graph_nodes for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_node_visible(graph_nodes.kind, graph_nodes.id, graph_nodes.project_key, graph_nodes.parties))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_node_visible(graph_nodes.kind, graph_nodes.id, graph_nodes.project_key, graph_nodes.parties));

alter table public.graph_tuples enable row level security;
create policy viewer on public.graph_tuples for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (dispatch_tuple_end_visible(graph_tuples.from_kind, graph_tuples.from_id)
		and (graph_tuples.to_kind = 'value' or dispatch_tuple_end_visible(graph_tuples.to_kind, graph_tuples.to_id))))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (dispatch_tuple_end_visible(graph_tuples.from_kind, graph_tuples.from_id)
		and (graph_tuples.to_kind = 'value' or dispatch_tuple_end_visible(graph_tuples.to_kind, graph_tuples.to_id))));

alter table public.refs enable row level security;
create policy viewer on public.refs for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or ((case refs.from_kind when 'comment' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			when 'ask' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			when 'message' then exists (select 1 from public.messages seen_message where seen_message.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			when 'artifact' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			else (select dispatch_viewer_open()) end)
		and (case refs.to_kind when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = refs.to_id)
			when 'artifact' then exists (select 1 from public.artifacts seen_document where seen_document.ref_key = refs.to_id)
			when 'ask' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when refs.to_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.to_id)::uuid end))
			when 'comment' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when refs.to_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.to_id)::uuid end))
			when 'message' then exists (select 1 from public.messages seen_message where seen_message.id = (case when refs.to_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.to_id)::uuid end))
			when 'block' then exists (select 1 from public.document_blocks seen_block where seen_block.node_id = refs.to_id)
			else (select dispatch_viewer_open()) end)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or ((case refs.from_kind when 'comment' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			when 'ask' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			when 'message' then exists (select 1 from public.messages seen_message where seen_message.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			when 'artifact' then exists (select 1 from public.artifacts seen_document where seen_document.id = (case when refs.from_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.from_id)::uuid end))
			else (select dispatch_viewer_open()) end)
		and (case refs.to_kind when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = refs.to_id)
			when 'artifact' then exists (select 1 from public.artifacts seen_document where seen_document.ref_key = refs.to_id)
			when 'ask' then exists (select 1 from public.asks seen_ask where seen_ask.id = (case when refs.to_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.to_id)::uuid end))
			when 'comment' then exists (select 1 from public.comments seen_comment where seen_comment.id = (case when refs.to_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.to_id)::uuid end))
			when 'message' then exists (select 1 from public.messages seen_message where seen_message.id = (case when refs.to_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (refs.to_id)::uuid end))
			when 'block' then exists (select 1 from public.document_blocks seen_block where seen_block.node_id = refs.to_id)
			else (select dispatch_viewer_open()) end)));

alter table public.workflow_nodes enable row level security;
create policy viewer on public.workflow_nodes for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_workflow_node_visible(workflow_nodes.kind, workflow_nodes.id, workflow_nodes.attributes, workflow_nodes.made_by))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or dispatch_workflow_node_visible(workflow_nodes.kind, workflow_nodes.id, workflow_nodes.attributes, workflow_nodes.made_by));

alter table public.workflow_tuples enable row level security;
create policy viewer on public.workflow_tuples for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.workflow_nodes seen_node where seen_node.kind = workflow_tuples.from_kind and seen_node.id = workflow_tuples.from_id))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or exists (select 1 from public.workflow_nodes seen_node where seen_node.kind = workflow_tuples.from_kind and seen_node.id = workflow_tuples.from_id));

alter table public.priority_nodes enable row level security;
create policy viewer on public.priority_nodes for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (select dispatch_viewer_open()))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or (select dispatch_viewer_open()));

alter table public.priority_tuples enable row level security;
create policy viewer on public.priority_tuples for all to public
	using ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or ((case priority_tuples.from_kind when 'priority' then (select dispatch_viewer_open()) when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = priority_tuples.from_id) when 'value' then true else false end)
		and (case priority_tuples.to_kind when 'priority' then (select dispatch_viewer_open()) when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = priority_tuples.to_id) when 'value' then true else false end)))
	with check ((select not dispatch_request_role())
		or (select dispatch_viewer_open() and not dispatch_viewer_private())
		or ((case priority_tuples.from_kind when 'priority' then (select dispatch_viewer_open()) when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = priority_tuples.from_id) when 'value' then true else false end)
		and (case priority_tuples.to_kind when 'priority' then (select dispatch_viewer_open()) when 'issue' then exists (select 1 from public.issues seen_issue where seen_issue.key = priority_tuples.to_id) when 'value' then true else false end)));
