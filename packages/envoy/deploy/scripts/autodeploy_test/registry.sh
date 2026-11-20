# shellcheck shell=bash
# Case group of autodeploy_test.sh: the pull's retry, the fetch of main and the registry's manifest
# reads. It runs on the stand-ins of fixture.sh, which autodeploy_test.sh sources first.

# A failed pull changes nothing: the previous image still serves, so there is nothing to stop or
# restart, and the next pass tries the pull again. Nothing started, so nothing is recorded as a
# failed deploy.
case_begin 'a failed pull, and the next pass pulls again'
fresh_env
for _ in 1 2; do
  if output=$(AUTODEPLOY_TEST_PULL_FAIL=1 deploy_once); then
    fail 'deployer did not fail when the image pull failed'
  fi
done
no_secret 'the failed pull'
if [[ $(grep -c ' pull ' "$log") -ne 2 || $(wc -l <"$log") -ne 2 ]]; then
  fail 'deployer did not retry the failed image pull on the next --once pass, or touched the running service'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]] || [[ -e "$failed_record" ]]; then
  fail 'deployer persisted the failed image tag after a pull failure, or recorded a deploy that never started'
fi

# A registry that answers late still answers: a manifest read has no bound of its own and waits as
# long as the pass's deadline leaves, so the newest commit, the one deployed, reads as published
# and nothing is deployed. The read is given exactly what the pass's deadline leaves at the read,
# read here at two deadlines far apart, the default hour and two hours: a read cut at a bound of its
# own, fixed or a cap under the deadline, is given less at one of them at least. The pass's clock
# is the fixture's, which no real time moves, so a slow box changes none of these numbers.
for pass in '' 7200; do
  case_begin "a registry that answers late, PASS_DEADLINE=${pass:-3600}"
  fresh_env
  deployed_tag_is new-image
  printf 'new-image late:2\n' >"$manifests"
  ok=0
  if [[ -z "$pass" ]]; then
    output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once) || ok=$?
  else
    output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' PASS_DEADLINE="$pass" deploy_once) || ok=$?
  fi
  if ((ok != 0)); then
    fail "deployer failed a pass whose registry answered late, inside PASS_DEADLINE=${pass:-3600}"
  fi
  if [[ -s "$log" ]] || [[ $(<"$manifest_log") != new-image ]] || [[ "$output" == *"not deploying"* ]]; then
    fail 'deployer did not take a late answer as the newest commit published, and leave the deployed image as it was'
  fi
  read_at=$(clocks_of 'manifest new-image')
  is_exactly "the manifest read's bound under PASS_DEADLINE=${pass:-3600}, what the pass's deadline leaves at the read" \
    "$((${pass:-3600} - ${read_at:-0}))" "$(bounds_of 'manifest new-image')"
  # The fetch of main before the walk is given what the pass leaves too.
  is_exactly "the fetch of main's bound under PASS_DEADLINE=${pass:-3600}" "${pass:-3600}" "$(bounds_of jj)"
done

# A fetch of main that fails is named, and the walk reads main as it was last fetched: the order
# read keeps a stale main from deploying anything older (loop.sh). One that never answers is
# stopped at the pass's deadline and named, and the pass then fails with nothing read.
case_begin 'a fetch of main that fails'
fresh_env
if output=$(AUTODEPLOY_TEST_FETCH_FAILS=1 deploy_once); then
  fail 'deployer did not fail at the origin check after a failed fetch of main'
fi
if [[ "$output" != *"jj git fetch of $repo failed with status 1; the walk reads main as it was last fetched"*"rolling back to old-image"* ]]; then
  fail 'deployer did not name a failed fetch of main and go on with the walk'
fi
case_begin 'a fetch of main that never answers'
fresh_env
if output=$(AUTODEPLOY_TEST_FETCH_HANG=1 AUTODEPLOY_TEST_TIMEOUT_CAP=2 deploy_once); then
  fail 'deployer passed a pass whose fetch of main never answered'
fi
if [[ "$output" != *"jj git fetch of $repo failed with status 124 (it had not answered when the pass's deadline, PASS_DEADLINE=3600s, passed)"*"the pass's deadline (PASS_DEADLINE=3600s) passed before docker manifest inspect example.test/dispatch:new-image could start; not deploying"* ]] ||
  [[ -s "$manifest_log" || -s "$log" ]]; then
  fail 'deployer did not name a fetch of main that ran out the pass, or read the registry after it'
fi

# A read the registry never answers is no proof the image is missing. When the pass's deadline
# passes the read is stopped and named on one line, the pass fails, and old-image, behind it, is
# neither asked about nor deployed over new-image, which already runs. (The timeout stand-in's cap
# stops the read two seconds in and moves the pass's clock on to where its bound ends; the suite's
# own limit on every run, deployer_prefix, fails a deployer that waits the hour out.)
case_begin 'a manifest read that never answers'
fresh_env
deployed_tag_is new-image
printf 'new-image hang\n' >"$manifests"
if output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' AUTODEPLOY_TEST_TIMEOUT_CAP=2 deploy_once); then
  fail 'deployer passed a pass whose registry read never answered'
fi
hung=$(still_hung)
if [[ -n "$hung" ]]; then
  kill "$hung"
  fail "the manifest read that never answered still runs after the deployer gave up on it (pid $hung)"
fi
if [[ ! -s "$manifests.hung" ]]; then
  fail 'the manifest read that never answers never ran, so this case proves nothing'
fi
if [[ "$output" != *"docker manifest inspect example.test/dispatch:new-image had not answered when the pass's deadline (PASS_DEADLINE=3600s) passed; not deploying"* ]]; then
  fail "deployer did not name the manifest read that ran out the pass's deadline"
fi
if [[ -s "$log" ]] || [[ $(<"$manifest_log") != new-image ]] || [[ "$output" == *"main has a newer published image"* ]]; then
  fail 'deployer went past a manifest read that never answered, to an older image'
fi

# An answer that does not say the image is missing, a rate limit here, proves nothing either: it is
# named with the registry's own words, and the pass fails with nothing deployed.
case_begin 'a refused manifest read'
fresh_env
deployed_tag_is new-image
printf 'new-image refused\n' >"$manifests"
if output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once); then
  fail 'deployer passed a pass whose registry refused the manifest read'
fi
if [[ "$output" != *"docker manifest inspect example.test/dispatch:new-image failed with status 1, an answer that does not say the image is missing: toomanyrequests: retry later; not deploying"* ]] ||
  [[ -s "$log" ]] || [[ $(<"$manifest_log") != new-image ]]; then
  fail 'deployer went past a refused manifest read, or did not name it'
fi

# The control: the registry's answer for a tag it does not hold, a commit whose image is still being
# built, is passed over as before, to the newest published commit behind it.
case_begin 'a commit the registry does not hold'
fresh_env
printf 'new-image unknown\n' >"$manifests"
if ! output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once); then
  fail 'deployer failed a pass on a commit the registry does not hold'
fi
if [[ $(<"$manifest_log") != $'new-image\nold-image' ]] || [[ -s "$log" ]] || [[ "$output" == *"not deploying"* ]]; then
  fail 'deployer did not pass over a commit the registry does not hold to old-image, which is deployed'
fi

# The pass's deadline bounds every read of the walk, not each read afresh: a commit the registry
# says it does not hold, whose answer took 1,000 s of the pass (the stand-in moves the pass's clock
# on), leaves the next read 2,600 s, and not a fresh hour.
case_begin 'the walk shares the pass deadline'
fresh_env
printf 'new-image unknown-clock:1000\n' >"$manifests"
if ! output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once); then
  fail 'deployer failed a pass whose first read answered 1,000 s in, inside its hour'
fi
is_exactly 'the bounds the walk gave its two reads' '3600 2600' \
  "$(bounds_of 'manifest new-image') $(bounds_of 'manifest old-image')"

# A read the pass's deadline has left no time for is not started, and the pass fails naming that
# deadline: a read handed the 0 seconds left would run under `timeout 0`, which is no bound at all.
# The newest commit's answer, that the registry holds no such image, comes past the pass's hour.
case_begin 'no read after the pass deadline'
fresh_env
printf 'new-image unknown-clock:3700\nold-image late:20\n' >"$manifests"
if output=$(AUTODEPLOY_TEST_COMMITS='new-image old-image' deploy_once); then
  fail "deployer passed a pass whose deadline passed during its walk"
fi
if [[ "$output" != *"the pass's deadline (PASS_DEADLINE=3600s) passed before docker manifest inspect example.test/dispatch:old-image could start; not deploying"* ]] ||
  [[ $(<"$manifest_log") != new-image ]] || [[ -s "$log" ]]; then
  fail "deployer started a manifest read after the pass's deadline had passed, or did not name the deadline"
fi

# A walk has no bound of its own: thirty commits, the newest twenty-nine of which the registry says
# it does not hold, each saying so 110 s in (the pass's clock moves on), are read to the end, to
# the thirtieth, which is deployed, inside the pass's hour. A walk with a deadline of its own fails
# such a pass after reads that all answered.
case_begin 'a walk of thirty reads'
fresh_env
for commit in $(seq -f 'c%02g' 1 29); do
  printf '%s unknown-clock:110\n' "$commit"
done >"$manifests"
deployed_tag_is c30
if ! output=$(AUTODEPLOY_TEST_COMMITS="$(seq -f 'c%02g' 1 30)" deploy_once); then
  fail "deployer failed a walk of thirty reads that all answered inside the pass's deadline"
fi
if [[ $(wc -l <"$manifest_log") -ne 30 ]] || [[ -s "$log" ]] || [[ "$output" == *"not deploying"* ]]; then
  fail 'deployer did not read all thirty commits of a walk whose reads all answered'
fi
