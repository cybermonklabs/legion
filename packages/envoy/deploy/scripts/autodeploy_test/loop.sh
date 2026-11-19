# shellcheck shell=bash disable=SC2154,SC2034 # fixture.sh, which autodeploy_test.sh sources first, sets these and reads deployer_prefix
# Case group of autodeploy_test.sh: the commit order, the record of failed deploys, the continuous
# loop, the settings and the end of a --once run. It runs on the stand-ins of fixture.sh, which
# autodeploy_test.sh sources first. Every order read is the real jj's, against the fixture's
# repository of three commits: old-image, its child new-image, and its grandchild newer-image.

# The deployer never deploys a commit older than the deployed one. The registry answers that it
# holds no image of the running commit (it was removed, or IMAGE names another repository), so the
# walk reaches its parent, which is published: the repository's history says that commit is an
# ancestor of the deployed one, and the deployer names both and deploys nothing.
case_begin 'an older commit over the running one'
fresh_env
deployed_tag_is new-image
printf 'new-image unknown\n' >"$manifests"
if ! output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once); then
  fail 'deployer failed a pass whose newest published commit is older than the deployed one'
fi
if [[ "$output" != *"the newest published main commit, old-image, is an ancestor of the deployed new-image; deploying nothing older over it"* ]] ||
  [[ -s "$log" ]] || [[ "$output" == *"newer published image"* ]]; then
  fail 'deployer deployed a commit older than the deployed one, or did not name both'
fi
# An order jj does not answer deploys nothing: a deploy needs the answer that the commit is newer,
# not the absence of the answer that it is older.
case_begin 'an order jj does not answer'
fresh_env
if output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' AUTODEPLOY_TEST_ORDER_FAILS=1 deploy_once); then
  fail "deployer passed a pass whose commits' order it could not read"
fi
if [[ "$output" != *"could not read from $repo whether new-image is older than the deployed old-image; not deploying"* ]] || [[ -s "$log" ]]; then
  fail "deployer deployed a commit whose order against the deployed one it could not read"
fi
# The control: a newer commit, the deployed one's child, still deploys (to the origin check's
# rollback here), and so does one over a deployed tag the repository does not know, and over one
# that holds a quote and a backslash, which reach jj inside its string literals and not as revset.
for deployed in old-image local 'by"hand\tag'; do
  case_begin "a newer commit over the deployed $deployed"
  fresh_env
  deployed_tag_is "${deployed//\\/\\\\}"
  if output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once); then
    fail "deployer did not fail at the origin check of a commit newer than $deployed"
  fi
  if [[ "$output" != *"main has a newer published image: new-image (deployed $deployed)"*"rolling back to $deployed"* ]]; then
    fail "deployer did not deploy a commit newer than the deployed $deployed"
  fi
done

# A deploy that started its image and failed is recorded, with the backup taken before it, and the
# loop goes on: a rollback that restored the previous image fails the pass alone. The next passes
# deploy that commit no more, each naming it and the record, while it is the newest published one.
# Here new-image fails its origin check, is rolled back, and the loop then passes over it twice
# before the case stops it at its third wait. (POLL is 11, a wait no step of a pass makes.)
case_begin 'the loop after a deploy that was rolled back'
fresh_env
rm -f "$backups"/pre-*.dump
status=0
output=$(POLL=11 AUTODEPLOY_TEST_LOOP_PASSES=3 AUTODEPLOY_TEST_COMMITS='new-image old-image' deployer) || status=$?
if ((status != 143)) || [[ $(wc -l <"$schema.passes") -ne 3 ]]; then
  fail "the continuous loop ended with status $status after a deploy that was rolled back, before the case stopped it at its third wait"
fi
is_exactly 'the deploys of new-image' 1 "$(grep -c 'main has a newer published image: new-image' <<<"$output")"
is_exactly 'the compose ups (the deploy and its rollback)' 2 "$(grep -c ' up ' "$log")"
is_exactly 'the passes that passed over new-image' 2 \
  "$(grep -cF "new-image failed its deploy before, as $failed_record records: deploying only a commit newer than it (delete $failed_record to deploy it again)" <<<"$output")"
dump=$(compgen -G "$backups/pre-new-image-*.dump") || fail 'the deploy took no backup'
is_exactly 'the record of the failed deploy' "new-image rolled-back $dump" "$(<"$failed_record")"

# A newer published commit deploys over a recorded failure, and a deploy that succeeds removes the
# record, so the backups it kept may be pruned again.
case_begin 'a newer commit after a failed one'
fresh_env
printf 'new-image rolled-back %s\n' "$backups/pre-new-image-recorded.dump" >"$failed_record"
if ! output=$(AUTODEPLOY_TEST_ORIGIN_OK=1 AUTODEPLOY_TEST_COMMITS='newer-image new-image old-image' deploy_once); then
  fail 'deployer did not deploy a commit newer than the one whose deploy failed'
fi
if [[ "$output" != *"deployed newer-image"*"removed $failed_record"* ]] || [[ -e "$failed_record" ]] ||
  [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=newer-image"* ]]; then
  fail 'deployer did not deploy newer-image over the failed new-image, or kept the record after it succeeded'
fi
# An ancestor of a commit whose deploy failed is no newer commit: it is passed over too.
case_begin 'an ancestor of a failed commit'
fresh_env
printf 'newer-image rolled-back %s\n' "$backups/pre-newer-image-recorded.dump" >"$failed_record"
if ! output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once); then
  fail 'deployer failed a pass whose newest commit is an ancestor of a failed one'
fi
if [[ "$output" != *"new-image is an ancestor of newer-image, whose deploy failed, as $failed_record records: deploying only a commit newer than it"* ]] || [[ -s "$log" ]]; then
  fail 'deployer deployed an ancestor of a commit whose deploy failed, or did not name both'
fi

# A refused rollback stops the loop: the pass exits 3, naming why, and the loop ends with 3 and
# one line saying forward recovery is required, as at main, starting nothing again. A deployer
# started again after that (a supervisor's restart) deploys nothing while the record says the
# deployer stopped, and neither does a single run.
case_begin 'the loop after a refused rollback'
fresh_env
status=0
output=$(POLL=11 AUTODEPLOY_TEST_LOOP_PASSES=3 AUTODEPLOY_TEST_MIGRATE_TO=113 AUTODEPLOY_TEST_COMMITS='new-image old-image' deployer) || status=$?
if ((status != 3)) || [[ -e "$schema.passes" ]]; then
  fail "the continuous loop ended with status $status after $(cat "$schema.passes" 2>/dev/null | wc -l) waits, where a refused rollback must stop it at once with 3"
fi
if [[ "$output" != *"refusing to roll back to old-image"*"the pass ended with status 3 after a deploy of new-image whose rollback did not restore the previous image, and forward recovery is required (the line above names why; $failed_record records the deploy): the deployer stops, and deploys nothing until a person deletes $failed_record"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]] || [[ $(cut -d' ' -f1,2 "$failed_record") != 'new-image stopped' ]]; then
  fail 'the continuous loop did not stop at a refused rollback with its named line, or started new-image again'
fi
case_begin 'a deployer started again after a refused rollback'
: >"$log"
: >"$manifest_log"
status=0
output=$(POLL=11 AUTODEPLOY_TEST_LOOP_PASSES=3 AUTODEPLOY_TEST_COMMITS='new-image old-image' deployer) || status=$?
if ((status != 3)) || [[ -s "$log" || -s "$manifest_log" ]] ||
  [[ "$output" != *"$failed_record records that the deploy of new-image left Dispatch for a person, and forward recovery is required: deploying nothing until a person deletes $failed_record"*"the deployer stops"* ]]; then
  fail "a deployer started again after a refused rollback ended with status $status, or read or started anything"
fi
status=0
output=$(deploy_once) || status=$?
if ((status != 3)) || [[ -s "$log" ]]; then
  fail "a single run after a refused rollback ended with status $status, or started anything"
fi

# The backup taken before a deploy that failed is kept while the record names it, past the ten
# newest the prune keeps: it is what a person recovering from that deploy may need. Here it is the
# oldest of eleven backups, and a deploy of newer-image adds a twelfth: the prune removes the
# oldest backup the record does not name, and keeps the recorded one, so eleven stay. The control,
# the same backups with no record, prunes the two oldest, the recorded one among them, and keeps
# ten.
for recorded in yes no; do
  case_begin "the prune of backups, the failed deploy recorded: $recorded"
  fresh_env
  rm -f "$backups"/pre-*.dump
  touch -d '-3 hours' "$backups/pre-new-image-kept.dump"
  for i in $(seq -w 1 10); do
    touch -d "-$((120 - 10#$i)) minutes" "$backups/pre-other-$i.dump"
  done
  if [[ "$recorded" == yes ]]; then
    printf 'new-image rolled-back %s\n' "$backups/pre-new-image-kept.dump" >"$failed_record"
  fi
  if output=$(AUTODEPLOY_TEST_COMMITS='newer-image new-image old-image' deploy_once); then
    fail 'deployer did not fail at the origin check of newer-image'
  fi
  left=$(find "$backups" -maxdepth 1 -name 'pre-*.dump' | wc -l)
  if [[ -e "$backups/pre-other-01.dump" ]]; then
    fail 'the prune did not remove the oldest backup no record names'
  fi
  if [[ "$recorded" == yes ]] && [[ ! -e "$backups/pre-new-image-kept.dump" || "$left" -ne 11 ]]; then
    fail "the prune removed the backup taken before a deploy that failed, which the record names, or left $left backups, not 11"
  fi
  if [[ "$recorded" == no ]] && [[ -e "$backups/pre-new-image-kept.dump" || "$left" -ne 10 ]]; then
    fail "the control: the prune kept the oldest backup, which no record names, or left $left backups, not 10, so the case proves nothing"
  fi
done

# The continuous loop survives a failed pass that started nothing, whatever failed: the pass logs
# its named line and ends, the loop says so and runs the next pass after POLL, and that pass
# deploys once the registry answers. Here the first read is refused, or the first pull fails, or
# the first pass meets a clock that answers `300s`, and the next pass deploys new-image. That last
# one is why each pass runs as a process of its own: in the loop's own shell an error of bash's
# would abort the whole loop, and the deployer would exit. The loop is still running at its third
# wait, where the case stops it. A single run of the same failed pass exits 1 (the refused read,
# the failed pull and the once-ends case below).
for failure in refused-read AUTODEPLOY_TEST_PULL_FAIL_ONCE garbled-clock; do
  case_begin "the loop after a failed pass that started nothing ($failure)"
  fresh_env
  once=AUTODEPLOY_TEST_NONE
  if [[ "$failure" == refused-read ]]; then
    printf 'new-image refused-once\n' >"$manifests"
  elif [[ "$failure" == garbled-clock ]]; then
    printf 'garbled-once\n' >"$clock"
  else
    once=$failure
  fi
  status=0
  output=$(export "$once=1" && POLL=11 AUTODEPLOY_TEST_LOOP_PASSES=3 AUTODEPLOY_TEST_ORIGIN_OK=1 \
    AUTODEPLOY_TEST_COMMITS='new-image old-image' deployer) || status=$?
  if ((status != 143)) || [[ $(wc -l <"$schema.passes") -ne 3 ]]; then
    fail "the continuous loop ended with status $status after a failed pass ($failure), before the case stopped it at its third wait"
  fi
  if [[ "$output" != *"the pass failed (exit 1); the next pass starts in 11s"*"deployed new-image"* ]] ||
    [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=new-image"* ]]; then
    fail "the continuous loop did not run the next pass after a failed one ($failure) and deploy new-image"
  fi
done

# A clock that answers no time at all is named, with what could not start without it, and the pass
# stops: before the pass, and inside a rollback, after the candidate was stopped, where the pass
# then stops the deployer, since what runs is not known.
case_begin 'a clock that fails before the pass'
fresh_env
printf 'broken\n' >"$clock"
status=0
output=$(deploy_once) || status=$?
if ((status != 1)) || [[ "$output" != *"the clock, date +%s, answered '', which is no time in seconds, before the pass could start"* ]] ||
  [[ -s "$manifest_log" ]]; then
  fail "a clock that fails before the pass was not named, or the pass went on (status $status)"
fi
case_begin 'a clock that fails inside the rollback'
fresh_env
status=0
output=$(AUTODEPLOY_TEST_CLOCK_BREAKS_ON_STOP=1 deploy_once) || status=$?
if ((status != 3)) ||
  [[ "$output" != *"the clock, date +%s, answered '', which is no time in seconds, before docker compose ps could confirm new-image stopped, so refusing to roll back to old-image"* ]] ||
  [[ $(cut -d' ' -f1,2 "$failed_record") != 'new-image stopped' ]]; then
  fail "a clock that fails inside the rollback was not named with the call it stopped, or did not stop the deployer (status $status)"
fi

# A setting of seconds with a unit, or that is no whole number above 0, is refused at the start by
# name, before any pass reads anything, where bash would read it as an arithmetic error; and a
# pass deadline that leaves a rollback no time is refused naming both numbers.
for setting in PASS_DEADLINE=3600s POLL=5m SCHEMA_READ_DEADLINE=1.5 PASS_DEADLINE=0; do
  case_begin "a setting refused at start ($setting)"
  fresh_env
  status=0
  output=$(export "${setting?}" && deploy_once) || status=$?
  if ((status != 2)) || [[ "$output" != "${setting%%=*} is ${setting#*=}, which is no whole number of seconds"* ]] ||
    [[ -s "$manifest_log" || -s "$log" ]]; then
    fail "deployer did not refuse $setting at its start, by name, before reading anything"
  fi
done
case_begin 'a pass deadline below the least'
fresh_env
status=0
output=$(PASS_DEADLINE=1559 deploy_once) || status=$?
if ((status != 2)) || [[ "$output" != "PASS_DEADLINE is 1559s, below the 1560s a pass needs: 1380s for its deploy steps (the pull 900s, the up 300s, the health check 180s) and 180s for its rollback's health check; not starting" ]] ||
  [[ -s "$manifest_log" ]]; then
  fail 'deployer did not refuse a pass deadline that leaves its rollback no time, naming both numbers'
fi
# The control: the least pass deadline the deployer takes starts a pass.
case_begin 'the least pass deadline'
fresh_env
if output=$(PASS_DEADLINE=1560 deploy_once); then
  fail 'deployer did not fail at the origin check under PASS_DEADLINE=1560'
fi
if [[ "$output" != *"rolling back to old-image"* ]]; then
  fail 'deployer refused the least pass deadline it takes'
fi
# The least pass deadline is computed from the health check's own numbers, not written beside them:
# a copy of the deployer whose health check waits 4 s between tries asks 30 s more of the pass, both
# for its own health check and for its rollback's, so it refuses 1560 and names 1620.
case_begin 'the least pass deadline follows the health check'
fresh_env
sed 's/^HEALTH_INTERVAL=3$/HEALTH_INTERVAL=4/' "$deployer_script" >"$root/autodeploy-interval-4.sh"
if cmp -s "$deployer_script" "$root/autodeploy-interval-4.sh"; then
  fail 'the deployer holds no HEALTH_INTERVAL=3 line, so this case changes nothing'
fi
chmod +x "$root/autodeploy-interval-4.sh"
status=0
output=$(deployer_script="$root/autodeploy-interval-4.sh" && PASS_DEADLINE=1560 deploy_once) || status=$?
if ((status != 2)) || [[ "$output" != "PASS_DEADLINE is 1560s, below the 1620s a pass needs: 1410s for its deploy steps (the pull 900s, the up 300s, the health check 210s) and 210s for its rollback's health check; not starting" ]]; then
  fail 'the least pass deadline did not move with the health check interval'
fi

# A --once run ends whatever ends its pass. An error of bash's aborts the whole top-level command
# it is in, the `--once` branch included, and bash then goes on with the next one; a run that fell
# through would reach the continuous loop, which starts a --once run of its own after every pass.
# So the run must end by itself, failed, having read the clock once. It runs under the real timeout,
# so a run that fell through ends at 30 s and fails this case rather than run on.
case_begin 'a --once run ends whatever ends its pass'
fresh_env
printf 'garbled-once\n' >"$clock"
status=0
output=$(deployer_prefix=("$real_timeout" 30) && deploy_once) || status=$?
if ((status != 1)) || [[ $(wc -l <"$clock.calls") -ne 1 ]] || [[ "$output" == *"autodeploy started"* ]] ||
  [[ -s "$manifest_log" ]]; then
  fail "a --once run whose pass met a garbled clock ended with status $status after $(wc -l <"$clock.calls") reads of the clock, where it must end at once, status 1, having read it once"
fi
