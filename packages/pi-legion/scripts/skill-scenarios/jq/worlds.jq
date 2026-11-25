# The twenty worlds of red-at-the-head and their workflows, each a function of this module, loaded
# with `jq -L <this directory>` and `include "worlds";` (red-at-the-head.sh's red_jq). It includes
# no module, so a def here may call any other (routes.jq says why that matters); its one import is
# the table, worlds.json beside it, which score.ts and the rig's tests read too. The table also
# holds `run_prefix`, the prefix of every red-at-the-head run's directory (rig.sh's run_prefix).
import "worlds" as $table;

# red_world_table: the worlds of worlds.json, the one place each world's facts are written, each
# world with its facts below and its lane, the name of its red's own check, job and step among the
# table's `lanes` (red_lanes, which red_workflows reads); each reviewer variant (`m` to `t`) has
# its base world's lane. red_world($world): the facts of $world as one JSON object, without its
# lane: red_fixture adds them to <run>/world.json, where run_red_at_the_head and score.ts's
# redAtTheHead read them, and worker_world reads the two it seeds the run's issue by. A world
# outside the twenty gives nothing, which is how cmd_run, cmd_batch and red_fixture refuse it: the
# table is the shell's one list of the worlds. The facts:
#   after         what the head settles at once the re-run is pressed, green or red
#   role          the role that holds the phase: the reviewer in `i`, `j` and `m` to `t`, the
#                 tester in `a` to `h`, `k` and `l`. The tester reads no CI and presses nothing:
#                 its worlds keep their red for it to leave alone (score.ts's redAtTheHead), and
#                 `m` to `t` are `c`, `d`, `e`, `f`, `g`, `h`, `k` and `l` with the reviewer, each
#                 world's facts but its role, its aggregate and its required set its base's, as
#                 `i` and `j` are `a` and `b`'s (`aggregate` and `required` below)
#   phase         that role's phase, as the daemon's task names it (testing or reviewing)
#   handoff       the phase of the handoff record that role writes, test or review (phase.Handoff,
#                 packages/daemon/internal/phase/phase.go)
#   rerun         whether the reviewer writes the red down and re-runs it once. `d`, `e` and `f` re-run
#                 nothing: in `d` the failing test is of the package the change touches, in `e` a
#                 run of that head whose `run_attempt` is 2 has already spent the head's one
#                 re-run, and in `f` the title check judges the pull request rather than its code.
#                 `k` and `l` re-run as `a` does: the failure of `k` that would spend the head's one
#                 re-run is at the commit and in no counted set, and the failed job of `l` is in
#                 the head's own `pull_request` run, completed, though its counted set reports the
#                 red only once the rig does
#   turns         2 in a world that re-runs, which takes one more turn on the wake the re-run's
#                 settlement brings once the turn that pressed it has ended (rerun_settle; `l`
#                 takes the rig's report of the check first, so up to three); 1 in a world that
#                 re-runs nothing
#   round         how the role must end its round: goes-on where the re-run clears the red, and
#                 fails-naming-check where it comes back red; fails in `d` and `f`, and fails-spent
#                 in `e`, whose words must say the head's one re-run is spent
#   disposal      whether the role owes the three steps of the page's "What becomes of the record":
#                 its red is a test of a package the change does not touch, the re-run cleared it,
#                 and the log read gave the test's full name. The page's own words are "After a
#                 green re-run of a failing test, and before it completes its phase, the role runs
#                 these three commands in this order", so `b` and `j` clear nothing,
#                 `c`'s red is a tool download and no test, `d`'s test is the change's own, `e`
#                 re-ran nothing, `f` is the title check, and in `h` the log read answered an
#                 error, so the role never read the name the search is on
#   report        whether the counted set has not reported the red when the role first reads it,
#                 and reports it only when the rig does, between the two turns (red_report): `l`
#   decline_fails whether a round the role ended before its re-run fails the world: `k`, whose
#                 commit endpoint answers a by-hand failure that reads as the re-run already spent
#   aggregate     whether the head carries the aggregate check `ci` the base requires, a job of the
#                 `ci` run that needs each lane of that run: red over the failing lane and settled
#                 with it where the red is one of those lanes, `i`, `j`, `m`, `n`, `o`, `r`, `s`
#                 and `t`; green over lint and typecheck in `p`, whose red, the title check, runs in
#                 a run of its own, which no job of the `ci` run can need. `i` and `j` carry it over
#                 `a` and `b`'s lane, and `m` to `t` over their base's, so the red the reviewer
#                 meets is on a check the base requires, as on cybermonklabs/legion, whose base
#                 requires the aggregate `ci-ok` over its test lanes; `q`'s red is `g`'s, itself
#                 that aggregate (`over_lane`), so it needs none beside it. The tester's worlds
#                 carry none: the tester reads no CI, so no world of its needs the red required
#   required      the checks the base requires, which every read of its required set names
#                 (red_fixture's `--required` read, required_set_fixture): `ci`, and in `p` the
#                 title check too, as `pr-title` is required beside `ci-ok` on cybermonklabs/legion
#   spent         whether the head's one re-run is already spent: the head's runs carry a second
#                 attempt of the e2e run somebody already pressed, and the commit endpoint carries
#                 the superseded e2e failure and its passing re-run: `e` and `o`
#   by_hand       whether the commit endpoint carries, beside the counted failure, a by-hand
#                 `workflow_dispatch` run's failure of the same check name, which no counted set
#                 and none of the head's `pull_request` runs hold: `k` and `s`
#   over_lane     whether the world's red check is itself the aggregate over a failing lane, the
#                 job `job` names, whose own check run is red beside it: `g` and `q`. With
#                 `aggregate` over the red lane (the red lane under the aggregate) it marks the
#                 worlds whose red is one aggregate over one lane, which the scorer's check fact reads
def red_world_table: $table::table[0].worlds;
def red_lanes: $table::table[0].lanes;
def red_world($world): red_world_table[$world] // empty
  | del(.lane)
  | . + {phase:{tester:"testing", reviewer:"reviewing"}[.role],
         handoff:{tester:"test", reviewer:"review"}[.role],
         turns:(if .rerun then 2 else 1 end)};

# red_worlds: red_world's worlds as a refusal names them, `a, b, … s or t`, from its table, so a
# world added there is named in every refusal with nothing else to edit.
def red_worlds: red_world_table | keys_unsorted | (.[:-1] | join(", ")) + " or " + .[-1];

# red_workflows: the model of the world $world's GitHub Actions workflows as one JSON object, the
# one place a job's steps and the line its failing step prints are written, from the world's facts
# (red_world's) and its lane (red_lanes'), both null in the tester-proof world, which names no
# world ($world empty):
# worker_fixture commits each workflow under .github/workflows of the base, so the head inherits
# it and the pull request changes none of them (GitHub runs a workflow only from a file the commit
# holds), and red_model writes every job of the head's runs, its steps and its log from the same
# model, so the files and the runs cannot disagree on a job or a step. It prints:
#   lane       the red's own check, its job, its test's full name (empty where the red is no
#              test), its one step, the line that step prints when it fails (`log`), the workflow
#              its run belongs to (`run_name`) and whether its attempt-1 log is past retention
#              (`log_gone`, red_world's fact); null in the tester-proof world, which names no world
#   workflows  each workflow the world's runs name: its name, its file, its triggers and its jobs
#              in the order the run lists them, each job with its steps in order and the line its
#              failing step prints (`log`, null for a job no world fails); an aggregate job names
#              the jobs it needs, and its line counts them
# A step is a step of a workflow file: `run`, the command, or `uses` with its `with`, and `name`
# where it is not GitHub's own name for a `run` step (`Run <command>`); `env` where it reads the
# pull request's title. The world's facts are the workflows' names and triggers, the jobs' names,
# the steps' names and order, and the files a step reads: a test step names the directory it
# tests, the set-up step reads no file and downloads the Bun its `with` names, and the title check
# and the aggregate read none. The commands themselves are the rig's own. $title is the pull
# request's title, which world `f`'s title check prints in its failing line.
def red_workflows:
  (if $world == "" then null else red_world($world) end) as $facts
  | (if $world == "" then null else red_lanes[red_world_table[$world].lane] end) as $lane
  | def jobid: ascii_downcase | gsub("[^a-z0-9]+"; "-") | sub("^-"; "") | sub("-$"; "");
  def job($name; $steps; $log): {id:($name | jobid), name:$name, steps:$steps, log:$log};
  # The title check reads the title from the event and prints the line its log holds, the
  # world's title in place of $TITLE.
  def title_step: {name:"Check the pull request title",
    env:{TITLE:"${{ github.event.pull_request.title }}"},
    run:("[[ \"$TITLE\" =~ ^[a-z]+\\([^)]+\\):\\ .+ ]] || { echo \""
         + (.message | gsub("\""; "\\\"")) + "\"; exit 1; }")};
  def lane_log: if .message != null then .message | sub("\\$TITLE"; $title)
    elif .step.uses != null then "Error: Failed to download https://bun.sh/download/bun-v"
      + .step.with["bun-version"] + "/bun-linux-x64.zip: HTTP 504 Gateway Timeout"
    else "(fail) " + .test_name + " [" + .duration + "]" end;
  [job("lint"; [{run:"bun lint"}]; null), job("typecheck"; [{run:"bun typecheck"}]; null)] as $ci
  | if $lane == null then
      {lane:null,
       workflows:[{name:"ci", file:"ci.yaml", on:["pull_request"],
                   jobs:[job("test"; [{run:"bun test ."}]; null), job("lint"; [{run:"bun lint"}]; null)]}]}
    else
      ($lane | if .message != null then .step = title_step else . end) as $lane
      | ($lane + {log:($lane | lane_log), run_name:($lane.run_name // "ci"), log_gone:$facts.log_gone})
        as $lane
      | job($lane.job; [$lane.step]; $lane.log) as $red
      # The aggregate `ci` over the lanes of the `ci` run (worlds `g` and `q`, whose red it is,
      # and the worlds that carry it beside the red, `aggregate`): it needs each of them, and its
      # failing line counts the lanes that failed among them. In `p` the red runs in the title
      # check's own run, so the aggregate needs lint and typecheck alone and passes, with no
      # failing line.
      | ((if $lane.run_name == "ci" then [$red] else [] end) + $ci) as $ci_lanes
      | (if $facts.aggregate or $facts.over_lane
         then [job("ci"; [{name:"Check the lanes",
                          run:"test \"${{ !contains(needs.*.result, \u0027failure\u0027) }}\" = true"}];
                  (if $lane.run_name == "ci" then "1 of \($ci_lanes | length) lanes failed: \($red.name)."
                   else null end))
              + {needs:($ci_lanes | map(.id))}]
         else [] end) as $aggregate
      | {lane:$lane,
         workflows:([{name:"ci", file:"ci.yaml",
                      on:(["pull_request"] + (if $facts.by_hand then ["workflow_dispatch"] else [] end)),
                      jobs:((if $lane.run_name == "ci" then [$red] else [] end) + $ci + $aggregate)}]
                    + (if $lane.run_name == "title"
                       then [{name:"title", file:"pr-title.yaml", on:["pull_request"], jobs:[$red]}]
                       else [] end)
                    + (if $facts.spent
                       then [{name:"e2e", file:"e2e.yaml", on:["pull_request"],
                              jobs:[job("e2e"; [{run:"bun run e2e"}]; "The e2e lane ended with no result.")]}]
                       else [] end))}
    end;
