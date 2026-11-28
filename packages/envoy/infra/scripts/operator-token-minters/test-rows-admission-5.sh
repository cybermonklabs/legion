# shellcheck shell=bash
# shellcheck disable=SC2016,SC2154 # the world, its names and its helpers are the test's; each single-quoted $name is a jq variable, not the shell's
# operator-token-minters/test-rows-admission-5.sh: the rows of operator-token-minters.test.sh on
# the reviews: their answer as kubectl create writes it, review's kubectl calls on a cluster, and
# the CPU lists their items are pinned by. Sourced by the test after its world and helpers; its
# header says what the fakes model. The rows:
#
#   (cy) the answer to the reviews, as kubectl create writes it, one document per review: two or
#        more so answered are judged, asked in one call or in four at once, the four each on one
#        CPU; an answer that repeats one review and leaves out another, or that holds a document
#        without a status, reads UNKNOWN naming the count; and a follow round whose answer refuses
#        every review, its witness among them, or holds no status, reads UNKNOWN for the witness
#        rather than pass a seeded chain;
#   (cz) review's kubectl calls on a cluster are REVIEW_WORKERS, capped at the nominal seats the
#        API server gives global-default, each on one CPU; one call when its /metrics cannot be
#        read or holds no such seats; UNKNOWN for a REVIEW_WORKERS that is no whole number above 0;
#        and UNKNOWN when one later batch alone is refused or answers short;
#   (de) the first CPU of a Cpus_allowed_list, and whether it is one CPU, which (cy) and (cz) pin
#        their items by, are read right from eight lists Linux printed.

# --- The pin of rows (cy) and (cz). Their items that set REVIEW_WORKERS=4 have review start its
# kubectl calls at once, one for each chunk, and wait on each, which is what they test (the chunks,
# the wait on each call's pid, the first refusal in the order asked, the order of the answers), so
# the calls overlap. CI runs each part as a pool entry of its own, which the pool counts as one
# slot, so each such item pins its own process to one CPU before its first run (pin_one_cpu): the
# check, the fakes and the calls inherit the pin, and still start at once and overlap, time-sliced
# on that one core. The pin is proved during the overlap: the item marks its world
# review-record-cpus, on which the fake kubectl writes each review call's own Cpus_allowed_list
# beside it (review-call-<n>.cpus), and after each run want_one_cpu_calls holds every call to one
# CPU. ---
# pin_one_cpu WHAT: this item's process ($BASHPID) pinned to the first CPU of its own
# Cpus_allowed_list, read by the shell from /proc/self/status (never a fixed CPU: a runner's cpuset
# need not hold CPU 0), and its world marked review-record-cpus. Where taskset is missing, or the
# status file holds no Cpus_allowed_list line, it fails naming WHAT and that; it never skips.
pin_one_cpu() {
  local key value cpus="" cpu
  command -v taskset >/dev/null || fail "$1: taskset is missing, so the item cannot pin review's calls to one CPU"
  while read -r key value; do
    [ "$key" != Cpus_allowed_list: ] || cpus="$value"
  done </proc/self/status
  [ -n "$cpus" ] || fail "$1: /proc/self/status holds no Cpus_allowed_list line, so the item cannot pin review's calls to one CPU"
  cpu="$(cpus_first "$cpus")"
  taskset -cp "$cpu" "$BASHPID" >/dev/null || fail "$1: taskset could not pin the item to CPU $cpu of $cpus"
  : >"$world/clusters/operator-cluster/review-record-cpus"
}
# want_one_cpu_calls WHAT: each review call the fake recorded on the operator's cluster
# (review-call-<n>.json) has its Cpus_allowed_list beside it (review-call-<n>.cpus), and it is one
# CPU; fails naming WHAT, each call that has none or more than one CPU, and its list.
want_one_cpu_calls() {
  local call cpus wrong=""
  for call in "$world/clusters/operator-cluster"/review-call-*.json; do
    [ -e "$call" ] || continue
    call="${call%.json}"
    if [ ! -e "$call.cpus" ]; then
      wrong+="; ${call##*/} wrote no list of its CPUs"
      continue
    fi
    cpus=""
    read -r cpus <"$call.cpus" || true
    cpus_one "$cpus" || wrong+="; ${call##*/} ran with CPUs ${cpus:-none} allowed"
  done
  [ -z "$wrong" ] || fail "$1: a review call was not held to one CPU: ${wrong#; }"
}

# --- (cy) The answer to the reviews, as kubectl create writes it: given a List, kubectl creates each
# object on its own and prints one JSON document per object, never a List (kubectl 1.35.9's
# create.go; measured at 1.36.4 against a mock API server: a List of 500 reviews made 500 POSTs and
# printed 500 documents). review-answer.jq reads that answer, holds each document to a
# SubjectAccessReview with an object status, and counts the principal-right pairs it answers against
# the reviews asked. The items:
#   judged: two reviews so answered are judged, and the grant in them is a finding;
#   repeated: the answer repeats the first review in place of the last, keeping the count, so one
#     pair is missing: UNKNOWN naming the pairs answered, the reviews asked and the documents;
#   witness-copied: the witness's reviews are copies of another, which is the same fault;
#   follow-refused and follow-nostatus: the step's own reviews are answered as they are, and each
#     follow_becomes round after them refuses every review, the witness's among them, or holds no
#     status; the round reads UNKNOWN for the witness rather than pass the seeded chain.
# Run by hand, the control is the script and its parts as at dabc806da7, whose review held the
# answer to is_list: judged reads "UNKNOWN live: RBAC on operator-cluster: the API server's answer
# to the reviews could not be read: the answer is JSON, but not one object with a list of items",
# exit 2, so no live step could ever judge a review; and with the fake's List of that head, which
# that code reads, follow-refused and follow-nostatus exit 0 with "PASS live: RBAC on
# operator-cluster … none allowed", where the chain is a finding. ---
review_answer_row() {
  local dir="$world/clusters/operator-cluster" chain
  base_world
  case "$1" in
    judged | chunked | repeated | witness-copied)
      subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" envoy-operator ""
      ;;
    follow-refused | follow-nostatus)
      # A seeded chain: kube-system/generic-garbage-collector may create the operator's token, and
      # legion/worker may create pods in kube-system, so follow_becomes finds the chain in a round
      # after the step's own reviews.
      subject_grant operator-cluster "$(sa_subject kube-system generic-garbage-collector)" create "" serviceaccounts token envoy-operator envoy-operator
      subject_grant operator-cluster "$(sa_subject legion worker)" create "" pods "" kube-system ""
      ;;
  esac
  case "$1" in
    repeated) : >"$dir/review-repeated" ;;
    witness-copied) : >"$dir/review-witness-copied" ;;
    follow-refused) : >"$dir/review-follow-refused" ;;
    follow-nostatus) : >"$dir/review-follow-nostatus" ;;
  esac
  # The chunked item asks the same reviews in four kubectl calls at once, as a live run does with
  # REVIEW_WORKERS workers: the finding and the run's exit are the same, and review reads the
  # chunks back in the order it asked them. Its process is pinned to one CPU first, so that the
  # part takes no more than the one slot the pool counts for it while the four calls still overlap,
  # and each call is held to that CPU after the run (the pin above).
  if [ "$1" = chunked ]; then
    pin_one_cpu "two or more reviews answered as separate documents ($1)"
    REVIEW_WORKERS=4 run
    want_one_cpu_calls "two or more reviews answered as separate documents ($1)"
  else
    run
  fi
  case "$1" in
    judged | chunked)
      want_status 1 "two or more reviews answered as separate documents ($1)"
      want_line "FINDING system:serviceaccount:legion:worker: create pods in envoy-operator" \
        "two or more reviews answered as separate documents ($1)"
      ! has_prefix "UNKNOWN live: RBAC on operator-cluster: the API server's answer to the reviews" ||
        fail "two or more reviews answered as separate documents ($1): the answer could not be read"
      ;;
    repeated | witness-copied)
      want_rbac_unknown "the API server answered " "an answer that repeats one review and leaves out another ($1)"
      want_text " of the " "an answer that repeats one review and leaves out another ($1)"
      want_text " reviews asked, in " "an answer that repeats one review and leaves out another ($1)"
      ;;
    follow-refused | follow-nostatus)
      chain="create pods in kube-system, so it can become system:serviceaccount:kube-system:generic-garbage-collector"
      if [ "$1" = follow-refused ]; then
        want_rbac_unknown "the witness review, the cluster-admin binding's group system:masters, was not allowed, so a refusal from this API server proves nothing" \
          "a follow round answered $1, where the seeded chain would go unseen"
      else
        want_rbac_unknown "the API server's answer to the reviews could not be read: " \
          "a follow round answered $1, where the seeded chain would go unseen"
        want_text "is not a SubjectAccessReview with a status" "a follow round answered $1"
      fi
      ! has_prefix "FINDING system:serviceaccount:legion:worker: $chain" ||
        fail "a follow round answered $1: the chain was still found, so the item plants nothing"
      ;;
  esac
}
if row cy; then
  each review_answer_row judged chunked repeated witness-copied follow-refused follow-nostatus
  passed "(cy) reviews answered as one document each are judged, asked in one call or in four at once; an answer that repeats one and leaves out another reads UNKNOWN naming the count; and a follow round whose answer is all refused or has no status reads UNKNOWN for its witness"
fi

# --- (cz) review's calls on a cluster are REVIEW_WORKERS, capped at the nominal seats the API server
# gives the priority level global-default (its /metrics): asked with REVIEW_WORKERS=4 in a clean
# world, a cluster with 2 seats gets 2 calls and one with 40 gets 4; a /metrics refused, or one
# holding no global-default seats, gets one call; each run passes its step as one call does. A
# REVIEW_WORKERS that is no whole number above 0 reads UNKNOWN naming it, exit 2. With four batches,
# one later batch alone (one that does not hold the witness, read from a first run's recorded
# requests) refused, or answering one review short, reads UNKNOWN, exit 2. Every item pins its
# process to one CPU before its first run, so that the part takes no more than the one slot the pool
# counts for it while the calls still overlap, and after each run holds each review call to that CPU
# (the pin above); the later items' reset between their two runs removes each call's CPU list with
# its other records, so the second run's calls are judged by their own. Run by hand, the control is
# reviews.sh with review_workers' cap taken out (WORKERS set to REVIEW_WORKERS): the capped item
# gets 4 calls, and the refused and no-seats items 4; and reviews.sh waiting on batch 0's call
# alone: the later-refused item passes its step. ---
worker_cap_row() {
  local dir="$world/clusters/operator-cluster" calls want later file
  base_world
  pin_one_cpu "the review calls of item $1"
  case "$1" in
    capped) echo 2 >"$dir/global-default-seats"; want=2 ;;
    upper) echo 40 >"$dir/global-default-seats"; want=4 ;;
    refused) : >"$dir/metrics-refused"; want=1 ;;
    no-seats) : >"$dir/metrics-without-seats"; want=1 ;;
    bad-workers)
      REVIEW_WORKERS=abc run
      want_one_cpu_calls "a REVIEW_WORKERS that is no number"
      want_rbac_unknown "REVIEW_WORKERS is not a whole number from 1 to 9999, so the reviews were not asked" \
        "a REVIEW_WORKERS that is no number"
      return
      ;;
    later-refused | later-short)
      REVIEW_WORKERS=4 run
      want_one_cpu_calls "the first run of $1"
      # The witness's line is the first of its batch, so `cut | grep -q` under pipefail could fail
      # with cut's SIGPIPE once grep matched it, and read the witness's batch as a later one: the
      # item would then refuse batch 0, and pass a reviews.sh that waits on batch 0's call alone.
      # grep reads the subjects through a process substitution instead.
      for file in "$dir"/review-requests-*.tsv; do
        if ! grep -qxF group:system:masters < <(cut -f7 "$file"); then later="$(cut -f7 "$file" | sed -n 1p)"; break; fi
      done
      [ -n "$later" ] || fail "no batch of the first run went without the witness, so the item ($1) proves nothing"
      rm -f "$dir"/review-call-*.json "$dir"/review-call-*.cpus "$dir"/review-requests-*.tsv "$dir/review-calls"
      if [ "$1" = later-refused ]; then
        printf '%s\n' "$later" >"$dir/review-refuse-holding"
        want="the API server refused the review (it needs create on subjectaccessreviews"
      else
        printf '%s\n' "$later" >"$dir/review-short-holding"
        want="the API server answered "
      fi
      REVIEW_WORKERS=4 run
      want_one_cpu_calls "the second run of $1"
      want_rbac_unknown "$want" "the batch holding $later alone, of four, $1"
      return
      ;;
  esac
  REVIEW_WORKERS=4 run
  want_one_cpu_calls "a clean world asked in capped calls ($1)"
  want_status 0 "a clean world asked in capped calls ($1)"
  want_text "PASS live: RBAC on operator-cluster: " "a clean world asked in capped calls ($1)"
  calls=$(find "$dir" -maxdepth 1 -name 'review-call-*.json' | wc -l)
  [ "$calls" -eq "$want" ] ||
    fail "the review on operator-cluster ($1) was asked in $calls kubectl calls, want $want"
}
if row cz; then
  each worker_cap_row capped upper refused no-seats bad-workers later-refused later-short
  passed "(cz) review's calls on a cluster are REVIEW_WORKERS capped at the API server's global-default seats (2 seats: 2 calls of 4; 40 seats: 4), one call when the seats cannot be read, UNKNOWN for a REVIEW_WORKERS that is no number, and UNKNOWN when one later batch alone is refused or answers short"
fi

# --- (de) The CPU lists rows (cy) and (cz) pin their items by. cpus_first and cpus_one read a
# Cpus_allowed_list value of /proc/<pid>/status: its first CPU, and whether it is one CPU. They are
# held to eight lists Linux printed, captured on build nodes on 2026-11-28
# (dispatch://INFRA-6357/artifact/cpus-allowed-fixtures-md, version 3): each line but the first
# was printed by `taskset -c <list> grep Cpus_allowed_list /proc/self/status`, and the first, the
# node's whole list, by the grep without taskset. Each line below is a list, its first CPU, and
# whether it is one CPU. The row runs no check. ---
if row de; then
  de_wrong=()
  while read -r de_list de_first de_one; do
    de_got="$(cpus_first "$de_list")"
    [ "$de_got" = "$de_first" ] || de_wrong+=("cpus_first read the first CPU of $de_list as $de_got, want $de_first")
    if cpus_one "$de_list"; then de_got="one CPU"; else de_got="more than one CPU"; fi
    [ "$de_got" = "$de_one" ] || de_wrong+=("cpus_one read $de_list as $de_got, want $de_one")
  done <<'LISTS'
0-31 0 more than one CPU
3 3 one CPU
0,2-3 0 more than one CPU
1,5,7-9 1 more than one CPU
0,2 0 more than one CPU
2,5 2 more than one CPU
2-3,5 2 more than one CPU
1-2,4-5 1 more than one CPU
LISTS
  [ "${#de_wrong[@]}" -eq 0 ] || { printf -v de_text '%s; ' "${de_wrong[@]}"; fail "the CPU lists Linux printed were misread: ${de_text%; }"; }
  passed "(de) the first CPU of a Cpus_allowed_list and whether it is one CPU are read from eight lists Linux printed"
fi
