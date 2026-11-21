-- 20261121002206_intake_consumers.up.sql
--
-- The selection records of the delivery intakes' two durables, delivery-events and delivery-jobs,
-- under the fresh consumer naming mode (ENVOY_CONSUMER_NAMES=fresh; consumernames.PGStore). Each
-- durable keeps its logical name, and every consumer created for it has a physical name of its
-- own, <logical>~<generation>, created once and never again: nats-server can leave a deleted
-- consumer's late state write in a new consumer of the same name, which then skips messages after
-- a restart. One row per logical name holds which physical name the intake delivers from, the
-- operation in flight, the start saved before anything is deleted, the last ack floor read, and
-- the retired names a check deletes.
--
--   logical    the durable's logical name, or consumernames.PGSentinelKey ('envoy.sentinel') for
--              the store's own row, whose record is consumernames.StoreState: the cutover marker
--              the fresh mode refuses every consumer call until it reads 'done'. No logical name a
--              durable has holds a '.', so the sentinel's key is no durable's.
--   record     consumernames.Record (or StoreState) as JSON.
--   revision   the row's version, 1 at its insert and one more at each update: every write is
--              conditional on the revision it read, so two processes' writes never pass each other.
--
-- A row is never deleted. The fixed mode, the default, neither reads nor writes this table, and
-- no row is written by this migration: the cutover's convert writes the two durables' rows and the
-- sentinel, and until it has, the fresh mode refuses (a missing row or sentinel is a refusal,
-- never a first create).
--
-- A new table nothing before this release names: the release before this one neither reads nor
-- writes it, so the two roll side by side (cmd/dispatch/README.md, "Database migrations"), and the
-- census answers 0 because the table holds no row at this schema. Rolling back past it is an image
-- rollback alone. The identifier is the UTC moment this file was created, as every migration past
-- the numbered series is named.
create table intake_consumers (
    logical text primary key,
    record jsonb not null,
    revision bigint not null check (revision > 0)
);
