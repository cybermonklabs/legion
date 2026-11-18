-- 20261117042519_block_moved_to_lens.up.sql — where a document's address moved to: the lens the
-- pre-mortem conversion retires the document into.
--
-- block_moved_to (migration 20261117002006) follows the moved_to tuples from a block address no
-- document_blocks row holds to the block they reach, for the resolvers that read a move
-- (dispatch://INFRA-1150 R8). The pre-mortem conversion (api.ConvertPremortem,
-- dispatch://INFRA-1950/spec#b-r-premortem-nodes) retires a document and appends one moved_to from
-- the document's own address - kind artifact, its ref_key - to the lens of kind pre-mortem it
-- wrote in the document's place - kind lens, the lens's uuid - so a citation of the retired
-- document reaches that lens rather than dangling. This migration widens the one reading of a move
-- to that source, so GET /api/v1/references?to= (graphstore.MovedTo) and refs.Closure (inside its
-- own statement) read a document's move through the same function as a block's.
--
-- A document's address is its ref_key, which never holds a '#', and a block's always holds one
-- (`<ref_key>#<block id>`), so the function tells the two apart by the address alone. From a
-- document's address it answers the id of the lens the newest moved_to that address states now
-- points at - the newest tuple of each far end kept while it asserts, as each step of a block's
-- chain is read - where that lens stands, which is a graph_nodes row of kind lens that is not
-- gone, and null where the address states no such moved_to or its lens is gone. A lens moves
-- nowhere, so the chain from a document's address is that one tuple. From a block's address it
-- answers what migration 20261117002006 answers, unchanged.
--
-- Rolling deploy. create or replace keeps the function's name, its argument and its result type.
-- The release before this one calls block_moved_to with block addresses alone, which the body
-- answers as before, and writes no moved_to from a document's address. Rolling back past this
-- migration is an image rollback alone: the release before calls the widened function with block
-- addresses only.
create or replace function block_moved_to(address text) returns text
  language plpgsql stable strict
  as $$
declare
  reached text := address;
  passed text[] := array[address];
begin
  if strpos(address, '#') = 0 then
    select moved.to_id into reached
    from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
      from graph_tuples t
      where t.from_kind = 'artifact' and t.from_id = address and t.kind = 'moved_to'
      order by t.to_kind, t.to_id, t.at desc, t.seq desc
    ) moved
    where moved.asserted and moved.to_kind = 'lens'
    order by moved.at desc, moved.seq desc
    limit 1;
    if found and exists (select 1 from graph_nodes n where n.kind = 'lens' and n.id = reached and n.state <> 'gone') then
      return reached;
    end if;
    return null;
  end if;
  for hop in 0 .. 8 loop
    if exists (select 1 from document_blocks d where d.node_id = reached) then
      return reached;
    end if;
    exit when hop = 8;
    select moved.to_id into reached
    from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
      from graph_tuples t
      where t.from_kind = 'block' and t.from_id = reached and t.kind = 'moved_to'
      order by t.to_kind, t.to_id, t.at desc, t.seq desc
    ) moved
    where moved.asserted and moved.to_kind = 'block'
    order by moved.at desc, moved.seq desc
    limit 1;
    if not found or reached = any(passed) then
      return null;
    end if;
    passed := passed || reached;
  end loop;
  return null;
end
$$;
