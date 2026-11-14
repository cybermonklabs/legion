-- 20261113082153_eval_registry.up.sql
--
-- The eval registry (dispatch://INFRA-2916, R1 and R2): every number a project page shows is an
-- eval, with its definition, its series and its readings in these three tables. An eval's id is
-- `<project_key>/<slug>`, and it is the same id in all three.
--
-- eval_versions holds one row per version of one definition, and nothing updates a row: any
-- change of a definition, a target included, writes the next version, so every version a reader
-- once saw stays readable. A reader reads evals_current, the newest version of each eval.
--   eval_id         `<project_key>/<slug>`; a check holds it to the two columns it is made of.
--   version         1 for an eval's first definition, then one more for each change.
--   project_key     the project the eval belongs to.
--   slug            the eval's name within the project: lower-case letters, digits and hyphens,
--                   starting with a letter or a digit.
--   name, question, number_rule, decision
--                   what the page shows of it. number_rule is empty for a computed eval, whose
--                   rule is the code its source names and is never restated here.
--   doc_block       the block of the project's `evals-md` document that holds its method.
--   failure_modes   a JSON array of {mode, guard}.
--   unit, direction higher_better or lower_better, and null until someone registers one.
--   target          null where the target is off; target_inclusive says whether the target
--                   value itself meets it.
--   target_judges   the `<series>.<stat>` values the target judges; empty means the headline
--                   statistic of the primary series.
--   owner_role, person, issue_key
--                   who owns the number.
--   kind            delivery, legion, ci or experiment: the page may group by it, and no
--                   computation branches on it.
--   source          `computed:<function>`, the one function that computes it, or `logged`.
--   state           active, stopped or replaced, with replaced_by naming the eval that took a
--                   replaced one's place, so a retired number stops and does not vanish.
--   links           a JSON array of links to results outside Dispatch.
--   written_via     how the version was written: seed, register, reading (a first reading under
--                   a slug no row held) or settings.
--   written_by      the actor who wrote it, and created_at when.
-- eval_versions_project, (project_key, eval_id, version desc), is the registry read of one
-- project. evals_current is distinct on (project_key, eval_id) rather than on eval_id alone,
-- which is the same set of rows because an eval id names its project: Postgres pushes a reader's
-- `project_key = $1` into the view only when the column is one the view is distinct on, so this
-- form reads one project's versions through that index, newest first, and the other form reads
-- every project's.
--
-- eval_series holds one row per version of one series of one eval, append-only and versioned the
-- same way, and eval_series_current is the newest version of each.
--   role            headline or control; is_primary marks the series the eval is judged by.
--   min_n           the count below which a value is provisional, or null for none.
--   listed          whether the list read folds the series. An unlisted series, such as one CI
--                   job's, is read by the items route alone.
--   agg             how a window folds the series' day rows: sum, ratio, quantile, max, last or
--                   weighted_mean.
--   links           as on eval_versions.
--
-- eval_readings holds the readings, one row per value logged under an eval, with the columns of
-- dispatch://INFRA-979/spec's readings table and links. A reading is append-only: a correction is
-- a new reading whose replaces names the reading it corrects, and the write that stores it sets
-- superseded_at on that reading. superseded_at is the one column a write changes, it is derived
-- from the corrections and never from a caller, and it is what lets a read skip replaced readings
-- with a filter rather than an anti-join.
--   eval_readings_value_or_reason
--                   refuses a reading with neither a value nor the reason it has none, so an
--                   absent value always says why and is never read as 0.
--   replaces        references eval_readings, so a correction names a reading that exists.
--   eval_readings_replaces
--                   is unique on replaces, so a reading is corrected once: a second correction
--                   of the same reading is refused, which no check in the write path can do
--                   under two concurrent writes.
--   eval_readings_row
--                   (eval_id, row_kind, row_key, measured_at desc), over the readings no
--                   correction replaced, is the read of an eval's rows and of one row's history
--                   (dispatch://INFRA-979/artifact/review-storage-measured-md, section 2), and
--                   being partial on superseded_at is null it is also the index of the readings
--                   that stand.
-- No index is keyed (eval_id, series, measured_at): the planner never chose that index in the
-- measurement, and it cost a write on every insert. Two reads carry no row kind and so key
-- eval_readings_row by eval_id alone: a readings write's read of a day row's newest standing
-- reading (evals' newestStanding) and the items route asked for no row_kind.
--
-- Three new tables, two views and their indexes, all empty: nothing existing is read or rewritten,
-- and the release before this one neither reads nor writes any of them, so the two builds roll
-- together (cmd/dispatch/README.md, "Database migrations"). eval_versions' foreign key to projects
-- takes SHARE ROW EXCLUSIVE on projects for the length of this transaction, which holds a project
-- write and no project read. The census answers 0, and rolling back past it is an image rollback
-- alone.
create table eval_versions (
    eval_id text not null,
    version integer not null check (version >= 1),
    project_key text not null references projects (key) on delete cascade,
    slug text not null check (slug ~ '^[a-z0-9][a-z0-9-]*$'),
    name text not null default '',
    question text not null default '',
    number_rule text not null default '',
    decision text not null default '',
    doc_block text not null default '',
    failure_modes jsonb not null default '[]'::jsonb check (jsonb_typeof(failure_modes) = 'array'),
    unit text not null default '',
    direction text check (direction in ('higher_better', 'lower_better')),
    target double precision,
    target_inclusive boolean not null default false,
    target_judges text[] not null default '{}',
    owner_role text not null default '',
    person text not null default '',
    issue_key text,
    kind text not null check (kind in ('delivery', 'legion', 'ci', 'experiment')),
    source text not null check (source = 'logged' or source ~ '^computed:[^[:space:]]+$'),
    state text not null default 'active' check (state in ('active', 'stopped', 'replaced')),
    replaced_by text,
    links jsonb not null default '[]'::jsonb check (jsonb_typeof(links) = 'array'),
    written_via text not null check (written_via in ('seed', 'register', 'reading', 'settings')),
    written_by jsonb not null,
    created_at timestamptz not null default now(),
    primary key (eval_id, version),
    constraint eval_versions_eval_id_check check (eval_id = project_key || '/' || slug)
);
create index eval_versions_project on eval_versions (project_key, eval_id, version desc);

create view evals_current as
    select distinct on (project_key, eval_id) *
    from eval_versions
    order by project_key, eval_id, version desc;

create table eval_series (
    eval_id text not null,
    series text not null check (series <> ''),
    version integer not null check (version >= 1),
    unit text not null default '',
    role text not null check (role in ('headline', 'control')),
    is_primary boolean not null default false,
    min_n integer check (min_n >= 0),
    listed boolean not null default true,
    agg text not null check (agg in ('sum', 'ratio', 'quantile', 'max', 'last', 'weighted_mean')),
    links jsonb not null default '[]'::jsonb check (jsonb_typeof(links) = 'array'),
    created_at timestamptz not null default now(),
    primary key (eval_id, series, version)
);

create view eval_series_current as
    select distinct on (eval_id, series) *
    from eval_series
    order by eval_id, series, version desc;

create table eval_readings (
    id uuid primary key default gen_random_uuid(),
    eval_id text not null,
    series text not null,
    row_key text not null default '',
    row_kind text not null,
    col text not null default '',
    value double precision,
    value_absent_reason text not null default '',
    unit text not null default '',
    n bigint,
    n_unit text not null default '',
    ci_low double precision,
    ci_high double precision,
    ci_level double precision,
    attributes jsonb not null default '{}'::jsonb check (jsonb_typeof(attributes) = 'object'),
    links jsonb not null default '[]'::jsonb check (jsonb_typeof(links) = 'array'),
    measured_at timestamptz not null,
    recorded_at timestamptz not null default now(),
    recorded_by jsonb not null,
    source text not null default '',
    note text not null default '',
    replaces uuid references eval_readings (id),
    superseded_at timestamptz,
    constraint eval_readings_value_or_reason check (value is not null or value_absent_reason <> '')
);
create index eval_readings_row on eval_readings (eval_id, row_kind, row_key, measured_at desc)
    where superseded_at is null;
create unique index eval_readings_replaces on eval_readings (replaces) where replaces is not null;
