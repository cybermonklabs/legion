-- 20261118165901_issue_tree_block_moved_to_room.up.sql — where a block address resolves to once
-- issues are born into their rooms.
--
-- A reference names `<document ref_key>#<block id>` (text.TargetID), and a block of an issue room
-- is the node `<tree ref_key>#<block id>`: its document_blocks row is keyed by the issue's tree
-- artifact (20261117045607_issue_tree_rooms). moved_to_reach (migration 20261117002006) and
-- block_moved_to (20261117002006 and 20261117042519) are the one reading every resolver of an
-- address takes, and they found no block at the spec's own address of a block of an issue born
-- converted, so a check of such an issue could serve none of its spec's requirements. This
-- migration resolves a block's address in one order (T17 of this branch's plan), in two steps that
-- are functions of their own:
--
-- block_address_walk(address) follows the address's current moved_to tuple - the newest of each
-- far end kept while it asserts, as before, of the far ends that are a block or an ask - and so on
-- from each block far end while another current moved_to stands there, and answers the address it
-- reaches, the address itself where no tuple stands; it stops at an address whose newest current
-- moved_to ends at an ask, and answers that address. A far end already visited ends the walk as a
-- cycle, and a ninth tuple as a chain past graphstore.MovedToHops, each answering null.
--
-- block_room_address(address) is the same block id in the room the document_rooms row of the
-- address's document names, `<tree ref_key>#<block id>`, and null where its document has no room.
--
-- moved_to_reach(address) of a block's address is ('block', the room address of the reached
-- address) where a document_blocks row holds it, else ('block', the reached address) where one
-- holds that, else ('ask', the ask's id) where the reached address's newest current moved_to ends
-- at an ask whose asks row stands - the heading of a decisions document's section that cites an
-- answered ask a person removed, as 20261117002006 reads it - and nulls otherwise. So a re-minted
-- block's old address reaches its fresh id through its moved_to and never the block of another
-- document that keeps the id in the room, and a block that stands is reached before any ask. From
-- a document's address, which holds no '#', it answers ('lens', the lens's id) as 20261117042519
-- reads the lens: the newest current moved_to from the address to a lens, where that lens is not
-- gone. graphcensus' dangling read calls it alone, as graphstore.MovedTo does.
--
-- block_moved_to(address) is what moved_to_reach reaches but an ask: the block, or from a
-- document's address the lens, null where it reaches neither, so the two readings are one
-- definition. The block index's writer takes the walk and the room step without asking for a row
-- where a pointer resolves to no block (refs' resolvePointerPairs), so a pointer whose block was
-- deleted keeps the far end it named.
--
-- Rolling deploy. Two new functions, and create or replace keeps moved_to_reach's and
-- block_moved_to's names, arguments and result types. The release before this one names neither
-- new function, and it calls block_moved_to and moved_to_reach after its own probe found no row at
-- the address, or alone, and the bodies answer each of those calls as before: a document_rooms row
-- is written only for an issue created while every live process lists issue_room, which no process
-- of that release does, no writer of either release states a moved_to from an address a
-- document_blocks row holds, and no stored moved_to runs from a document's address until a
-- conversion of this release writes one. Rolling back past this migration is an image rollback
-- alone: the release before calls the restated functions as it called the ones they replace, and
-- calls neither of the other two.
create function block_address_walk(address text) returns text
  language plpgsql stable strict
  as $$
declare
  reached text := address;
  passed text[] := array[address];
  far_kind text;
  far_end text;
begin
  for hop in 1 .. 9 loop
    select moved.to_kind, moved.to_id into far_kind, far_end
    from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
      from graph_tuples t
      where t.from_kind = 'block' and t.from_id = reached and t.kind = 'moved_to'
      order by t.to_kind, t.to_id, t.at desc, t.seq desc
    ) moved
    where moved.asserted and moved.to_kind in ('block', 'ask')
    order by moved.at desc, moved.seq desc
    limit 1;
    exit when not found;
    if hop = 9 then
      return null;
    end if;
    exit when far_kind = 'ask';
    if far_end = any(passed) then
      return null;
    end if;
    passed := passed || far_end;
    reached := far_end;
  end loop;
  return reached;
end
$$;

create function block_room_address(address text) returns text
  language sql stable strict
  as $$
  select tree.ref_key || substr(address, strpos(address, '#'))
  from artifacts document
  join document_rooms room on room.artifact_id = document.id
  join artifacts tree on tree.id = room.room
  where document.ref_key = split_part(address, '#', 1)
$$;

create or replace function moved_to_reach(address text, out kind text, out id text)
  language plpgsql stable strict
  as $$
declare
  reached text;
  roomed text;
  far_kind text;
  far_end text;
begin
  if strpos(address, '#') = 0 then
    select moved.to_id into far_end
    from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
      from graph_tuples t
      where t.from_kind = 'artifact' and t.from_id = address and t.kind = 'moved_to'
      order by t.to_kind, t.to_id, t.at desc, t.seq desc
    ) moved
    where moved.asserted and moved.to_kind = 'lens'
    order by moved.at desc, moved.seq desc
    limit 1;
    if found and exists (select 1 from graph_nodes n where n.kind = 'lens' and n.id = far_end and n.state <> 'gone') then
      kind := 'lens';
      id := far_end;
    end if;
    return;
  end if;
  reached := block_address_walk(address);
  if reached is null then
    return;
  end if;
  roomed := block_room_address(reached);
  if roomed is not null and exists (select 1 from document_blocks d where d.node_id = roomed) then
    kind := 'block';
    id := roomed;
    return;
  end if;
  if exists (select 1 from document_blocks d where d.node_id = reached) then
    kind := 'block';
    id := reached;
    return;
  end if;
  select moved.to_kind, moved.to_id into far_kind, far_end
  from (
    select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
    from graph_tuples t
    where t.from_kind = 'block' and t.from_id = reached and t.kind = 'moved_to'
    order by t.to_kind, t.to_id, t.at desc, t.seq desc
  ) moved
  where moved.asserted and moved.to_kind in ('block', 'ask')
  order by moved.at desc, moved.seq desc
  limit 1;
  if not found or far_kind <> 'ask' then
    return;
  end if;
  -- An ask node's id is its row's uuid; anything else names no ask, and is read as none rather
  -- than cast.
  if far_end ~ '^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$' then
    if exists (select 1 from asks k where k.id = far_end::uuid) then
      kind := 'ask';
      id := far_end;
    end if;
  end if;
end
$$;

create or replace function block_moved_to(address text) returns text
  language sql stable strict
  as $$
    select reach.id from moved_to_reach(address) reach where reach.kind <> 'ask'
  $$;
