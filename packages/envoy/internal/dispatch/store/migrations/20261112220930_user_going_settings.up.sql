-- 20261112220930_user_going_settings.up.sql — a person's own setting of the command bar.
--
-- A person may ask that every write the command bar makes for them wait for their confirmation
-- (R4 of the command bar's spec). The setting is the person's own and follows them to every
-- browser they sign in from, so it is held here rather than in one browser's storage, and the
-- writes route enforces it (api/going_writes.go, goingAutoRefusal): with confirm_writes on, a write
-- sent to run at once is refused and the bar sends it confirmed.
--
-- - login is the person's canonical login (model.CanonicalLogin), as user_agent_read's (0051) and
--   user_agent_pin's (20261106215223) are, so one person holds one setting however their identity
--   source spells them. GET and PUT /api/v1/me/going/settings read and write the caller's own row
--   and nobody else's (api/going_settings.go).
-- - confirm_writes is the setting. A person with no row reads false: the setting is off until that
--   person turns it on.
-- - updated_at is when it was last written.
--
-- A new table no earlier release names: nothing existing is read, locked or rewritten, its census
-- answers 0, and the release before this one neither reads nor writes it, so it rolls beside this
-- one (cmd/dispatch/README.md, "Database migrations"). The identifier is the UTC moment this was
-- written, as `migrationid` requires of every migration past the numbered series.
create table user_going_settings (
  login text primary key,
  confirm_writes boolean not null,
  updated_at timestamptz not null default now()
);
