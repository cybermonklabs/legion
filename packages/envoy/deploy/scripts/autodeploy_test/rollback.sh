# shellcheck shell=bash disable=SC2154 # fixture.sh, which autodeploy_test.sh sources first, sets these
# Case group of autodeploy_test.sh: a failed deploy's rollback, the schema-set refusal, the stop
# first, the migration key's poll and holder, and the database credential's split. It runs on the
# stand-ins of fixture.sh, which autodeploy_test.sh sources first.

# A deploy that fails its origin check with the schema where it was stops the candidate, confirms
# it stopped, reads the schema, and rolls back. Every database client gets the URL without its
# password and the password as PGPASSWORD.
case_begin 'the rollback after a failed origin check'
fresh_env
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment'
fi
no_secret 'the rollback'
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer did not restore the previous image tag after the origin check failed'
fi
if ! compgen -G "$backups/pre-new-image-*.dump" >/dev/null; then
  fail 'deployer did not write the pre-deploy database backup'
fi
if [[ $(<"$log") != *" pull "*$'\n'*" up "*$'\n'*" stop dispatch"$'\n'*" ps "*$'\n'*" up "* ]] || [[ $(wc -l <"$log") -ne 5 ]]; then
  fail 'deployer did not pull, deploy, stop, confirm the stop, and roll back after the origin check failed'
fi
if [[ "$output" != *"rolling back to old-image"* ]]; then
  fail 'deployer did not report the redirect-uri rollback'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:$(schema_value)"* ]]; then
  fail 'deployer did not record the set of migrations old-image runs against'
fi
if [[ $(grep -c "^pg_dump -Fc $url_without_password\$" "$argv") -ne 1 ]] ||
  [[ $(grep -c "$psql_run" "$argv") -ne 2 ]] || [[ $(grep -cF "$schema_try" "$argv") -ne 2 ]] ||
  [[ $(grep -cF "$schema_read" "$argv") -ne 2 ]]; then
  fail 'deployer did not run pg_dump once and psql twice (before the deploy and after the stop) on the URL without its password, each run the try for the runner key and then the read'
fi
# Each client container runs under an init that passes on a signal, so `timeout` ends it.
if [[ $(grep -c '^docker run ' "$argv") -ne 3 || $(grep -c '^docker run --init ' "$argv") -ne 3 ]]; then
  fail 'deployer ran a database client container without --init, which a timeout could not stop'
fi

# A new image that serves with its NATS connection failing (`nats` false) fails the health check
# and is rolled back, as when Dispatch exited at boot without NATS; one with NATS turned off
# (`nats` null) passes it and goes on to the origin check.
case_begin 'a candidate whose NATS connection fails'
fresh_env
if output=$(AUTODEPLOY_TEST_CANDIDATE_NATS=false deploy_once); then
  fail 'deployer kept an image whose NATS connection failed'
fi
if [[ "$output" != *"did not answer 200 with NATS connected"*"rolling back to old-image"* ]] ||
  [[ "$output" == *"OAuth redirect_uri"* ]] || [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer did not roll back an image whose /healthz reported nats false'
fi
case_begin 'a candidate with NATS turned off'
fresh_env
if output=$(AUTODEPLOY_TEST_CANDIDATE_NATS=null deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment with NATS turned off'
fi
if [[ "$output" != *"OAuth redirect_uri"*"rolling back to old-image"* ]] || [[ "$output" == *"NATS connected"* ]]; then
  fail 'deployer did not pass the health check of an image with NATS turned off (nats null)'
fi

# A deploy whose image moved the schema before it failed refuses to start the previous image
# against that schema: it stops the new one, names both versions, and fails.
case_begin 'the refusal after a candidate migrated'
fresh_env
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  fail 'deployer did not fail after a deploy that moved the schema failed its origin check'
fi
no_secret 'the refusal'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112 (112 applied)"*"113 (113 applied)"*"forward recovery is required"* ]]; then
  fail 'deployer did not refuse the rollback naming both schema versions'
fi
if [[ $(grep -c ' up ' "$log") -ne 1 || $(grep -c ' stop dispatch' "$log") -ne 1 ]]; then
  fail 'deployer did not deploy once and stop the new image once, without rolling back'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer persisted the failed image tag after refusing its rollback'
fi

# A person who has recovered deletes the record of the refused deploy (loop.sh's cases read it), and
# the next pass retries the same image. The schema is already at 113 before it starts, and the
# rollback is still refused: old-image runs against 112, as recorded on the first attempt.
case_begin 'the retry of an image whose rollback was refused, once the record is deleted'
: >"$log"
rm -f "$failed_record"
if output=$(deploy_once); then
  fail 'deployer did not fail on the retry of an image whose rollback it refused'
fi
no_secret 'the retry'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112 (112 applied)"*"113 (113 applied)"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail 'deployer rolled back to old-image against schema 113 on the retry'
fi

# A migration the failed image held open commits only as the image stops. The deployer reads the
# schema after the stop, so it sees 113 and refuses; a read before the stop would see 112.
case_begin 'a migration that commits as the candidate stops'
fresh_env
if output=$(AUTODEPLOY_TEST_MIGRATE_ON_STOP=113 deploy_once); then
  fail 'deployer did not fail when the failed image migrated as it stopped'
fi
no_secret 'the migration committing as the image stops'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112 (112 applied)"*"113 (113 applied)"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]] ||
  [[ $(schema_value) != 113-113-* ]]; then
  fail 'deployer rolled back to old-image while the failed image held a migration open'
fi

# An out-of-order migration: the failed image applied a version below the highest one already
# applied, a numbered migration under a timestamped one, so max(version) is where it was and only
# the set changed. The deployer compares the set, so it refuses this rollback too.
case_begin 'an out-of-order migration'
fresh_env
printf '%s\n' 20260115123000 >>"$schema"
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  fail 'deployer did not fail after a deploy that applied an out-of-order migration failed its origin check'
fi
no_secret 'the out-of-order refusal'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 20260115123000 (113 applied)"*"20260115123000 (114 applied)"*"forward recovery is required"* ]]; then
  fail 'deployer did not refuse the rollback after a migration applied below the highest one left max(version) where it was'
fi
if [[ $(grep -c ' up ' "$log") -ne 1 || $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer started old-image against a schema holding a migration old-image never ran'
fi

# A compose/.env the deployer before this one wrote records <tag>:<max(version)>, with no set
# behind it. While that tag is still deployed it is compared the old way, so a retry in flight
# across the upgrade keeps the safety it started with: a schema whose max has moved refuses, and
# the record is left as it is.
case_begin 'an old-form schema record, the schema moved'
fresh_env
printf 'ENVOY_IMAGE_SCHEMA=old-image:112\n' >>"$compose/.env"
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  fail 'deployer did not fail on an old-form record after a deploy that moved the schema failed'
fi
no_secret 'the old-form record'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112"*"113 (113 applied)"*"forward recovery is required"* ]] ||
  [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:112"* ]]; then
  fail 'deployer did not refuse the rollback against an old-form ENVOY_IMAGE_SCHEMA record, or rewrote it'
fi
# The same record with the schema where old-image left it rolls back.
case_begin 'an old-form schema record, the schema unmoved'
fresh_env
printf 'ENVOY_IMAGE_SCHEMA=old-image:112\n' >>"$compose/.env"
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment on an old-form record'
fi
no_secret 'the old-form record with the schema unmoved'
if [[ "$output" != *"rolling back to old-image"* ]] || [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer did not roll back on an old-form record with the schema where old-image left it'
fi

# A stop that fails, or that leaves the candidate running, proves nothing about the schema: the
# refusal says the candidate may still run and that whether it migrated is not known.
for stop in AUTODEPLOY_TEST_STOP_FAIL AUTODEPLOY_TEST_STOP_LEAVES_RUNNING; do
  case_begin "a stop that proves nothing ($stop)"
  fresh_env
  if output=$(export "$stop=1" && deploy_once); then
    fail "deployer did not fail when $stop"
  fi
  no_secret "the refused stop ($stop)"
  if [[ "$output" != *"refusing to roll back to old-image"*"; new-image may still run, and whether it changed the applied migrations is not known, since the schema was not read: forward recovery is required"* ]] ||
    [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
    fail "deployer rolled back without a confirmed stop, or did not say what is not known ($stop)"
  fi
done

# A schema version it cannot read after the stop is no proof the schema stayed: it refuses.
case_begin 'a schema it cannot read after the stop'
fresh_env
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE_AFTER_UP=1 deploy_once); then
  fail 'deployer did not fail when it could not read the schema version after a failed deploy'
fi
no_secret 'the unreadable schema'
if [[ "$output" != *"could not read the schema version"*"status 2"*"refusing to roll back to old-image"*"; new-image is stopped, and whether it changed the applied migrations is not known"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail 'deployer rolled back without knowing the schema version'
fi

# A schema version it cannot read before a deploy stops the deploy before anything changes.
case_begin 'a schema it cannot read before the deploy'
fresh_env
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE=1 deploy_once); then
  fail 'deployer deployed without reading the schema version first'
fi
no_secret 'the unreadable schema before the deploy'
if [[ "$output" != *"could not read the schema version"*"not deploying new-image"* ]] || [[ -s "$log" ]]; then
  fail 'deployer did not stop before deploying when the schema version was unreadable'
fi

# A key another session holds does not make the read wait on it: a blocking wait deadlocks against
# a migration runner building an index concurrently under that key, so the read tries the key,
# reads nothing of the schema while it is held, and tries again. Here the key is held against the
# first two tries; the third takes it, and the deploy runs as it does with a free key.
case_begin 'a migration key held for two tries'
fresh_env
printf '2\n' >"$schema.key"
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment behind a held key'
fi
no_secret 'the held migration key'
# While the key is held the read must not run at all, not merely be dropped from the answer: the
# fake fails such a read, as the server can, and under ON_ERROR_STOP=1 psql's non-zero status
# would end the deployer's read rather than its poll. So of the four runs only the two granted
# the key read anything: the third try, which took it, and the one after the stop. The two refused
# ones read the key's holder instead.
if [[ $(grep -cF "$schema_read" "$argv") -ne 2 ]] || [[ "$output" == *"could not read the schema version"* ]]; then
  fail 'deployer read the applied set while another session held the migration key, so a failing read ended its poll'
fi
if [[ $(grep -cF "$holder_read" "$argv") -ne 2 ]]; then
  fail 'deployer did not read the holder of the migration key on each refused try, and on no other'
fi
if [[ $(<"$schema.key") -ne 0 ]]; then
  fail 'deployer did not try the held migration key more than once'
fi
# Four psql runs, each trying the key: it refused the first two, the third took it and read the
# set, and one more after the stop. An f answer taken for the applied set would record 0 versions
# and refuse the rollback.
if [[ $(grep -c "$psql_run" "$argv") -ne 4 ]] || [[ $(grep -cF "$schema_try" "$argv") -ne 4 ]] ||
  [[ "$output" != *"rolling back to old-image"* ]] ||
  [[ "$output" == *"refusing to roll back"* ]] || [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:$(schema_value)"* ]]; then
  fail 'deployer did not poll the held migration key and deploy once it came free'
fi
# Nothing in the run waits on the key inside the database, in a statement run or in an argument.
if grep -qF 'select pg_advisory_lock(' "$argv"; then
  fail 'deployer waited on the migration key in a blocking statement, which deadlocks the migration runner'
fi

# A key no session ever lets go of, as a pod killed mid-CREATE INDEX CONCURRENTLY leaves behind:
# the backend goes on building the index, and holds the key for minutes. The deployer tries the
# key a second apart (the sleep stand-in moves the pass's clock on by that second) until the read's
# deadline, SCHEMA_READ_DEADLINE's 60 s, has passed: 60 tries. It never reads behind the key, and
# then refuses, naming the key and the session the server named as its holder: a schema version it
# cannot read is no ground to deploy on.
case_begin 'a migration key held past the read before the deploy'
fresh_env
printf 'always\n' >"$schema.key"
if output=$(deploy_once); then
  fail 'deployer deployed without reading the schema version behind a key no session released'
fi
no_secret 'the migration key held past the deadline'
if [[ $(grep -cF "$schema_try" "$argv") -ne 60 ]] || [[ $(grep -c "$psql_run" "$argv") -ne 60 ]]; then
  fail "deployer made $(grep -cF "$schema_try" "$argv") tries for the migration key, a second apart, under the read's 60 s deadline, not 60"
fi
if grep -qF "$schema_read" "$argv"; then
  fail 'deployer read the applied set while another session held the migration key'
fi
if [[ "$output" != *"could not read the schema version (the migration key, 8150001, was still held by pid 4242 (dispatch-migrate, active, in a transaction for 00:01:15) when SCHEMA_READ_DEADLINE=60s had passed); not deploying new-image"* ]]; then
  fail 'deployer did not name the held migration key, its holder and the deadline that passed, and stop'
fi
# Nothing of the deploy ran: no pg_dump, which is how the pre-deploy backup is taken, and no
# compose, so the previous image still serves. ($backups holds the dumps of the cases above, so
# what this reads is the argument log of this run, which fresh_env emptied.)
if [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv" ||
  [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer deployed, or backed up for a deploy, behind a migration key it could not take'
fi

# Each try's waits are bounded by what the read's deadline leaves, never by a fresh bound of their
# own: its psql's `timeout` inside the container, and its lock_timeout, which a statement waiting on
# a table lock (the runner's widening ALTER TABLE) waits out. Under a three-second deadline the
# three tries a second apart are given 3, 2 and 1 s, and 3000, 2000 and 1000 ms.
case_begin 'each try is given what the read before the deploy leaves'
fresh_env
printf 'always\n' >"$schema.key"
if output=$(SCHEMA_READ_DEADLINE=3 deploy_once); then
  fail 'deployer deployed behind a key no session released under a three-second deadline'
fi
no_secret 'the bounds the tries were given'
is_exactly 'the bounds the tries of a three-second read were given' '3 2 1' \
  "$(grep -o '^container-timeout [0-9]*$' "$argv" | awk '{print $2}' | paste -sd' ')"
is_exactly 'the lock_timeout the tries of a three-second read were given' '3000 2000 1000' \
  "$(sed -n 's/^psql-sql set lock_timeout = //p' "$argv" | paste -sd' ')"
if grep -qF "$schema_read" "$argv" || [[ "$output" != *"could not read the schema version"*"SCHEMA_READ_DEADLINE=3s had passed); not deploying new-image"* ]] || [[ -s "$log" ]]; then
  fail 'deployer did not report the unreadable schema version and stop once its deadline had passed'
fi
# And the default deadline is what it was for an operator: against a free key the first try gets
# the whole minute. The read after the stop, which decides the rollback, is given what the pass's
# deadline leaves instead (deadline.sh reads it held). Each database client container is given
# what the pass leaves too: before the deploy less the 180 s it keeps for a rollback, and in the
# rollback all of it.
case_begin 'the bounds of the reads and of their containers'
fresh_env
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment it read the schema for'
fi
is_exactly 'what the two schema reads were given, the one before the deploy and the one after the stop' \
  "60 $((3600 - 3))" "$(grep -o '^container-timeout [0-9]*$' "$argv" | awk '{print $2}' | paste -sd' ')"
is_exactly 'the lock_timeout of the two schema reads' "60000 $(((3600 - 3) * 1000))" \
  "$(sed -n 's/^psql-sql set lock_timeout = //p' "$argv" | paste -sd' ')"
# The read before, the backup, and the read after the stop: the health check's one try moved the
# pass's clock 3 s before the stop.
is_exactly 'the database client containers' "3420@0 3420@0 3597@3" \
  "$(awk '$3 == "run" {print $1 "@" $2}' "$bounds_log" | paste -sd' ')"


# Both URL loops below run under en_US.UTF-8, where bash's [[ =~ ]] ranges such as [a-z] and
# [0-9] also match non-ASCII characters, so a grammar that left the locale in force would accept
# what it should refuse. The control needs that locale installed and widening the ranges.
case_begin 'the locale the URL cases need'
if ! LC_ALL=en_US.UTF-8 bash -c '[[ "ä" =~ ^[a-z]$ ]]' 2>/dev/null; then
  fail 'en_US.UTF-8 is not installed here (sudo locale-gen en_US.UTF-8), so the URL loops would not test the locale'
fi

# The deployer accepts one URL shape and refuses every other before any client starts, so no
# password in a refused URL can reach a process's arguments, and the refusal never prints the URL.
# Build the URLs from source fragments. The fixture must exercise credentials without putting a
# credential-shaped literal in a public source file.
review_credential='review-'"secret"
password_key='pass'"word"
sslpassword_key='sslpass'"word"
url_scheme='postgres'
url_with_user_credential() {
  local user=$1 credential=$2 target=$3 scheme=${4:-$url_scheme}
  printf '%s://%s' "$scheme" "$user"
  printf ':%s@%s' "$credential" "$target"
}
url_without_user_credential() {
  local user=$1 target=$2 scheme=${3:-$url_scheme}
  printf '%s://%s@%s' "$scheme" "$user" "$target"
}
refused_case() {
  printf '%s|%s' "$1" "$2"
}
user_case() {
  refused_case "$1" "$(url_with_credential "$2" "$3")"
}
query_case() {
  refused_case "$1" "$(url_without_credential "$2")?$3"
}
accepted_case() {
  printf '%s|%s|%s' "$1" "$2" "$3"
}

# The cases cover each way out of the shape:
#   - the credential: a malformed escape, %00, an escape that decodes to bytes that are not
#     UTF-8, a raw ? / @ or space, raw non-ASCII, and a double-byte character whose second byte
#     is @;
#   - the query: a credential under any spelling, a key outside the list, an @, and a non-ASCII
#     value;
#   - the user, host and port: a non-ASCII user, a bracketed, IPv6, multi-host or socket host, an
#     empty or hyphen-edged host label, a port of 0, above 65535 or not ASCII digits;
#   - an @ in the path, and a keyword/value string.
# Most are URLs pgx or libpq would connect with; the fake is given the credential libpq would
# read, so a deployer that let one through would show it in some client's arguments.
unicode_credential='pässwörd-'cret-canary
bad_bytes=$'pw\xa9@cret-'canary
for case in \
  "$(user_case "$review_credential" "${review_credential}%oops" "$default_database_target")" \
  "$(user_case s "s%00${password#s@}" "$default_database_target")" \
  "$(user_case x "s%FF${password#s@}" "$default_database_target")" \
  "$(user_case "s?${password#s@}" "s?${password#s@}" "$default_database_target?sslmode=require")" \
  "$(user_case "s/${password#s@}" "s/${password#s@}" "$default_database_target")" \
  "$(user_case s "$password" "$default_database_target")" \
  "$(user_case "s ${password#s@}" "s ${password#s@}" "$default_database_target")" \
  "$(user_case "$unicode_credential" "$unicode_credential" "$default_database_target")" \
  "$(user_case x "$bad_bytes" "$default_database_target")" \
  "$(query_case "$review_credential" "$default_database_target" "sslmode=require&${password_key}=${review_credential}")" \
  "$(query_case "$review_credential" "$default_database_target" "${sslpassword_key}=${review_credential}")" \
  "$(query_case "$review_credential" "$default_database_target" "pass%77ord=${review_credential}")" \
  "$(query_case "$review_credential" "$default_database_target" "sslmode=require& ${password_key}=${review_credential}")" \
  "$(query_case "$review_credential" "$default_database_target" "${password_key} =${review_credential}")" \
  "$(query_case "$review_credential" "$default_database_target" "${sslpassword_key} =${review_credential}")" \
  "$(user_case "$password" "$encoded_password" "$default_database_target?target_session_attrs=any")" \
  "$(query_case "$review_credential" "$default_database_target" "application_name=a@b&${password_key}=${review_credential}")" \
  "$(user_case "$password" "$encoded_password" "$default_database_target?application_name=ｄｅｐｌｏｙ")" \
  "$(refused_case "$password" "$(url_with_user_credential 'dispätch' "$encoded_password" "$default_database_target")")" \
  "$(query_case "$review_credential" '[::1?x]:5432,db.test:5432/dispatch' "${password_key}=${review_credential}&sslmode=require")" \
  "$(user_case "$password" "$encoded_password" '[::1]:5432/dispatch')" \
  "$(user_case "$password" "$encoded_password" 'db1.test:5432,db2.test:5432/dispatch')" \
  "$(user_case "$password" "$encoded_password" '%2Fvar%2Frun%2Fpostgresql/dispatch')" \
  "$(user_case "$password" "$encoded_password" 'db..test:5432/dispatch')" \
  "$(user_case "$password" "$encoded_password" '-db.test:5432/dispatch')" \
  "$(user_case "$password" "$encoded_password" 'db.test:0/dispatch')" \
  "$(user_case "$password" "$encoded_password" 'db.test:65536/dispatch')" \
  "$(user_case "$password" "$encoded_password" 'db.test:5²/dispatch')" \
  "$(user_case "$password" "$encoded_password" 'db.test:5432/dis@patch')" \
  "$(refused_case "$review_credential" "host=db.test user=dispatch ${password_key}=${review_credential} dbname=dispatch")"; do
  case_begin "a refused DATABASE_URL (${case#*|})"
  fresh_env "${case#*|}"
  if output=$(LC_ALL=en_US.UTF-8 test_password="${case%%|*}" deploy_once); then
    fail "deployer deployed with ${case#*|}"
  fi
  no_secret "${case#*|}"
  if [[ "$output" != *"DATABASE_URL"*"not deploying new-image"* ]] || [[ "$output" == *"${case#*|}"* ]] ||
    [[ -s "$log" || -s "$argv" ]]; then
    fail "deployer did not refuse ${case#*|} before running anything, without printing it"
  fi
done

# The accepted shape splits as intended: each allowed query key, the highest port, a URL with no
# port, escapes that decode to reserved characters or to multi-byte UTF-8, and a URL with no
# credential at all. The clients get the URL without its credential as their last argument. The
# credential reaches the fake libpq only as PGPASSWORD, and the deploy goes on to the origin
# check's rollback.
encoded_unicode='s%C3%BC%C3%9F-'"canary"
for case in \
  "$(accepted_case "$password" "$(url_with_credential "$encoded_password" "$default_database_target?sslmode=require&connect_timeout=10&application_name=dispatch-deploy")" "$(url_without_credential "$default_database_target?sslmode=require&connect_timeout=10&application_name=dispatch-deploy")")" \
  "$(accepted_case 'Az09._~-:/?#[] @' "$(url_with_user_credential 'dispatch.ro' 'Az09._~-%3A%2F%3F%23%5B%5D%20%40' 'db-1.internal.example/dispatch_prod' postgresql)" "$(url_without_user_credential 'dispatch.ro' 'db-1.internal.example/dispatch_prod' postgresql)")" \
  "$(accepted_case 'süß-'"canary" "$(url_with_credential "$encoded_unicode" '10.0.0.7:65535/dispatch')" "$(url_without_credential '10.0.0.7:65535/dispatch')")" \
  "$(accepted_case '' "$(url_without_credential 'db.test/dispatch')" "$(url_without_credential 'db.test/dispatch')")"; do
  IFS='|' read -r case_password case_url case_client_url <<<"$case"
  case_begin "an accepted DATABASE_URL ($case_url)"
  fresh_env "$case_url"
  if output=$(LC_ALL=en_US.UTF-8 test_password="$case_password" deploy_once); then
    fail "deployer did not reach the origin check with $case_url"
  fi
  no_secret "$case_url"
  if [[ "$output" != *"rolling back to old-image"* ]] ||
    [[ $(awk -v url="$case_client_url" '$NF == url' "$argv" | wc -l) -ne 3 ]] ||
    { [[ -n "$case_password" ]] && grep -qF -- "$case_password" "$argv"; }; then
    fail "deployer did not hand the clients $case_client_url, with the credential as PGPASSWORD alone"
  fi
done
