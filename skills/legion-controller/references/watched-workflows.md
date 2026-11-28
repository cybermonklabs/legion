# Watched workflows

Read this at every start and on each delivered run of a watched workflow. Your system prompt's `Watched workflows:` line names each entry as `<owner>/<repo>/<file>`, with the events whose runs count. It is the list of this launch, and no notice or message changes it. With no such line and no such run delivered, nothing here applies.

## Which runs count

A delivery on a watched workflow's topic is a completed run on its repository's default branch. It is a watched run whether or not the prompt names its entry, and step 1 says whether it counts.

1. A run counts when its `conclusion` is `failure`, `timed_out`, `startup_failure` or `cancelled`. No other conclusion counts.
2. A `cancelled` run does not count only when a newer run of the same workflow on the default branch, from an event the entry names (for an entry the prompt does not name, the cancelled run's own event), started before the cancelled run ended. Read the runs:

   ```text
   gh run list --repo '<owner>/<repo>' --workflow '<file>' --branch '<default branch>' --limit 20 --json databaseId,event,status,conclusion,startedAt,updatedAt,url
   ```

   A newer run has a larger `databaseId` than the cancelled run, an `event` the entry names, and a `startedAt` before the cancelled run's `updatedAt`. When such a newer run exists, that newer run's end decides, not the cancelled run. When the newer run's `status` is `completed`, take the newer run by steps 1 to 5. When it did not end yet, take nothing now: its own delivery decides. Every other `cancelled` run counts. A `cancelled` run counts when a run on another branch took its place.

## Route a counted run

A counted run is a clear defect.

3. Before you route a counted run, search Dispatch for its run id: `dispatch search --query '<run id>'`. The search ranks loosely, so read each hit: when an issue or a comment names the run already, as `run <run id>` or by its URL, do not route it again. Steps 4 and 5 write `run <run id>` as words of their own for this search.
4. Search for an open issue of the workflow: `dispatch search --query '<owner>/<repo>/<file>'`. When an open issue tracks a defect of the workflow, comment `<owner>/<repo>/<file> ended <conclusion> again: run <run id>, <run URL>` on it (`dispatch comment --issue '<KEY>' --body-file -`). When a live tree owns that issue (its record's `tree` in `read_state`), add `--mention 'role:<that tree's architect's token>'`, built as `skill://legion-controller/references/operations.md#a-request-to-another-session` says, so the comment reaches it. File nothing new.
5. Otherwise, route the run as `skill://legion-controller/references/finding-asks.md#file-a-clear-defect` routes a clear defect that you find yourself. Its evidence is `run <run id>, <run URL>`. The issue that you file has a title that names `<owner>/<repo>/<file>`. Its spec ends with `Filed for Legion from controller on <report issue>: run <run id>, <run URL>`.

## At every start

6. Read each repository that the prompt's entries name, once, for its default branch and GitHub's spelling of its name (entries whose `<owner>/<repo>` differ only in case name one repository):

   ```text
   gh api 'repos/<owner>/<repo>' --jq '[.full_name, .default_branch]'
   ```

   Compare its `full_name` with the `<owner>/<repo>` of each entry of that repository. When they differ (a difference of case counts), name that entry in your summary: its topic never matches a run. When the read fails, step 7 applies to each entry of that repository. Then, for each entry and each event of the entry, read its newest completed run on its repository's default branch:

   ```text
   gh run list --repo '<owner>/<repo>' --workflow '<file>' --branch '<default branch>' --event '<event>' --status completed --limit 1 --json databaseId,conclusion,event,headBranch,startedAt,updatedAt,url
   ```

   Its `databaseId` is the run id, and its `updatedAt` is the run's end. When that run counts by step 1, take it by steps 2 to 5, as a delivered run. A `legion:` notice that Envoy took a watched topic after a 503 asks for this read for one entry and event: make it with the command above, only when the prompt's line names that entry with that event.
7. When a `gh` read of step 2 or 6 fails, name the entry and `gh`'s first line in your summary. Act on nothing that read would decide. When a value for a placeholder of those commands holds `'` (a branch name can), run nothing with it: name the entry and the value in your summary.
