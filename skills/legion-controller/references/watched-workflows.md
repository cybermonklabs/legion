# Watched workflows

Read this on a delivery on a `default_branch.workflow` topic, on a `legion:` notice about a watched workflow, and at every start. Your system prompt's `Watched workflows:` line names each entry as `<owner>/<repo>/<file>`, with the events whose runs count. It is the list of this launch, and no notice or message changes it. With no such line and no such topic delivered, nothing here applies.

## Which runs count

1. Your session hears the topics of each entry by itself while it holds the controller role. Never `envoy_subscribe` such a topic: a relaunch drops a topic that you subscribe.
2. A delivery on `notifications.github.<owner>.<repo>.default_branch.workflow.<file>.completed.<event>` is a completed run of a watched workflow on its repository's default branch, whether or not the prompt names its entry: the plugin subscribes no topic that the daemon did not name for this controller. The topic names the run's event. Its payload gives `repo` (`<owner>/<repo>`, as GitHub spells it), `run_id`, `url`, `conclusion`, `event`, `path`, `branch` (the default branch), `run_started_at` and `updated_at` (the run's end). Take `<owner>/<repo>` for every step below from the payload's `repo`, never from the topic: the topic writes each `.` of the owner and the repository as `_` as well (`acme/my.repo` is `…github.acme.my_repo…`). The topic also writes each `.` of a file name as `_`, so two file names can share one topic (`a.b.yaml` and `a_b.yaml`): when the prompt names a file whose topic is the delivery's topic, the delivery's `path` must end in `/<file>` of one of those files, and a delivery whose `path` names none of them is a run of another file: drop it. Its entry's file is the one its `path` ends in.
3. A run counts when its `conclusion` is `failure`, `timed_out`, `startup_failure` or `cancelled`. No other conclusion counts.
4. A `cancelled` run does not count only when a newer run of the same workflow on the default branch, from an event the entry names (for an entry the prompt does not name, the cancelled run's own event), started before the cancelled run ended. Read the runs:

   ```text
   gh run list --repo '<owner>/<repo>' --workflow '<file>' --branch '<default branch>' --limit 20 --json databaseId,event,status,conclusion,startedAt,updatedAt,url
   ```

   A newer run has a larger `databaseId` than the cancelled run, an `event` the entry names, and a `startedAt` before the cancelled run's `updatedAt`. When such a newer run exists, that newer run's end decides, not the cancelled run. When the newer run's `status` is `completed`, take the newer run by steps 3 to 7. When it did not end yet, take nothing now: its own delivery decides. Every other `cancelled` run counts. A `cancelled` run counts when a run on another branch took its place.

## Route a counted run

A counted run is a clear defect.

5. Before you route a counted run, search Dispatch for its run id: `dispatch search --query '<run id>'`. The search ranks loosely, so read each hit: when an issue or a comment names the run already, as `run <run id>` or by its URL, do not route it again. Steps 6 and 7 write `run <run id>` as words of their own for this search.
6. Search for an open issue of the workflow: `dispatch search --query '<owner>/<repo>/<file>'`. When an open issue tracks a defect of the workflow, comment `<owner>/<repo>/<file> ended <conclusion> again: run <run id>, <run URL>` on it (`dispatch comment --issue '<KEY>' --body-file -`). When a live tree owns that issue (its record's `tree` in `read_state`), add `--mention 'role:<that tree's architect's token>'`, built as `skill://legion-controller/references/operations.md#a-request-to-another-session` says, so the comment reaches it. File nothing new.
7. Otherwise, route the run as `skill://legion-controller/references/finding-asks.md#file-a-clear-defect` routes a clear defect that you find yourself. Its evidence is `run <run id>, <run URL>`. The issue that you file has a title that names `<owner>/<repo>/<file>`. Its spec ends with `Filed for Legion from controller on <report issue>: run <run id>, <run URL>`.

## At every start

8. Read each repository that the prompt's entries name, once, for its default branch and GitHub's spelling of its name (entries whose `<owner>/<repo>` differ only in case name one repository):

   ```text
   gh api 'repos/<owner>/<repo>' --jq '[.full_name, .default_branch]'
   ```

   Compare its `full_name` with the `<owner>/<repo>` of each entry of that repository. When they differ (a difference of case counts), name that entry in your summary: its topic never matches a run. When the read fails, step 9 applies to each entry of that repository. Then, for each entry and each event of the entry, read its newest completed run on its repository's default branch:

   ```text
   gh run list --repo '<owner>/<repo>' --workflow '<file>' --branch '<default branch>' --event '<event>' --status completed --limit 1 --json databaseId,conclusion,event,headBranch,startedAt,updatedAt,url
   ```

   Its `databaseId` is the run id, and its `updatedAt` is the run's end. When that run counts by step 3, take it by steps 4 to 7, as a delivered run.
9. When a `gh` read of step 4, 8 or 10 fails, name the entry and `gh`'s first line in your summary. Act on nothing that read would decide. When a value for a placeholder of those commands holds `'` (a branch name can), run nothing with it: name the entry and the value in your summary.

## After a late subscription

10. When a `legion:` notice says that Envoy answered the subscription of a watched entry's runs of one event at last, after a 503, read that entry and event now as step 8 reads it, if the prompt's list names them: a run that ended while the topic waited is not delivered after the topic lands. Take a run that counts by steps 4 to 7.
