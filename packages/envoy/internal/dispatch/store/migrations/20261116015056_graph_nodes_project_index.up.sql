-- pgmigrate:non-transactional
-- 20261116015056_graph_nodes_project_index.up.sql — the index a read of one project's nodes takes.
--
-- 20261116015042 gave graph_nodes a project_key column, the projection of every node's `project`
-- base attribute. A read that asks which nodes of a kind a project holds, and the census that
-- counts them per kind (graphcensus.ProjectComplete), bind (project_key, kind); without this index
-- each is a scan of graph_nodes, which holds a node for every block of every document.
--
-- It is partial on `project_key is not null`: a person, a session, a role, a page, a supertag, an
-- attribute and a conversation's nodes carry no project, and no read asks for them by one.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name, which the runner drops with DROP INDEX
-- CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists graph_nodes_project
    on graph_nodes (project_key, kind) where project_key is not null;
