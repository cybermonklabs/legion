steps-answer.json: the step runs of one issue of a Legion deployment's Dispatch, as its steps route answered them (the answer `dispatch steps --issue <issue>` reads), read on 2026-11-27 with:

  printf 'Authorization: Bearer %s' "$(cat "$DISPATCH_TOKEN_FILE")" | curl -fsS -H @- "$DISPATCH_URL/api/v1/issues/<issue>/steps"

and kept with:

  jq 'walk(if type == "string"
           then gsub("<PROJECT>-<n>"; "ACME-12") | gsub("<project>-<n>"; "acme-12")
                | gsub("<PROJECT>"; "ACME") | gsub("<project>"; "acme")
           else . end)'

The issue's default task holds 13 runs: the planner's, three rounds of the implementer's, the tester's
and the reviewer's (the reviewer's first two failed and returned the work to step 2), the acceptance
review, the retro with the rating it gave the plan, and the merge. seed.test.ts holds the steps route of
its fake Dispatch to the fields of each run here, in this order.

Replacements: the issue's key, in upper case and in lower case (the issue of the answer and of each run,
and the key inside each run's agent name, `legion-<project>-<project>-<n>-<role>`), is ACME-12 and
acme-12, and the project's key elsewhere (the agent names' project) is ACME and acme. Nothing else was
changed: the run ids, session ids, report ids, models, times, results and the retro's rating and reason
are as Dispatch answered them. The answer holds no host, machine, cwd or session_title.
