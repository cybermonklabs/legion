-- 20261113175649_task_numbers.up.sql — a claim's task as a number the record writes and the
-- worker takes, rather than a prompt the daemon pushes and then judges by its acknowledgement
-- (INFRA-2811).
--
-- What stands before this migration is the push path's record of a pending task: three marks on
-- its pending_task_deliveries row saying what came of the prompts the daemon sent of it.
-- delivered_at is the acknowledgement of the latest prompt (0002_claims), marked_by the delivery
-- id that prompt carried (0018_delivery_marked_by), and prompted whether any prompt of the task
-- went out at all (20261029121000_claim_death_and_start_causes). A task the worker takes is
-- acknowledged by nothing: the take is the worker's own act, the daemon's answer to it is what
-- records the task as taken (confirmed_at), and the three marks have nothing left to say, so this
-- migration drops them.
--
-- The columns it adds are the record of that task as a number. claims.task_written is the highest
-- number the record queued, task_taken the number of the last take the daemon accepted,
-- task_finished the number of the last task whose turn finished, and task_finished_session the
-- claim's session when it finished it. task_seen_epoch and task_seen_revision are the bucket epoch
-- and revision of the claim's value that its worker last reported seeing, task_take_state what
-- came of a take at that revision ('' while none came, made, accepted or refused), and
-- task_written_epoch and task_written_revision the epoch and revision of the daemon's own last
-- write of the value. A revision is carried beside its epoch because a bucket created again counts
-- its revisions from 1 again. pending_task_deliveries.number is the number the pending task was
-- queued under, origin the source that queued it, taken_text the text the accepted take of it was
-- answered with, empty while no take of it stands accepted, so a take sent again after its answer
-- was lost is answered with that same text, before the take's turn starts and after;
-- turn_started whether that take's turn has started; and take_told which of the claim's two
-- records of the agent's earlier work that take marked told — 'abandoned' for the session the
-- claim abandoned (abandoned_session), 'ended' for what the last let-go of its process ended
-- (ended_work) — so a give-back of that take untells those and nothing an earlier take of the
-- same task told, whose delivery id is the same (supervise's Delivery.TakeTold).
--
-- The fill numbers what the release before this one left as each claim's first task. A claim
-- holding a pending row holds task 1: the row is number 1 and the claim's task_written is 1. A
-- row neither acknowledged nor confirmed is a task that release queued and nobody took, so
-- nothing of it is taken. A row acknowledged (delivered_at set) and not confirmed is a prompt a
-- process of the claim acknowledged and started no turn for. Where that process is the one the
-- claim records now, it holds the prompt and may still start its turn under this release: the
-- claim records a locator, is ready, idle or working, and its last_ready, the moment its agent
-- said it was ready, is no later than delivered_at. The release before this one writes last_ready
-- once a launch, at the first ready of the process the launch started (supervise's ready, on the
-- one row that leaves registered, which only a launch enters), and marks a prompt acknowledged
-- only on a ready or idle claim (supervise's acked), so an acknowledgement by the process the
-- claim records now comes after that process's ready, and one a process before it left — that
-- release keeps the mark through a death, a retirement and a suspension — comes before it. Such a
-- row is filled as an accepted take awaiting its turn — confirmed_at from delivered_at,
-- taken_text the row's own text, turn_started false, task_taken 1 — so the turn the old process
-- starts is that take's turn (supervise's Delivery.awaitsStart), and a process let go before it
-- starts one gives the take back (supervise's interrupted), so the relaunched worker takes the
-- task under its number: either way it runs once. The take is that process's, of the claim's
-- generation and session, which nothing but a let-go of that process moves, and the let-go gives
-- the take back. On any other claim — one suspended, queued or launching, one whose relaunched
-- process has not yet said it is ready, one failed or retired, one recording no locator, and one
-- whose process said it was ready after the acknowledgement — no process that stands holds the
-- prompt, so the row is left a task owed and not taken, and the next process takes it as it takes
-- any task, with the working copy's adoption, the notes it is owed and the confirmation an
-- accepted take makes (supervise's accept). Filled as a take there, it would be answered to that
-- next process from the record, with the stored text and nothing applied (supervise's
-- answeredFromRecord). A claim that records no ready at all is one whose process said it was
-- ready before 20261029121000_claim_death_and_start_causes added the column and was not launched
-- again since; nothing dates its acknowledgement, so its row is left owed too. Where the row is
-- confirmed — the turn it was sent for started — its worker took it, so task_taken is 1 as well
-- and the row's turn_started is true. A claim holding no pending row that served a task, a run
-- (serving_generation <> 0) or a phase entry (served_entry > 0), took and finished task 1, so
-- task_written, task_taken and task_finished are 1, and task_finished_session is the session it
-- holds — or, where that session is a new one that has taken nothing yet, the session it
-- abandoned. That is the claim whose abandoned_session is recorded and not told: that release
-- marked the abandoned session told at the confirmation of the first prompt the new session took,
-- so an untold one says the session the claim holds has finished nothing, and the task was
-- finished in the session it abandoned, the newest it can have been. Filled with the session it
-- holds, the claim would read as finished in that new session, and the resume rule would give it
-- no task (workflow's finishedInItsSession). Every other claim was given nothing and stays at 0
-- throughout. The epochs and revisions start empty, because no value in the bucket is the
-- record's yet, so the first boot writes every owed value once. origin starts empty, which names
-- no source: the task was queued before the record kept one. taken_text is the row's own text on
-- a row filled as a take and empty on every other, since no take of those was ever answered; a
-- confirmed row's take is therefore one a take sent again is refused in-turn, not answered
-- (supervise's answeredFromRecord). take_told names each of the claim's two records whose toldBy
-- is the row's delivery id: that release marked a record told at the confirmation of the prompt
-- that carried it, under the delivery's id, and gave the delivery a new id at every take-back,
-- so a record naming the row's id was told by the turn that confirmed the row, which is the take
-- the row holds now, and by nothing else. A row no turn confirmed is named by none.
--
-- The take the fill makes confirms its claim's handshake as an accepted take does (supervise's
-- EntryHandshake.confirmed): where the handshake names the row's entry and run and records no
-- confirmation yet, handshake_confirmed_at takes the take's confirmed_at, so the state serves
-- entryConfirmedAt beside entryQueuedAt, and a give-back of the take unconfirms the handshake
-- again (EntryHandshake.unconfirmed, which asks that the two times be one). The handshake's
-- columns are 20261114020537_claim_entry_handshake's, numbered after this file. Every database
-- the release before this one migrated records that migration, so they stand when this file runs
-- there. A database that takes the two in version order — a new one, or the roll-forward after a
-- rollback past contract 146, whose block drops them — holds them only after this file, and
-- holds no handshake to confirm either, since that migration adds them at defaults that name no
-- entry. So the statement runs inside a block that asks first whether the column stands.
--
-- Once this migration has run, the daemon of the release before it cannot read the claims table
-- at all: its own claim read names d.delivered_at, d.marked_by and d.prompted
-- (packages/daemon/internal/store/claims.go, claimSelect), so every claim read fails and the
-- supervisor loads nothing, and its delivery write names all three (putDelivery). Rolling the image
-- back therefore needs the three columns back first. This directory keeps no down migrations —
-- migrations here are forward-only and no .down.sql file exists — so the way back is the
-- twenty-two statements below, run with the daemon stopped. docs/kubernetes.md carries the same
-- block as the step an operator runs, beside the phase entry's, and the two say the same thing.
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   alter table pending_task_deliveries add column if not exists delivered_at timestamptz;
--   alter table pending_task_deliveries add column if not exists marked_by text not null default '';
--   alter table pending_task_deliveries add column if not exists prompted boolean not null default false;
--   update outbox set payload = payload - 'origin' where kind = 'supervise' and payload ? 'origin';
--   update outbox set payload = payload - 'mergeState' - 'stateReadAt' - 'launch' - 'sentLaunch' where kind = 'merge_forward' and (payload ? 'mergeState' or payload ? 'stateReadAt' or payload ? 'launch' or payload ? 'sentLaunch');
--   update claims set failure_cause = 'never_registered' where failure_cause = 'never_reconnected';
--   update claims set pending_cause = 'never_registered' where pending_cause = 'never_reconnected';
--   alter table claims drop column if exists task_written;
--   alter table claims drop column if exists task_taken;
--   alter table claims drop column if exists task_finished;
--   alter table claims drop column if exists task_finished_session;
--   alter table claims drop column if exists task_seen_epoch;
--   alter table claims drop column if exists task_seen_revision;
--   alter table claims drop column if exists task_take_state;
--   alter table claims drop column if exists task_written_epoch;
--   alter table claims drop column if exists task_written_revision;
--   alter table pending_task_deliveries drop column if exists number;
--   alter table pending_task_deliveries drop column if exists origin;
--   alter table pending_task_deliveries drop column if exists taken_text;
--   alter table pending_task_deliveries drop column if exists turn_started;
--   alter table pending_task_deliveries drop column if exists take_told;
--   delete from schema_version where version = 20261113175649;
--   commit;
--   SQL
--
-- The run prints BEGIN, three ALTER TABLEs, UPDATE <n> for the supervise rows whose payload
-- carried the key, UPDATE <m> for the merge-forward rows whose payload carried any of its four
-- keys, UPDATE <f> for the claims whose failure cause it rewrote, UPDATE <p> for those whose
-- pending cause it rewrote, fourteen ALTER TABLEs, DELETE 1 and COMMIT. A rerun prints a NOTICE
-- for each of the seventeen columns it finds as it means to leave them, the same ALTER TABLEs,
-- UPDATE 0 four times, DELETE 0 and COMMIT, and changes nothing. The transaction, the
-- ON_ERROR_STOP and the if-exists clauses are what make the twenty-two one act that may be run
-- twice, for the reason the phase entry's block gives: run as bare statements in a psql without
-- ON_ERROR_STOP, either half a failure leaves is a broken store — the drops taken while the delete
-- fails leave this release a schema it reads by names that no longer stand, and the delete taken
-- while a drop fails stops the roll-forward at its first add column.
--
-- The first three statements are the columns that daemon reads and writes, back at the defaults
-- their own migrations gave them: no acknowledgement, no marking prompt and no prompt sent. That is
-- the record of a task the older daemon has queued and not yet sent, so it sends an unconfirmed
-- pending task at its claim's next ready as it sends any task it queued itself, and reads a
-- confirmed one as a turn under way.
--
-- The fourth is the payload key this release writes, taken back out, as the phase entry's fourth
-- takes out the two keys that release writes. This release writes origin into every supervise
-- start a source queues (record.SuperviseRequest.Origin); the release before it knows no such key
-- and decodes an outbox payload strictly (record.DecodeOutboxPayload, DisallowUnknownFields), and
-- its record.Postgres.RoleRuns decodes every supervise row of every issue an admission pass reads,
-- so a row carrying the key left in the table fails to decode for ever: the runner retries it on
-- its backoff and every admission pass of its issue fails with it. Nothing that daemon reads is
-- lost with the key, since it records no origin, and a row the roll-forward finds naming none is
-- read as a phase start where it names a phase and an operator's task otherwise (supervise's
-- originOf), as a row of the release before this one is.
--
-- The fifth takes out the four keys this release writes into a merge-forward row: the two each in
-- an ask or hold whose record holds it, the read of GitHub the act rests on
-- (record.MergeForwardMessage.MergeState and StateReadAt), and the two in an ask the re-send at a
-- launch's ready or a reopen queues, the launches of the asked role's claim the ask was told
-- (record.MergeForwardMessage.Launch and SentLaunch). The release before it knows none of them and
-- decodes that row strictly too, so a row still carrying any fails to decode at every attempt,
-- retried on the runner's backoff, and its message is never delivered. Nothing that daemon reads
-- is lost with them, since its ask and its hold name no read and carry no launch; a row the
-- roll-forward finds carrying no read says that GitHub's merge state and the time of its read are
-- unknown (record.AnsweredMergeState), and an ask carrying no launch goes to whatever launch
-- holds the role, as that daemon sends it.
--
-- The sixth and seventh write never_registered over never_reconnected in the claim's two cause
-- columns, failure_cause and pending_cause (20261111212544_claim_failure_cause). never_reconnected
-- is the cause this release adds (supervise's CauseNeverReconnected): it is the pending cause of
-- the relaunch the daemon makes when a live process's shim says no hello within the reconnect
-- deadline, after a daemon start restored its claim or after its connection closed over a pending
-- task, and the failure cause once those relaunches run the launch budget out
-- (reconnectDeadline), so only this release, from the restart that installs it on, writes it.
-- The release before it reads both columns unchecked and serves them verbatim, in
-- a claim view's failure.cause and pendingCause, and its plugins read the state through a strict
-- enum of the causes they were built with (legion-api.ts's LEGION_FAILURE_CAUSES), which names no
-- such word: one claim left carrying it refuses the whole state body at every plugin that reads
-- it, until that claim runs again. never_registered is the nearest cause that release knows, a
-- process that never said hello before its deadline, and the word this release itself serves a
-- plugin below its contract in the other's place (api's withoutNeverReconnected). The roll-forward
-- reads that nearer word back for the one episode it names: the launch that runs the claim again
-- clears a failure cause, and the agent that comes up a pending one.
--
-- The fourteen drops and the deleted version row are for the roll-forward that follows the rollback
-- rather than for the older daemon, which reads none of the fourteen columns and writes none of them:
-- its claim and delivery upserts name their own columns (putClaim, putDelivery), so a row it
-- inserts takes each new column's default and a row it updates keeps whatever the column held.
-- Left standing, those columns would be read on the roll-forward as current when nothing kept them
-- so: a task that daemon queued carries number 0 under a claim whose task_written names an older
-- task, and a task it finished leaves task_finished where it stood. Together the drops and the
-- delete re-arm this migration. store.apply decides each migration on its own row —
-- versionRecorded asks `select exists (select 1 from schema_version where version = $1::bigint)`
-- (internal/store/store.go) — so deleting this one row leaves this migration unapplied and every
-- other migration recorded and untouched, and when the newer image comes back its Migrate runs this
-- file again from its first statement. The fourteen columns go with the row because those first
-- statements add them and `add column` on a column that already stands fails; the three marks the
-- block brought back are dropped again by this file's last statements. The missing row does not
-- move the number the state read reports while the older image runs (Store.SchemaVersionTx, the
-- highest row recorded): every database this release migrated records
-- 20261124143026_issue_release_teardown, that older daemon's own newest migration, above this
-- version, so the number reads 20261124143026 before the delete and after it, which is the truth
-- about the image that is running. A rollback to a release before #5372 reads 20261124143026 there
-- too: neither that migration nor #5349's 20261124073512 nor #5365's 20261124060500 carries a way
-- back, so their rows stand, and the way back in the header of 20261123224717_members_owed, which
-- a rollback to a release before #5303 runs first, deletes that version's row alone.
--
-- What that costs is the numbering, as the phase entry's second fill costs its own: the second
-- fill reads what the older daemon left exactly as this fill reads what the release before this
-- one left, so every claim stands at task 1 again, or at 0, whatever number it stood at before the
-- rollback. The one record a number outlives the rollback in is processed_events, which nothing
-- prunes: the accepted take's confirmation is applied once under an event id, and that id names
-- the task's delivery id beside its number (internal/daemon's confirmationEventID), so the
-- confirmation of a task numbered 1 again is not taken for one of the task 1 the claim took
-- before the rollback. Nothing else reads a number across it: the worker of the older release
-- takes no value, and the epochs and revisions start empty again, so the first boot after the
-- roll-forward writes every owed value once under the number the second fill gave it.
--
-- The re-arm asks nothing of the other rows. What it does need is that no migration recorded
-- after this one builds on the fourteen columns it drops whose way back has not already run, and
-- at this release thirty of the thirty-one recorded after it touch none of them:
-- 20261113181500_architect_stalled_notices adds the table
-- architect_stalled_notices, 20261113182000_architect_entry_upgrade writes the root architects'
-- phases rows, their claims' given_entry and served_entry and that table's marks,
-- 20261114020537_claim_entry_handshake adds the four claims.handshake_* columns,
-- 20261114031356_merge_queue_removal rewrites outbox_kind_check,
-- 20261114061900_phase_entry_verbatim adds phases.entry_verbatim,
-- 20261115194357_project_pause adds the tables project_pauses, project_pause_stops and
-- project_pause_pod_holds, pod_resumes.clock_at with the trigger that fills it,
-- pod_resumes.pause_id and outbox.held_by_pause, and rewrites no kind list,
-- 20261116144907_claim_sessions adds the table claim_sessions and fills it from each claim's
-- session, its file and the session it last abandoned,
-- 20261116165926_role_holders_publish rewrites outbox_kind_check again and adds the unique index
-- outbox_role_holders_publish_issue_unique, 20261116234034_phase_reviewed_head adds
-- phases.reviewed_head and pending_task_deliveries.round_named and ends each task a reviewer's row
-- stores with the review round's two sentences, 20261117000528_outbox_kinds_with_the_step_record
-- rewrites outbox_kind_check a third time, 20261117004117_step_starts adds the tables step_starts
-- and step_starts_rollback and the function step_run_witness, which reads issues and phases,
-- 20261117023932_outbox_kinds_with_the_plan_record rewrites outbox_kind_check a fourth time,
-- 20261117034837_issue_plans (cybermonklabs/legion#4611) adds the tables issue_plans, plan_tasks
-- and plan_steps and the columns phases.step_task and phases.step_place, and rewrites no kind list,
-- 20261117064152_step_starts_roll_forward writes step_starts, step_starts_rollback and the
-- step_record rows it adds to outbox, 20261117112334_pull_request_statuses adds
-- pull_requests.statuses, 20261117200337_update_ask_marks_at_delivery clears
-- claims.update_tasked_at where that claim's ask still waits in the outbox,
-- 20261119003157_conflict_reads_and_merge_forward rewrites outbox_kind_check a fifth time, deletes
-- the queued phase_ended rows and adds pull_requests.conflict_read and pull_requests.merge_forward,
-- 20261119203047_design_news (cybermonklabs/legion#3995) adds the tables design_news_told,
-- design_changes and design_history_unread, eight functions and the columns
-- pending_task_deliveries.design_news, carried_news, carried_ran and news_only, and rewrites
-- outbox_kind_check a sixth time with design_changes,
-- 20261120012547_set_back_observed_seq changes no table and runs no statement,
-- 20261120042902_claim_agent_resume_generation adds claims.agent_resume_generation at its default,
-- 0, and rewrites no kind list,
-- 20261120055717_dispatch_issue_value_sequences adds seven columns of dispatch_issues, an
-- index on dispatch_issues.parent and the table dispatch_issue_refills, and rewrites no kind list,
-- 20261120144241_intake_failures (cybermonklabs/legion#4590) adds the tables intake_failures,
-- cleared_pull_requests and pull_request_settled_heads, and rewrites no kind list,
-- 20261121170714_dispatch_issue_applied_seq (cybermonklabs/legion#4903) adds
-- dispatch_issues.applied_seq at its default, 0, and rewrites no kind list,
-- 20261123050533_idle_release_keeps_no_workspace (cybermonklabs/legion#4869) drops
-- workspace_bundles and adds the table idle_release_declines and the column
-- issue_releases.turn_start, and rewrites no kind list,
-- 20261123165139_experiment_assignment_plan (cybermonklabs/legion#4930) adds
-- experiment_assignments.plan at its default, '', and rewrites no kind list,
-- 20261123212127_claim_start_failure_after_ready (cybermonklabs/legion#5278) empties
-- claims.last_start_failure on every claim stored ready, working or idle, and rewrites no kind
-- list, 20261123224717_members_owed (cybermonklabs/legion#5303) adds issues.members_owed, true on
-- the roots whose re-admission has still to bring their members back, and rewrites no kind list,
-- 20261124060500_issue_priority (cybermonklabs/legion#5365) adds issues.priority, null on every
-- row it finds, and rewrites no kind list, 20261124073512_claim_cause_times
-- (cybermonklabs/legion#5349) adds claims.last_death_at, claims.last_start_failure_at,
-- claims.last_death_digest and claims.last_start_failure_digest, null on every row it finds, and
-- rewrites no kind list, and 20261124143026_issue_release_teardown (cybermonklabs/legion#5372)
-- adds issue_releases.teardown at its default, 'reached', and pod_releases.issue_release_id, null
-- on every row it finds, with a unique index of the rows that name a mark, and rewrites no kind
-- list.
-- 20261119203047_design_news reads no column this block drops or puts back: its functions read
-- pending_task_deliveries.confirmed_at, which both releases keep, and its four columns stand
-- through the block, so this block reverses nothing of it, and the two hold in either order.
-- The twenty-fourth, 20261122160000_design_news_credit_at_turn_start, is this release's own: it
-- writes #3995's function design_news_told_in again, reading pending_task_deliveries.turn_started,
-- which this block drops, so its way back, the last paragraph of its header, runs before this
-- block and writes #3995's text of the function back; every rollback of this release starts with
-- it, after the way back of 20261123224717, the step of 20261123165139 and the way back of
-- 20261123050533 where the rollback crosses those too.
-- 20261123050533_idle_release_keeps_no_workspace, above both, carries a way back of its own, the
-- last paragraph of its header (docs/kubernetes.md, "Rolling back the release that keeps no saved
-- workspace"), which makes workspace_bundles again, empty, drops
-- idle_release_declines and issue_releases.turn_start and deletes its version row, with every
-- daemon of the database stopped; it reads nothing 20261122160000's way back or this block drops
-- or writes back.
-- 20261123212127_claim_start_failure_after_ready, above all three, needs nothing: it carries no
-- way back, since an earlier release reads the start failures it emptied as none recorded.
-- 20261123224717_members_owed, above all four, carries a way back of its own, the last paragraph
-- of its header, which drops issues.members_owed and deletes its version row, with every daemon of
-- the database stopped; it reads nothing 20261122160000's way back or this block drops or writes
-- back. 20261124060500_issue_priority, 20261124073512_claim_cause_times and
-- 20261124143026_issue_release_teardown, above all five, need nothing: none carries a way back,
-- since an earlier release names none of the columns they add (each migration's header), and
-- none reads what 20261122160000's way back or this block drops or writes back. The release before
-- this one carries the other thirty: it is contract 256, the one whose internal/api/version.go
-- carries the entry for the claims' cause times, cybermonklabs/legion#5349's lastDeathAt and
-- lastStartFailureAt on each claim view of GET /legion/v1/state, as main releases it after
-- #5372, its newest migration #5372's 20261124143026_issue_release_teardown, so the rollback to it
-- runs 20261122160000's way back and this block and leaves the thirty standing. A rollback to
-- contract 246, or to any release before it, crosses contract 256 and #5365's contract 250, which
-- take no step of their own beyond pinning the worker image with the daemon, since they add no
-- legion.yaml key and their migrations, 20261124073512_claim_cause_times and
-- 20261124060500_issue_priority, need nothing, and neither does #5372's
-- 20261124143026_issue_release_teardown; crossing 20261124073512 costs the claims' two cause
-- times, which the roll-forward serves only beside a cause whose text it dated (that migration's
-- header). A rollback to contract 232, or to any release
-- before it, crosses contract 246, which takes no step of its own beyond pinning the worker image
-- with the daemon, since it adds no migration and no legion.yaml key, and it, like any rollback to
-- a release before #4869, crosses 20261123050533_idle_release_keeps_no_workspace, and so runs that
-- migration's own way back before 20261122160000's. A rollback to a release before #5303 also
-- crosses 20261123224717_members_owed, and so runs that migration's own way back first, before
-- 20261122160000's. A rollback to contract 228, or to any release before it, crosses contract
-- 232, which takes no step of its own beyond pinning the worker image with the daemon: it adds no
-- migration and no legion.yaml key. A rollback to contract 220, or to any
-- release before it, crosses contract 228, and so also takes that release's steps
-- (docs/kubernetes.md, "Rolling back the release that gives up on intake messages"), which, with
-- the daemon stopped, delete the project's unsent intake-gave-up controller notices and its open
-- failures, rows neither this block nor 20261122160000's way back reads, and keep the version row
-- of 20261120144241_intake_failures and its three tables, which no earlier release reads. A
-- rollback to contract 219, or to any release before it, also crosses contract 220, and so first
-- takes that release's one step, before 20261122160000's way back: `plan` comes out of every
-- variant of legion.yaml that names it before the earlier
-- image is pinned (docs/kubernetes.md, "Rolling back the release whose experiment sets how each
-- issue is done"), and the store needs no statement for it, since the earlier release names the
-- columns of experiment_assignments in every statement it runs and leaves the column standing.
-- A rollback to contract 217, or to any release before
-- it, crosses contract 219, and so also takes that release's steps: those of #5161
-- (docs/kubernetes.md, "Rolling back the release that runs a task's steps in any order"), and the
-- way back in the header of 20261117034837_issue_plans, its last paragraph, between those two,
-- after 20261122160000's way back and before this block, run with every project's daemon on the
-- database stopped and once for each project: it drops the tables issue_plans, plan_tasks and
-- plan_steps and the columns phases.step_task and phases.step_place, deletes the queued notices
-- and step_stop rows an earlier release cannot decode, clears the hold reasons task-stopped and
-- children and deletes its version row, and projects.<KEY>.default_steps comes out of legion.yaml
-- too. A rollback to a release before #4903 also runs the way back in the header of
-- 20261121170714_dispatch_issue_applied_seq, its last paragraph, between those two, after
-- 20261122160000's way back and before 20261117034837's, with every daemon of the database
-- stopped: it drops dispatch_issues.applied_seq and deletes its version row. A rollback to
-- contract 213, or to any release before it, crosses contract 217 too, and so also takes that
-- release's two steps of its own (docs/kubernetes.md, "Rolling back below the release that guards
-- the controller's pod's memory"): runtime.kubernetes.memory_guard.controller_pod comes out of
-- legion.yaml before the earlier image is pinned, and the unsent memory-guard-kill notices are
-- deleted once. A rollback to a release before #3995, contract 213 without it or contract 205,
-- runs those four ways back (20261122160000's, 20261121170714's, 20261117034837's and this block)
-- and then #3995's four statements (docs/kubernetes.md, "Rolling back the release that tells a
-- holder of an approved new version"), which read none of the fourteen columns. A rollback to
-- contract 202, the
-- release before 20261120012547_set_back_observed_seq, runs those, #3995's four statements and then
-- that migration's own statement, the last paragraph of its header (docs/kubernetes.md, "Rolling
-- back the release that lets the owning architect's status stand"), which takes observedSeq off
-- each set-back of the project. A rollback to contract 188, the release before
-- 20261120042902_claim_agent_resume_generation, runs those and then the way back in that
-- migration's own header, its last paragraph, which deletes the queued session-forgotten notices
-- no earlier release decodes, drops claims.agent_resume_generation and deletes its version row. A
-- rollback to a release before 20261120055717_dispatch_issue_value_sequences runs those and
-- then that migration's own statement, the last paragraph of its header (docs/kubernetes.md,
-- "Rolling back the release that fences each value of the copy of Dispatch's issues"). Contract
-- 160's two other migrations,
-- 20261111090000_volume_trim_notices and 20261111091500_claim_pod_end, are numbered below this one,
-- so every database records them before it, and neither touches the fourteen columns. A rollback
-- past it — to contract 87, across 20261111225552_phase_entry — crosses every release between the
-- two and runs their ways back newest first, with the daemon stopped, as docs/kubernetes.md lists
-- them: #5372's 20261124143026_issue_release_teardown, #5349's 20261124073512_claim_cause_times
-- and #5365's 20261124060500_issue_priority first, which need nothing, since none carries a way
-- back and no earlier release names the columns they add; the way back in the header of
-- 20261123050533_idle_release_keeps_no_workspace,
-- #4869's, which makes workspace_bundles again, empty, drops idle_release_declines and
-- issue_releases.turn_start and deletes its version row, with every daemon of the database
-- stopped; the way back in the header of 20261123224717_members_owed, #5303's, which drops
-- issues.members_owed and deletes its version row, with every daemon of the database stopped;
-- #5278's 20261123212127_claim_start_failure_after_ready, which needs nothing, since it carries
-- no way back and an earlier release reads the start failures it emptied as none recorded;
-- contract 228's steps (docs/kubernetes.md, "Rolling back the release that gives up on
-- intake messages"), which delete the project's unsent intake-gave-up controller notices and its
-- open failures and keep the version row of 20261120144241_intake_failures and its three tables;
-- contract 220's step, which takes `plan` out of every variant of legion.yaml that names
-- it (docs/kubernetes.md, "Rolling back the release whose experiment sets how each issue is done");
-- 20261122160000_design_news_credit_at_turn_start's way back, which writes back #3995's
-- design_news_told_in; the way back in the header of 20261121170714_dispatch_issue_applied_seq,
-- #4903's, which drops dispatch_issues.applied_seq and deletes its version row, with every daemon
-- of the database stopped; the steps of contract 219's #5161, which runs a task's steps in any
-- order (docs/kubernetes.md, "Rolling back the release that runs a task's steps in any order"),
-- and the way back in the header of 20261117034837_issue_plans, contract 219's, which drops the
-- tables issue_plans, plan_tasks and plan_steps and the columns phases.step_task and
-- phases.step_place, deletes the queued notices and step_stop rows no earlier release decodes,
-- clears the hold reasons task-stopped and children and deletes its version row, with every
-- project's daemon on the database stopped and once for each project; contract 217's delete of
-- the unsent memory-guard-kill notices, since an
-- earlier release cannot send them: it refuses a controller notice of a kind it does not know
-- (docs/kubernetes.md, "Rolling back below the release that guards the controller's pod's
-- memory"); this block; #3995's four statements, which delete the queued news-only starts and the
-- news-only tasks and take the news off each pending task (docs/kubernetes.md, "Rolling back the
-- release that tells a holder of an approved new version"); the statement in the header of
-- 20261120012547_set_back_observed_seq, contract
-- 205's, which takes observedSeq off each set-back of the project; the way back in the header of
-- 20261120042902_claim_agent_resume_generation, contract 189's, which deletes the queued
-- session-forgotten notices no earlier release decodes, drops claims.agent_resume_generation and
-- deletes its version row; the statement of the release
-- that fences each value of the copy of
-- Dispatch's issues, which marks each project that has rows of that copy again, so that project's
-- next boot on a release that carries 20261120055717_dispatch_issue_value_sequences refills them
-- (docs/kubernetes.md, "Rolling back the release that fences each value of the copy of Dispatch's
-- issues"); contract 178's three statements for the project pause, which delete the
-- controller's pause and resume notices and the queued pause_stop rows and take pause and wakeKind
-- off each queued start row, after the project is resumed and each held pod has a role running
-- (docs/kubernetes.md, "Rolling back the project pause"); contract 168's steps for its merge
-- forward, which delete the queued second
-- reads and merge-forward messages and clear pull_requests.conflict_read and merge_forward, and
-- whose delete of the phase_ended rows the roll-forward runs again (docs/kubernetes.md, "Rolling
-- back the release that merges a conflicting head forward in its phase"); contract 168's steps
-- for its plans, which delete the queued writes of the runs' plans and of the retros' ratings,
-- since the release that records each step fails the decode of a rating row at every attempt
-- (docs/kubernetes.md, "Rolling back the release that rates each plan");
-- contract 160's statement, which deletes every queued volume-near-full notice,
-- since no earlier release can decode one, with the counts of each pod it was of
-- (docs/kubernetes.md, "Rolling back below the release that trims an issue pod's volume"); the step
-- records' way back, which keeps each issue's witness while the phase entries it reads stand and
-- re-arms 20261117064152_step_starts_roll_forward; the commit statuses' block in
-- 20261117112334_pull_request_statuses's header, which no earlier release needs, since none names
-- pull_requests.statuses, and which clears for the roll-forward a list no earlier release keeps
-- current; the reviewed commit's block, which takes the key roundNamed off each start row
-- contract 87 decodes strictly; the role-holders publication's outbox delete, which leaves no row
-- of its kind behind; contract 146's block, which reverses the four of those thirty that are
-- the root architect's entry (all but the six kind lists, the five other migrations of contract
-- 152, contract 160's 20261116144907_claim_sessions, contract 178's 20261115194357_project_pause,
-- contract 188's 20261120055717_dispatch_issue_value_sequences, contract 189's
-- 20261120042902_claim_agent_resume_generation, contract 205's
-- 20261120012547_set_back_observed_seq, contract 228's 20261120144241_intake_failures, whose
-- tables stand, #4903's 20261121170714_dispatch_issue_applied_seq, #4869's
-- 20261123050533_idle_release_keeps_no_workspace, whose own way back has run,
-- contract 220's 20261123165139_experiment_assignment_plan, contract 219's
-- 20261117034837_issue_plans, whose own way back has run, #5278's
-- 20261123212127_claim_start_failure_after_ready, which carries none, #5303's
-- 20261123224717_members_owed, whose own way back has run, #5365's
-- 20261124060500_issue_priority, #5349's 20261124073512_claim_cause_times and #5372's
-- 20261124143026_issue_release_teardown, which carry none, and #3995's
-- 20261119203047_design_news, whose own four statements are its way back);
-- the merge-queue removal's outbox delete; the
-- five statements of the release whose update round ends at its bound, whose
-- 20261113110610_drop_claim_update_answer_marks dropped two columns
-- contract 87's claim read names; the steps of contract 111's slot past review and of contract
-- 98's held list; and last the phase entry's own block. Contract 141's
-- 20261111045636_experiment_assignments, contract 133's 20261112070503_pull_request_state_read_at
-- and 20261113082415_claim_ended_work, which cybermonklabs/legion#3983 landed at that contract
-- too, and contract 132's 20261111214351_issue_released_for_wait have no way back of their own,
-- and each adds one column or table no earlier release names; contract 123's
-- 20261113090512_phase_entry_moved_back, contract 152's
-- 20261117000528_outbox_kinds_with_the_step_record and 20261117004117_step_starts, which the step
-- records' way back leaves standing, its 20261117200337_update_ask_marks_at_delivery and contract
-- 168's 20261117023932_outbox_kinds_with_the_plan_record, whose kind the check keeps after the
-- rollback, need no statement on the way back, and contracts 151, 144, 137 and 104 add no
-- migration. Nor does contract 178's 20261115194357_project_pause, whose tables and columns no
-- earlier release names, nor contract 220's 20261123165139_experiment_assignment_plan, whose
-- experiment_assignments.plan no earlier release names, nor #5278's
-- 20261123212127_claim_start_failure_after_ready, whose emptied start failures an earlier release
-- reads as none recorded, nor #5365's 20261124060500_issue_priority, #5349's
-- 20261124073512_claim_cause_times or #5372's 20261124143026_issue_release_teardown, whose columns
-- no earlier release names, nor
-- contract 160's 20261111091500_claim_pod_end, whose claims.pod_end an earlier release leaves
-- unread and unwritten; the tables pod_volume_notices its 20261111090000_volume_trim_notices adds,
-- claim_sessions its 20261116144907_claim_sessions adds and the three contract 228's
-- 20261120144241_intake_failures adds stand too, read by no earlier release.
-- The release before the phase entry reads the three
-- marks as the release before this one does, and neither
-- contract 146's block, the step records', the commit statuses', the reviewed commit's nor the
-- phase entry's touches this migration's columns. The roll-forward after them applies again, in
-- version order, every migration those ways back re-armed: the phase entry's first, then
-- 20261113110610_drop_claim_update_answer_marks, which the update round's way back re-arms, then
-- this one and contract 146's four after it, so this fill reads the served_entry the phase entry's
-- second fill wrote, before 20261113182000 settles each root architect at entry 1 again, and
-- 20261116234034_phase_reviewed_head, 20261117064152_step_starts_roll_forward and
-- 20261117112334_pull_request_statuses last.
alter table claims add column task_written bigint not null default 0;
alter table claims add column task_taken bigint not null default 0;
alter table claims add column task_finished bigint not null default 0;
alter table claims add column task_finished_session text not null default '';
alter table claims add column task_seen_epoch text not null default '';
alter table claims add column task_seen_revision bigint not null default 0;
alter table claims add column task_take_state text not null default ''
  check (task_take_state in ('', 'made', 'accepted', 'refused'));
alter table claims add column task_written_epoch text not null default '';
alter table claims add column task_written_revision bigint not null default 0;
alter table pending_task_deliveries add column number bigint not null default 0;
alter table pending_task_deliveries add column origin text not null default '';
alter table pending_task_deliveries add column taken_text text not null default '';
alter table pending_task_deliveries add column turn_started boolean not null default false;
alter table pending_task_deliveries add column take_told text[] not null default '{}'
  check (take_told <@ array['abandoned', 'ended']);

-- A confirmed row's turn started: the release before this one confirmed a task at its turn's
-- start. This comes before any other fill, so it reads confirmed_at as that release left it.
update pending_task_deliveries set turn_started = confirmed_at is not null;

-- An acknowledged row that no turn confirmed, acknowledged by the process its claim records now —
-- the claim records a locator, is ready, idle or working, and its agent said it was ready
-- (last_ready) no later than the acknowledgement — is a take accepted at its acknowledgement and
-- awaiting its turn, its text the prompt's; turn_started stays false. Every other such row is one
-- no standing process holds, and it stays a task owed and not taken. A claim that records no
-- ready dates nothing, so its row stays owed too.
update pending_task_deliveries d set confirmed_at = d.delivered_at, taken_text = d.task
from claims c
where c.token = d.claim_token
  and c.locator is not null
  and c.state in ('ready', 'idle', 'working')
  and d.delivered_at is not null and d.confirmed_at is null
  and d.delivered_at >= c.last_ready;

-- The take made above confirms its claim's handshake where the handshake names the task's entry
-- and run and records no confirmation yet, at the take's own moment, as an accepted take does.
-- The takes made above are the confirmed rows whose turn has not started. The handshake's columns
-- come with 20261114020537_claim_entry_handshake, numbered after this file, so the statement runs
-- only where they stand (the header says why that is enough).
do $$
begin
  if exists (select 1 from information_schema.columns where table_schema = current_schema()
      and table_name = 'claims' and column_name = 'handshake_confirmed_at') then
    update claims c set handshake_confirmed_at = d.confirmed_at
    from pending_task_deliveries d
    where d.claim_token = c.token
      and d.confirmed_at is not null and not d.turn_started
      and d.entry <> 0 and d.entry = c.handshake_entry and d.generation = c.handshake_generation
      and c.handshake_confirmed_at is null;
  end if;
end $$;

-- Every pending row is its claim's task 1, written; a confirmed one — one the standing process
-- acknowledged among them now — is taken as well.
update pending_task_deliveries set number = 1;
update claims c set task_written = 1, task_taken = case when d.confirmed_at is null then 0 else 1 end
from pending_task_deliveries d
where d.claim_token = c.token;

-- A claim holding no pending row that served a run or a phase entry took and finished task 1: in
-- the session it holds, unless that session has taken nothing since the claim abandoned the one
-- before it (abandoned_session recorded and not told), which then finished it.
update claims c set task_written = 1, task_taken = 1, task_finished = 1,
  task_finished_session = case
    when coalesce(c.abandoned_session ->> 'session', '') <> ''
      and not coalesce((c.abandoned_session ->> 'told')::boolean, false)
    then c.abandoned_session ->> 'session'
    else c.session end
where (c.serving_generation <> 0 or c.served_entry > 0)
  and not exists (select 1 from pending_task_deliveries d where d.claim_token = c.token);

-- A record the release before this one marked told names the delivery whose confirmation told it
-- (toldBy), and that release gave a delivery a new id at every take-back, so a record naming the
-- row's id was told by the take the row holds now. An empty toldBy names no delivery and matches
-- nothing.
update pending_task_deliveries d set take_told = array_remove(array[
    case when c.abandoned_session ->> 'toldBy' = d.delivery_id then 'abandoned' end,
    case when c.ended_work ->> 'toldBy' = d.delivery_id then 'ended' end], null)
from claims c
where c.token = d.claim_token
  and d.delivery_id <> ''
  and d.delivery_id in (c.abandoned_session ->> 'toldBy', c.ended_work ->> 'toldBy');

alter table pending_task_deliveries drop column delivered_at;
alter table pending_task_deliveries drop column marked_by;
alter table pending_task_deliveries drop column prompted;
