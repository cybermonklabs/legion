#!/usr/bin/env bash
# The settings a job's Postgres service runs with, read back from the server itself.
#
# Who runs it, and why it is a script: the read-back step of every ci.yaml job whose Postgres
# service starts on the lane's disposable settings - the test job, envoy-go (one step for each of
# its four services), envoy-go-race, envoy-go-race-api, envoy-go-race-docs,
# envoy-go-docs-finalizers, dispatch-memory, dispatch-multiprocess, broker-multiprocess and
# envoy-session-stall. Those thirteen steps were one eleven-line loop written out thirteen times,
# three of envoy-go's four differing in the service they named and the error they printed, until
# a review read them together (dispatch://INFRA-1075). dispatch-browser-postgres.sh's head records
# this repository's rule for a step body repeated across jobs, and this follows it: the five
# settings are written once, here, and each step is one line calling this.
#
#   .github/scripts/postgres-service-settings.sh <container id> <label>
#
# <container id> is the service's `${{ job.services.<service id>.id }}`. The runner sets it for a
# service the job declares and leaves it empty for a service id the job does not have, so an
# empty one is refused by name rather than handed to docker. <label> is that service id, and
# every line this prints starts with it, so a red step of a job with four services says which of
# them it read.
#
# Why the settings are read back: they are the service's command: line, which makes the server
# start with them. A command: the runner did not pass through would leave the server up on the
# image's defaults rather than failing, and the job would then refuse connections under load
# (max_connections), skip the event log's floor test (max_prepared_transactions) and pay for the
# forced checkpoints the other three make cheap, with nothing saying so. A setting the server
# itself refuses never reaches this script: postgres exits, and the service's health check fails
# the job before its first step. ci.yaml's envoy-go service says what each setting buys.
#
# What it prints: one line, `<label>: <container id>: fsync=off full_page_writes=off ...`, the
# five as the server reported them, and before it an ::error:: for each setting the server
# reports at another value, naming the label, the setting, what it read and what it wants. It
# exits 1 when any setting differs, after reading all five, so one red step names every setting
# that is off. A setting `docker exec` cannot read at all ends it at once, with an ::error::
# naming the label, the container and the setting.
#
# Run from anywhere: it reads nothing of the checkout. Its test is
# .github/scripts/postgres-service-settings.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit

usage() {
  echo "::error::postgres-service-settings.sh takes two arguments, the service's container id" \
    "and its service id: .github/scripts/postgres-service-settings.sh <container id> <label>." \
    "It was given: $*"
  exit 1
}

[ "$#" -eq 2 ] || usage "$@"
id=$1
label=$2
[ -n "$label" ] || usage "$@"
if [ -z "$id" ]; then
  echo "::error::$label: the container id is empty, which is what the runner hands a step for" \
    "a service id the job does not declare"
  exit 1
fi

# The service's command: line, setting by setting, as `show` reports each one.
settings=(fsync=off full_page_writes=off synchronous_commit=off max_connections=500 max_prepared_transactions=20)

failed=0
read_back=
for want in "${settings[@]}"; do
  setting=${want%%=*}
  if ! value=$(docker exec "$id" psql -U postgres -Atc "show $setting"); then
    echo "::error::$label: docker exec into $id could not read $setting"
    exit 1
  fi
  read_back+=" $setting=$value"
  if [ "$value" != "${want#*=}" ]; then
    echo "::error::the Postgres service $label reports $setting=$value, not ${want#*=}"
    failed=1
  fi
done
echo "$label: $id:$read_back"
exit "$failed"
