# shellcheck shell=bash disable=SC2154 # fixture.sh, which autodeploy_test.sh sources first, sets these
# Case group of autodeploy_test.sh: the bound each call of a deploy and of its rollback is given,
# against the pass's deadline. It runs on the stand-ins of fixture.sh, which autodeploy_test.sh
# sources first. The pass's clock is the fixture's, which only the stand-ins move, so every bound
# below is an exact number, whatever the box's load.

# Each call of a deploy is given what the pass's deadline leaves less the 180 s the pass keeps for
# a rollback, and the pull and the up their own bound where that is less: the pull's 900 s, after
# which nothing has changed, and the up's 300 s, the deploy's limit for the new image's start. Each
# health check request is given its 3 s, and the origin check what the deploy's part of the pass
# leaves. The rollback's stop, its confirmation, its schema read and its own up have no bound of
# their own: each gets what the pass leaves, all of it. Here the pass starts at 0 on its clock and
# the deploy's one health check try moves it 3 s on.
case_begin 'the bounds of a deploy and of its rollback'
fresh_env
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment'
fi
is_exactly "the pull's bound" 900 "$(bounds_of 'compose pull')"
is_exactly "the bounds of the deploy's up and of the rollback's up" '300 3597' "$(bounds_of 'compose up' | paste -sd' ')"
is_exactly "the health check requests' bounds, the deploy's and the rollback's" '3 3' "$(bounds_of 'curl /healthz' | paste -sd' ')"
is_exactly "the origin check's bound" 3417 "$(bounds_of 'curl /auth/start')"
is_exactly "the rollback's stop's bound" 3597 "$(bounds_of 'compose stop')"
is_exactly "the rollback's confirmation's bound" 3597 "$(bounds_of 'compose ps')"

# Where the pass has less left than a step's own bound, the step gets what is left: the newest
# commit's manifest read took 3,000 s of the pass (the stand-in moves the pass's clock on), so the
# pull gets 420 s, the 600 s left less the rollback's 180, not its 900, and the rollback's stop all
# that is left after the health check's 3 s, 597.
case_begin 'a deploy with 600 s of the pass left'
fresh_env
printf 'new-image clock:3000\n' >"$manifests"
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment'
fi
is_exactly "the pull's bound with 600 s left" 420 "$(bounds_of 'compose pull')"
is_exactly "the up's bound with 600 s left" 300 "$(bounds_of 'compose up' | head -n1)"
is_exactly "the rollback's stop's bound with 600 s left" 597 "$(bounds_of 'compose stop')"

# A pull that runs out its own 900 s is named with that bound, and nothing changed: the previous
# image still serves, nothing is recorded as a failed deploy, and the next pass pulls again. (The
# timeout stand-in's cap stops the hung pull two seconds in and moves the pass's clock on to where
# its bound ends.)
case_begin 'a pull that runs out its own bound'
fresh_env
if output=$(AUTODEPLOY_TEST_PULL_HANG=1 AUTODEPLOY_TEST_TIMEOUT_CAP=2 deploy_once); then
  fail 'deployer passed a pass whose pull never answered'
fi
if [[ "$output" != *"could not pull new-image (it ran out its own 900s); old-image still serves"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 0 ]] || [[ -e "$failed_record" ]]; then
  fail 'deployer did not name a pull that ran out its own bound, or went on past it'
fi

# An up that runs out its own 300 s is named with that bound and rolled back.
case_begin 'an up that runs out its own bound'
fresh_env
if output=$(AUTODEPLOY_TEST_UP_TAKES=400 deploy_once); then
  fail 'deployer passed a pass whose up ran out its bound'
fi
if [[ "$output" != *"compose up failed for new-image (it ran out its own 300s)"*"rolling back to old-image"* ]]; then
  fail 'deployer did not name an up that ran out its own bound, or did not roll it back'
fi

# A stop that never answers fails, named as having run out the pass's deadline, and the rollback is
# refused as for a failed stop: the line says new-image may still run and that whether it migrated
# is not known. So does a schema read whose container never answers. Each leaves the record saying
# the deployer stopped. (The timeout stand-in's cap ends each two seconds in and moves the pass's
# clock on to where its bound ends.)
case_begin 'a rollback stop that never answers'
fresh_env
status=0
output=$(AUTODEPLOY_TEST_STOP_HANG=1 AUTODEPLOY_TEST_TIMEOUT_CAP=2 deploy_once) || status=$?
if ((status != 3)) ||
  [[ "$output" != *"could not stop new-image (it had not answered when the pass's deadline, PASS_DEADLINE=3600s, passed), so refusing to roll back to old-image, which runs against schema 112 (112 applied); new-image may still run, and whether it changed the applied migrations is not known, since the schema was not read: forward recovery is required"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]] || [[ $(cut -d' ' -f1,2 "$failed_record") != 'new-image stopped' ]]; then
  fail "deployer did not name a rollback stop that ran out the pass's deadline and stop (status $status), or rolled back without it"
fi
case_begin 'a schema read after the stop whose container never answers'
fresh_env
status=0
output=$(AUTODEPLOY_TEST_RUN_HANG_AFTER_UP=1 AUTODEPLOY_TEST_TIMEOUT_CAP=2 deploy_once) || status=$?
if ((status != 3)) ||
  [[ "$output" != *"could not read the schema version after stopping new-image (status 124 (it had not answered when the pass's deadline, PASS_DEADLINE=3600s, passed)), so refusing to roll back to old-image"*"; new-image is stopped, and whether it changed the applied migrations is not known, since the schema was not read: forward recovery is required"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail "deployer did not name a schema read that ran out the pass's deadline, or rolled back without it"
fi

# The read after the stop waits on the migration key for as long as the pass's deadline leaves, not
# a bound of its own: a migration the stopped image sent still holds the key, and still works. Here
# it holds it for 75 tries a second apart after the stop, past the read before a deploy's 60 s; the
# read takes the key on the 76th and rolls back. Each refused try read the key's holder.
case_begin 'the migration key held for 75 s after the stop'
fresh_env
if output=$(AUTODEPLOY_TEST_KEY_HELD_AFTER_STOP=75 deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment'
fi
if [[ "$output" != *"rolling back to old-image"* ]] || [[ "$output" == *"refusing to roll back"* ]] ||
  [[ $(cut -d' ' -f1,2 "$failed_record") != 'new-image rolled-back' ]]; then
  fail 'deployer did not wait for a migration key held 75 s after the stop, and roll back'
fi
is_exactly 'the holder reads of the refused tries after the stop' 75 "$(grep -cF "$holder_read" "$argv")"
# The read's tries after the stop are given what the pass leaves, from 3597 s at its first try down
# to 3522 at the one that took the key; the rollback's up gets what is left after them.
is_exactly 'the first and the last bound of the tries after the stop' '3597 3522' \
  "$(grep -o '^container-timeout [0-9]*$' "$argv" | awk 'NR == 2 {first = $2} {last = $2} END {print first, last}')"
is_exactly "the rollback's up's bound" 3522 "$(bounds_of 'compose up' | tail -n1)"

# The control: a key held past the pass's deadline is not waited out. The read refuses at that
# deadline, naming the key and the session the server named as its holder, and says new-image is
# stopped and whether it migrated is not known; the deployer stops. (Each refused try here takes
# 500 s of the pass's clock, as a statement waiting out a long lock would, so the pass's hour is
# reached in eight tries.)
case_begin 'the migration key held past the pass deadline after the stop'
fresh_env
status=0
output=$(AUTODEPLOY_TEST_KEY_HELD_AFTER_STOP=always AUTODEPLOY_TEST_KEY_TRY_TAKES=500 deploy_once) || status=$?
if ((status != 3)) ||
  [[ "$output" != *"could not read the schema version after stopping new-image (the migration key, 8150001, was still held by pid 4242 (dispatch-migrate, active, in a transaction for 00:01:15) when the pass's deadline (PASS_DEADLINE=3600s) passed), so refusing to roll back to old-image, which runs against schema 112 (112 applied); new-image is stopped, and whether it changed the applied migrations is not known, since the schema was not read: forward recovery is required"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail "deployer did not refuse at the pass's deadline, naming the held key and its holder (status $status)"
fi
is_exactly 'the tries after the stop' 8 "$(($(grep -cF "$schema_try" "$argv") - 1))"

# A deploy starts only where the pass leaves more than the 180 s it keeps for a rollback: a deploy
# that started with less would leave the image it started no time to be rolled back, and a person
# would be sent to recover forward from an image that migrated nothing. Here the newest commit's
# manifest read answers 3,590 s into the pass: nothing is backed up, pulled or started, old-image
# still serves, and the next pass starts with the whole deadline.
case_begin 'a deploy the pass has no time for'
fresh_env
printf 'new-image clock:3590\n' >"$manifests"
if output=$(AUTODEPLOY_TEST_CANDIDATE_NATS=false deploy_once); then
  fail 'deployer passed a pass that had no time for a deploy'
fi
if [[ "$output" != *"the pass leaves 10s, no more than the 180s it keeps for a rollback, before the deploy of new-image could start; old-image still serves"* ]] ||
  [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv" || [[ "$output" == *"forward recovery"* ]] || [[ -e "$failed_record" ]]; then
  fail 'deployer started a deploy the pass had no time to roll back, or did not name why it did not'
fi

# A health check never takes the rollback's time: each try is cut to what the deploy's part of the
# pass leaves, and the check fails at the try that has none, so the rollback still has its 180 s.
# Here the deploy starts with 300 s left, and each health request of new-image runs out its 3 s:
# twenty tries of 6 s fit before the pass has only the 180 s left, the 21st does not start, and the
# rollback runs in those 180 s.
case_begin 'a health check that would run into the rollback time'
fresh_env
printf 'new-image clock:3300\n' >"$manifests"
if output=$(AUTODEPLOY_TEST_HEALTH_TAKES=5 deploy_once); then
  fail 'deployer kept an image whose health check never answered'
fi
if [[ "$output" != *"the pass leaves 180s, no more than the 180s it keeps for a rollback, before try 21 of the health check of new-image could start"*"rolling back to old-image"* ]] ||
  [[ $(cut -d' ' -f1,2 "$failed_record") != 'new-image rolled-back' ]]; then
  fail "deployer did not end the health check at the rollback's time, and roll back"
fi
is_exactly "the deploy's health check tries" 20 "$(awk '$3 == "curl" && $4 == "/healthz" && $2 < 3420' "$bounds_log" | wc -l)"
is_exactly "the rollback's stop, at the start of the 180 s kept for it" '180@3420' \
  "$(awk '$3 == "compose" && $4 == "stop" {print $1 "@" $2}' "$bounds_log")"
