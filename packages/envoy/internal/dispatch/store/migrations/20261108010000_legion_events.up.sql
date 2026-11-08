-- 20261108010000_legion_events.up.sql — the Legion daemon pushes its changes (prototype of
-- dispatch://INFRA-979, "The daemon sends its changes to Dispatch").
--
-- legion_events is every change event a daemon delivered, once per (project, generation, seq): a
-- repeated delivery inserts nothing. The board still reads legion_observations, which the route
-- writes from each new event at the event's own time.
--
-- legion_pushes is one row per project: the generation (one daemon boot) it last heard, the last
-- sequence number it holds with no gap below it, and the time of the newest event or heartbeat,
-- which is the freshness the board's header shows.
create table legion_events (
  project text not null references projects(key) on delete cascade,
  generation text not null,
  seq bigint not null,
  at timestamptz not null,
  kind text not null,
  issue_key text not null default '',
  value jsonb not null default 'null'::jsonb,
  received_at timestamptz not null default now(),
  primary key (project, generation, seq)
);

create table legion_pushes (
  project text primary key references projects(key) on delete cascade,
  generation text not null,
  last_seq bigint not null,
  last_at timestamptz not null,
  updated_at timestamptz not null default now()
);
