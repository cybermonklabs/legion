-- 20261116082001_drop_removed_reach_notices.up.sql — what an earlier build of the release that
-- reports claims out of reach left in a database it ran against (the e2e lanes' fixtures, a dev
-- stack), from before that release dropped its notices of an unreachable claim: outbox rows of the
-- two notice kinds it no longer has (reach_notice, reach_controller_notice), the rows of those
-- notices a still earlier build queued under notice and controller_notice, and the table of the
-- project's notice marks (reach_project_notices). No release wrote any of them, so a deployment's
-- database deletes nothing. 20261029120800 deletes the rows of the merge kinds it retired the same
-- way.
--
-- A daemon of this release never runs a row of the two kinds and lets it hold back no other row:
-- no lane holds its kind (internal/record's LaneOf), so the runner starts no executor for it
-- (Postgres.UnfinishedLanes), no lane lists it (Postgres.DueRows, which each lane asks for its own
-- kinds), and the lane rule counts lifecycle rows alone (outboxLaneFree). But a later migration
-- that writes outbox_kind_check without that kind is refused over it (SQLSTATE 23514), which fails
-- Migrate and leaves the daemon unable to boot. A notice or controller_notice row of a removed
-- notice fails its decode at every try (internal/record's DecodeOutboxPayload). This version
-- stands below every kind list a later release adds, so on such a database it runs before them.
-- The version 20261116082000 such a database recorded names a migration this release does not
-- embed, which Migrate passes over.
--
-- Rolling the image back needs no statement: the release before this one names neither kind,
-- none of the notices, nor the table.
--
-- There is no down file: migrations here are forward-only.
delete from outbox where kind in ('reach_notice', 'reach_controller_notice')
  or (kind = 'notice' and payload->>'kind' = 'claim-out-of-reach')
  or (kind = 'controller_notice' and payload->>'kind' in ('unreachable-claims', 'relaunch-ready',
    'relaunch-guard-held', 'relaunch-not-back', 'unreachable-exit', 'envoy-no-live-session',
    'worker-locked-out', 'claim-out-of-reach'));
drop table if exists reach_project_notices;
