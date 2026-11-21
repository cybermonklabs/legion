-- pgmigrate:non-transactional
-- 20261116094058 gave ask_followers a foreign key on (node_kind, node_address) and no index over
-- those columns, so every delete of a graph_nodes row would read ask_followers once for each node
-- deleted, with nothing to read it by, and graph_edges' tuple arm (20261116094101) probes
-- ask_followers for the row that names an ask's node and the session a followed_by edge reaches,
-- once for each such edge it answers. This is the index both read: an ask's follower rows under
-- its node, of which an ask holds a handful, then the session among them.
--
-- A file of its own, as 20261116094059 is, because pgmigrate admits exactly one concurrent index
-- build in a non-transactional migration. It is a performance index: no constraint depends on it.
-- A failed concurrent build leaves an INVALID index of this name, which the runner drops with DROP
-- INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists ask_followers_node_address
    on ask_followers (node_kind, node_address);
