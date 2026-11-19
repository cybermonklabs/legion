-- 20261118012235_document_client_ids.up.sql
-- The Yjs client ids of a live document and the person each belongs to. A browser's update names
-- the client its edits are under, and before this table nothing tied a client id to anyone: a
-- signed-in person could write under another's id at the clock that person typed next, and the
-- other's own keystroke was then dropped as a duplicate (dispatch://INFRA-179).
--
-- A row is written in two ways. A document socket's admission gives the page a client id of its
-- own and writes it here first (minted): Dispatch draws it at 2^32 or above, where no browser of
-- an earlier build and no write of the server's own draws one, and the page writes under no other
-- id. A client id below 2^32 that no admission gave out - one a page of an earlier build, or a
-- client that presents no client id at all, drew for itself - is written here (claimed) by the
-- person whose page of this build presents it at the page's admission, or whose browser lands an
-- edit under it first: an edit's claim is given back unless the edit stands. Every claim is
-- counted, from this table's rows, among the person's claims in the document in the UTC day. A
-- socket that presents no client id may claim an id the stored history does not name at any
-- time, and is held to at most 64 such claims a socket and 64 claims a person in one document in
-- one UTC day. An id the stored history names may be claimed only within 30 days of
-- document_client_ids_since, and only by a person a version or a pending author row lists as
-- having edited the document since the history first named it; and a page of this build claims
-- any id only within those 30 days, at its admission held to the same 64 claims a day. So a tab
-- open across the deploy, the edits it saved, and the clients that present no id still land.
-- From then on a browser's update carrying an edit under a client id whose row names another
-- person is refused whole, and one carrying an edit under an id at 2^32 or above that has no row
-- is refused too (packages/envoy/internal/dispatch/docs/client_ids.go).
--
-- A claim can be made first by someone other than the tab whose id it is - a co-author, reading
-- the id off the history, or any reader, reading an id no history names off the room's awareness
-- - writing at the clock the tab writes next, and the tab's own edit at that clock then arrives
-- as a duplicate of a clock the room holds, which would be dropped with no word. So an update
-- carrying an edit under a claimed id below 2^32 at a clock from claimed_from on that the room
-- already holds is compared with what the room holds there, and refused where its content
-- differs, where it carries such clocks under at most 4 claimed ids; one carrying them under more,
-- a whole-state handoff of the history, is not compared, and is refused whole from a socket that
-- presents no client id. claimed_from is the id's clock in the document when the claim was made,
-- so the history before the claim, which no claim could have written, is never compared.
--
-- The person a row names never changes, but the row does: presented_at is moved to now each time
-- a socket's admission gives the id back to the page that presented it, and landed is set once an
-- edit under the id has landed in the document. A row whose id never landed an edit and that no
-- admission has presented for 30 days is deleted by the hourly compaction sweep, since every page
-- load is given an id and most never type: a page that comes back after that is given a fresh
-- id, which loses nothing, as its document holds nothing under the old one. A claimed row is
-- written landed and never swept: its id could otherwise be claimed by another person while the
-- tab that holds it is still open and writing under it. The claims are bounded instead: a page
-- of this build's by the 30 days, those no window ends by the 64 a socket, and every admission's
-- claim and every claim no window ends by the 64 a person makes in a document a day.
create table document_client_ids (
  artifact_id uuid not null references artifacts (id) on delete cascade,
  client_id bigint not null check (client_id >= 0),
  actor_kind text not null,
  actor_id text not null,
  -- true: an admission gave this id out; false: an id no admission gave out, claimed by a page of
  -- this build presenting it at its admission or by the first person to land an edit under it.
  minted boolean not null,
  -- whether an edit under this id has landed in the document: set by the first edit under an id
  -- an admission gave out, and on every claim as it is written, which is never swept.
  landed boolean not null default false,
  -- for a claimed id, the clock the document held the id to when the claim was made; null for a
  -- minted id.
  claimed_from bigint check (claimed_from >= 0),
  created_at timestamptz not null default now(),
  presented_at timestamptz not null default now(),
  primary key (artifact_id, client_id)
);

-- The sweep's read: the ids that never landed an edit, oldest presentation first.
create index document_client_ids_unlanded on document_client_ids (presented_at) where not landed;

-- The count of a person's claims in a document since the start of the UTC day, which a page's
-- admission reads before it claims the id it presents, and a socket that presents no client id
-- before it claims an id no stored history names.
create index document_client_ids_claims on document_client_ids (artifact_id, actor_kind, actor_id, created_at)
  where not minted;

-- When this database first served a build that gives client ids out: the first process of that
-- build to boot writes the one row (docs.Service.RecordClientIDsSince) and no later boot moves it.
-- An id the stored history names may be claimed for 30 days from it, and not after.
create table document_client_ids_since (
  singleton boolean primary key default true check (singleton),
  since timestamptz not null
);
