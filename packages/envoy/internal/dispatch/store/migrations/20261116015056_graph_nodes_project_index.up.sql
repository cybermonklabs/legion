-- pgmigrate:non-transactional
-- 20261116015056_graph_nodes_project_index.up.sql — the index a read of one project's nodes takes.
--
-- 20261116015042 gave graph_nodes a project_key column, the projection of every node's `project`
-- base attribute. The read it serves comes with private projects: the read of the nodes a reader
-- may see, which keeps a node only where its project_key is one of the projects the reader sees,
-- and the reads that list the nodes of a kind one project holds. Each binds (project_key, kind);
-- without this index each is a scan of graph_nodes, which holds a node for every block of every
-- document. Nothing in the change that adds the column reads it yet: the project census joins
-- graph_nodes on (kind, id) and binds no project. It is built with the column so that the read
-- finds it in place, rather than a later release building it under live traffic.
--
-- It is partial on `project_key is not null`: a person, a session, a role, a page, a supertag, an
-- attribute and a conversation's nodes carry no project, and no read asks for them by one.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name, which the runner drops with DROP INDEX
-- CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists graph_nodes_project
    on graph_nodes (project_key, kind) where project_key is not null;
