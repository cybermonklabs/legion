// Package plansql is the SQL of the two roles a read of an issue takes from the plan rows of the
// issue's generation: the role of the step its task stands at (record.Issue.StepRole) and the role
// of the claim that works its task's place (record.Issue.PlaceRole). The record's read of an issue
// (record's issueColumns), which the cancel of a pod's release while the project is paused reads
// through the daemon (store's ReleaseCancelPause.BackstopTarget), and the idle view of its pod
// (store's readIdleView) both select them, so the reads give one answer. It is a package of its
// own, under internal/store, because store and record import neither each other, and it imports
// nothing, so both can.
//
// Each is a subquery over a row of issues the caller names by its alias. A run works one task at
// a time in this release, so each reads only a task at a step or at its retro and finds one row at
// most, also in a stored plan of more tasks than this release walks (record.StoredPlan.Task).
package plansql

// StepRole is the subquery of the role of the step the task of the generation of alias, a row of
// issues, stands at, "" where its plan rows hold none or the task stands at no step.
func StepRole(alias string) string {
	return `coalesce((select s.role from plan_tasks t join plan_steps s on s.issue = t.issue and s.generation = t.generation and s.task = t.task and s.position = t.position ` +
		`where t.issue = ` + alias + `.key and t.generation = ` + alias + `.generation and t.running_kind = 'step'), '')`
}

// PlaceRole is the subquery of the role of the claim that works the place the task of the
// generation of alias, a row of issues, stands at: at a step, the architect where its key is owner
// and its role otherwise; at the retro, the same of the task's last step of the type
// implementation, and the architect where it has none; "" otherwise. It is the rule of
// record.PlaceWorker (Worker.ClaimRole), which the workflow's walk reads (Walk.PlaceRole) and a
// test holds this to.
func PlaceRole(alias string) string {
	return `coalesce((select case t.running_kind ` +
		`when 'step' then (select case when s.agent_key = 'owner' then 'architect' else s.role end from plan_steps s ` +
		`where s.issue = t.issue and s.generation = t.generation and s.task = t.task and s.position = t.position) ` +
		`when 'retro' then coalesce((select case when s.agent_key = 'owner' then 'architect' else s.role end from plan_steps s ` +
		`where s.issue = t.issue and s.generation = t.generation and s.task = t.task and s.type = 'implementation' order by s.position desc limit 1), 'architect') ` +
		`end from plan_tasks t where t.issue = ` + alias + `.key and t.generation = ` + alias + `.generation and t.running_kind in ('step', 'retro')), '')`
}
