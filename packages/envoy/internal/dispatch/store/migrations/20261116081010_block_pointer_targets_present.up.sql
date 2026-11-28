-- 20261116081010_block_pointer_targets_present.up.sql
--
-- A node is present at every state but `gone`. The node `state` takes `proposed`,
-- `settled` and `overtaken` beside `live` and `gone` now, and no read of the store takes
-- `state = 'live'` to mean present (spec requirement r-census-states).
-- `block_pointer_targets` (20261109215947) read a pointer's attribute node only while its state
-- was `live`; this replaces it with the same function, reading that node while its state is not
-- `gone`, and nothing else. It still reads the schema as `pmdoc/schemanodes.Read` installs it,
-- which takes the same test.
--
-- An attribute node is only ever `live` or `gone` today: the type migrations write it `live`, and
-- no route moves it to another state. So the function answers every row exactly as it did, and a
-- build of the release before, whose `graph_edges` and `refs.BacklinkCounts` call it, reads the
-- same rows over the new body.
--
-- `create or replace function` with the same signature keeps every dependent of the function -
-- the tuple arm of `graph_edges` - as it is, and locks the function's own catalogue row and no
-- table.
create or replace function block_pointer_targets(from_node text, attribute_name text) returns text[] language sql stable as $$
  select case when attribute.attributes ? 'targets'
              then array(select jsonb_array_elements_text(attribute.attributes->'targets')) end
  from document_blocks source
  join graph_tuples held on held.kind = 'attribute_of' and held.to_kind = 'supertag'
                        and held.to_id = source.type and held.from_kind = 'attribute' and held.asserted
  join graph_nodes attribute on attribute.kind = held.from_kind and attribute.id = held.from_id
                            and attribute.state <> 'gone'
  where source.node_id = $1 and attribute.attributes->>'name' = $2
    and not exists (select 1 from graph_tuples later
                    where later.from_kind = held.from_kind and later.from_id = held.from_id
                      and later.kind = held.kind and later.to_kind = held.to_kind
                      and later.to_id = held.to_id and later.seq > held.seq)
  order by held.seq desc
  limit 1
$$;
