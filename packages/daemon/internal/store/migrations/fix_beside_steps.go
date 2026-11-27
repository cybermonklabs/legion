package migrations

// FixBesideSteps is the version of 20261127131056_fix_beside_steps.up.sql, the migration of the
// release that starts a fix beside the other steps of a task, whose header carries that release's
// way back (WayBack): the one spelling of it every test that runs or reads that block names. Its
// migration adds columns to tables older releases' ways back drop (plan_tasks, step_starts,
// pending_task_deliveries), so a rollback past it to any of them runs this way back first.
const FixBesideSteps = 20261127131056

// RunningFixSQL is the number of the fix of an issue's task that runs at the issue's generation, 0
// for none, as one expression over a row of issues: a row of task_fixes in state running
// (20261127131056_fix_beside_steps). A fix that still reads its failed job is none yet, since its
// entry is not raised. Package record's issue read selects it as record.Issue.RunningFix, and a
// read of package store over issues reads it by the same text. It lives beside the schema it reads
// because record and store cannot import each other and both import this package. A generation
// holds one task (plan.Supported) and a task one fix in state running (task_fixes_one_open), so the
// subquery answers one row at most.
const RunningFixSQL = `coalesce((select f.fix from task_fixes f where f.issue = issues.key and f.generation = issues.generation and f.state = 'running'), 0)`
