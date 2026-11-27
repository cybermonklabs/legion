-- 20261126050006 only re-states graph_edges, a catalogue write: it reads no stored row, and
-- refuses and rewrites none. The tuple arm's filter keeps or drops whole groups of the
-- `distinct on` key, so every edge the view answered before it is answered as it was.
select 0;
