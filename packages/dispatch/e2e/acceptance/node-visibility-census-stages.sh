# shellcheck shell=bash
# The sections of node-visibility-census.sh (check c-census) from H3 to its end, which that driver
# sources from beside it as its last command, after node-visibility-census-lib.sh. Sourced, never
# run.

# --- H3: this head and the backfill again, and the census ---------------------------------------

boot_head H3 "this head's server again"
registered_commit H3 "read this head's row of server_processes"
[[ "$answer" == "$head_commit" ]] || fail H3 "this head serves the database again" "its row names '$answer', want $head_commit"
pass H3 "this head serves the database again at $base, stamped with $head_commit"
fleet_reads H3 "$head_commit"
# A process announces the build the fleet serves only once processes.Live reads every live process
# as serving its own commit (startup.Announcer, servedByEvery). While both builds served (B2) the
# fleet agreed on none, so neither wrote a row then; the base build wrote its row once this head's
# server stopped, which fleet_reads B2 read. So the window of two builds adds no announcement.
want_value H3 "processes.Live read the base build and this head as two builds: the announcements, in their order, name the base build, this head, the base build after the rollback and this head again" \
  "$baseline_commit $head_commit $baseline_commit $head_commit" \
  "select coalesce(string_agg(commit, ' ' order by seq), '') from server_build_announcements"

# The census beside a planted instance of what it must find (the Verification preamble): before
# the backfill, run $run_new holds the row the base build wrote and no interaction node, and the
# terminal frame the base build's sweep appended to run $run_ahead holds its going_events row and
# no frame value. This census must name both, so the zero readings after the backfill read the
# store, not a census that finds nothing.
query H3 "read the sequence of the terminal frame the base build's sweep appended to run $run_ahead" \
  "select coalesce(string_agg(sequence::text, ' '), '') from going_events where request_id = '$run_ahead' and name in ('end', 'error')"
readonly ahead_frame="$answer"
[[ "$ahead_frame" =~ ^[0-9]+$ ]] || fail H3 "run $run_ahead holds one terminal frame" "the sequences of its terminal frames read '$ahead_frame'"
want_value H3 "before the backfill, run $run_new holds its row and no interaction node, and run $run_ahead holds the row of its terminal frame $ahead_frame and no frame value" \
  "1|0|1|0" \
  "select (select count(*) from going_requests where id = '$run_new') || '|'
          || (select count(*) from graph_nodes where kind = 'interaction' and id = '$run_new') || '|'
          || (select count(*) from going_events where request_id = '$run_ahead' and sequence = $ahead_frame) || '|'
          || (select count(*) from graph_tuples where from_kind = 'interaction' and from_id = '$run_ahead' and kind = 'frame')"
take_census H3 planted 1
grep -Fqx "census: store kind=interaction difference at interaction $run_new: the row names a node the store does not hold" \
  "$work/census.planted" ||
  fail H3 "the census names run $run_new, a row with no node, a difference of the kind interaction" \
    "it reads: $(grep -E '^census: store kind=interaction ' "$work/census.planted" | tr '\n' ' ')"
pass H3 "the census names run $run_new, a row with no node, a difference of the kind interaction: the row names a node the store does not hold"
grep -Eq "^census: store kind=frame difference at frame $run_ahead $ahead_frame: the row and its node disagree: the row digests [0-9a-f]{32} and the node \\(none\\)\$" \
  "$work/census.planted" ||
  fail H3 "the census names the terminal frame $ahead_frame of run $run_ahead, a row with no frame value, a difference of the kind frame" \
    "it reads: $(grep -E '^census: store kind=frame ' "$work/census.planted" | tr '\n' ' ')"
pass H3 "the census names the terminal frame $ahead_frame of run $run_ahead, a row with no frame value, a difference of the kind frame: its node holds none"
line="$(grep -m 1 -E '^census: store rows_checked=' "$work/census.planted")" ||
  fail H3 "the census reads its total of differences" "no total in: $(<"$work/census.planted")"
[[ ! "$line" =~ differences=0\ read_seq=[0-9]+$ ]] || fail H3 "the census reads differences while the two are planted" "$line"
pass H3 "the census reads differences while the two are planted, beside its zero readings after the backfill: $line"

backfill H3
nodes_json H3 "read the store's interaction nodes" "$work/store.h3.json"
runs_json H3 "read going_requests and going_events" "$work/table.h3.json"
jq -e --arg id "$run_new" --slurpfile table "$work/table.h3.json" \
  '(.[] | select(.id == $id)) as $node | ($table[0][] | select(.id == $id)) as $row
   | $node.state == "live" and $node.visibility == "author" and $node.author == $row.author and $node.words == $row.words
     and $node.frames == $row.frames and ($row.frames | length) > 0' "$work/store.h3.json" >/dev/null ||
  fail H3 "the store holds a node for run $run_new, with a frame value for each frame the base build wrote" \
    "node: $(jq -c --arg id "$run_new" '.[] | select(.id == $id)' "$work/store.h3.json"); row: $(jq -c --arg id "$run_new" '.[] | select(.id == $id)' "$work/table.h3.json")"
pass H3 "the store holds a node for run $run_new, which the base build recorded, with a frame value for each of its $(jq --arg id "$run_new" '.[] | select(.id == $id) | .frames | length' "$work/table.h3.json") frame(s)"
jq -e --arg id "$run_ahead" --slurpfile table "$work/table.h3.json" \
  '(.[] | select(.id == $id)) as $node | ($table[0][] | select(.id == $id)) as $row
   | $node.frames == $row.frames and ($row.frames | map(select(.name == "error")) | length) == 1' "$work/store.h3.json" >/dev/null ||
  fail H3 "the store holds a frame value for the terminal frame the base build's sweep appended to run $run_ahead" \
    "node: $(jq -c --arg id "$run_ahead" '.[] | select(.id == $id)' "$work/store.h3.json"); row: $(jq -c --arg id "$run_ahead" '.[] | select(.id == $id)' "$work/table.h3.json")"
pass H3 "the store holds a frame value for the terminal frame the base build's sweep appended to run $run_ahead"
jq -e --slurpfile table "$work/table.h3.json" \
  '([.[] | {key: .id, value: .frames}] | from_entries) as $nodes
   | all($table[0][]; .frames == $nodes[.id])' "$work/store.h3.json" >/dev/null ||
  fail H3 "the store holds a frame value for every frame going_events holds" \
    "$(jq -c --slurpfile nodes "$work/store.h3.json" '[.[] | .id as $id | select(.frames != ($nodes[0][] | select(.id == $id) | .frames))]' "$work/table.h3.json")"
pass H3 "the store holds a frame value for every frame going_events holds, $(jq '[.[].frames[]] | length' "$work/table.h3.json") frame(s) of $(jq 'length' "$work/table.h3.json") run(s)"
want_value H3 "the interaction of run $run_dated, whose row the base build deleted, is at state gone, and no row of it is left" "gone|0|0" \
  "select n.state || '|' || (select count(*) from going_requests where id = '$run_dated')
          || '|' || (select count(*) from going_events where request_id = '$run_dated')
   from graph_nodes n where n.kind = 'interaction' and n.id = '$run_dated'"
# The inventory again: each of its runs but the one the base build deleted is in both products.
jq -c --arg gone "$run_dated" '[.[] | select(.id != $gone)]' "$work/inventory.json" >"$work/inventory.h3.json" ||
  fail H3 "read the inventory but run $run_dated" "unreadable: $(head -c 600 "$work/inventory.json")"
jq -r --slurpfile store "$work/store.h3.json" --slurpfile table "$work/table.h3.json" "$against_inventory_jq" \
  "$work/inventory.h3.json" >"$work/lost.h3" || fail H3 "hold the store and the going tables to the inventory" "jq refused it"
[[ ! -s "$work/lost.h3" ]] ||
  fail H3 "the store and the going tables still hold every run of the inventory the base build did not delete" "$(tr '\n' ' ' <"$work/lost.h3")"
pass H3 "the store and the going tables still hold the $(jq 'length' "$work/inventory.h3.json") runs of the inventory the base build did not delete"

# A node at proposed, settled or overtaken is compared with its row like a live one (R12): alice
# moves her pre-mortem to proposed, the trade-off analysis's finding to proposed and then settled,
# and her risk to overtaken, and the census below counts each among the nodes of its kind and
# reads zero differences.
readonly finding_store="${finding_node/\#b-/#}"
sign_in H3 "$alice" " at $base"
move_state H3 "$premortem" "lens/$premortem_id" live proposed
move_state H3 "$finding" "$finding_store" live proposed
move_state H3 "$finding" "$finding_store" proposed settled
move_state H3 "$risk" "risk/$risk_id" live overtaken

take_census C h3 0
census_zero C h3 comment
census_zero C h3 interaction
census_zero C h3 frame
census_zero C h3 block
census_zero C h3 block_ref
census_zero C h3 person
census_zero C h3 session
census_zero C h3 role
census_zero C h3 page
for kind in "${populated_kinds[@]}"; do
  census_zero C h3 "$kind"
done
for kind in interaction lens finding risk constraint lens_kind; do
  census_zero C h3 "${kind}_visibility_state"
done
census_differences C h3
# The census counted each moved node as it counts a live one: the nodes of the kind it checked are
# every node of the kind not gone, more than those at live.
for moved in "lens|lens|$premortem_id|proposed" "finding|block|$finding_store|settled" "risk|risk|$risk_id|overtaken"; do
  IFS='|' read -r kind node_kind id state <<<"$moved"
  narrow="kind = '$node_kind'"
  [[ "$kind" != finding ]] || narrow="$narrow and id like 'lens/%'"
  query C "count the $kind nodes not gone and those at live" \
    "select count(*) filter (where state <> 'gone') || '|' || count(*) filter (where state = 'live') || '|'
            || coalesce(max(state) filter (where id = '$id'), 'none')
     from graph_nodes where $narrow"
  IFS='|' read -r present live held <<<"$answer"
  census_kind "$work/census.h3" "${kind}_visibility_state" ||
    fail C "the census reads the kind ${kind}_visibility_state" "no line of it in: $(<"$work/census.h3")"
  [[ "$held" == "$state" && "$c_store" == "$present" && "$present" -gt "$live" ]] ||
    fail C "the census counts the $kind $id at $state among the nodes of its kind, as it counts a live one" \
      "the store holds it at $held, $present $kind node(s) are not gone and $live are live, and the census checked $c_store"
  pass C "the census counts the $kind $id at $state among the $c_store $kind node(s) it checked, every one not gone, $live of them live, at zero differences"
done
want_value C "every interaction node is at visibility author" "0|true" \
  "select count(*) filter (where visibility <> 'author') || '|' || (count(*) > 0)::text
   from graph_nodes where kind = 'interaction'"
want_value C "the interaction of run $run_dated, whose row is gone, is gone and no difference" "gone" \
  "select state from graph_nodes where kind = 'interaction' and id = '$run_dated'"
# The person of the identity table who asks the command bar and writes no comment.
want_value C "$carol writes no comment" 0 \
  "select count(*) from comments where user_threads_login(coalesce(author->>'owner', author->>'id', '')) = '$carol'"
want_value C "the store holds one person node for every spelling of $carol, one for $twin, and the session proved hers" "1|1|1" \
  "select (select count(*) from graph_nodes where kind = 'person' and lower(btrim(id)) = '$carol') || '|'
          || (select count(*) from graph_nodes where kind = 'person' and id = '$twin') || '|'
          || (select count(*) from graph_nodes where kind = 'session' and id = '$carol_session')"
# The census's person and session node sides are every node of each kind (graphcensus's person and
# session kinds), so had a row side not held $carol, $twin or $carol_session, whose nodes the check
# above read, that node would be a node without a row, on which census_zero above fails.
pass C "the census reads no person or session node without a row while $carol, $twin and her session $carol_session are nodes, so its row sides hold all three too: each is on both sides"
want_value C "the store still holds the node of the session $bearer_session, the writer the lens on the shared bearer names" 1 \
  "select count(*) from graph_nodes where kind = 'session' and id = '$bearer_session'"
# As at H2: the census's session node side is every session node, so a session node off its row
# side would be a node without a row, on which census_zero above fails.
pass C "the census reads no session node without a row while $bearer_session is a session node, so its row side still holds that session: it is on both sides"

# The census beside two differences planted after its zero reading (Verification v53): with psql,
# one interaction node no going_requests row projects - run $run_bob's node copied under an id no
# row names, every column of its row and every value tuple the store holds of a run but its frames
# (its visibility, its state, its words and its ends_at), so the census of a run's visibility and
# state reads the copy as it reads any run - and one frame value no going_events row projects, of
# run $run_ahead, whose one going_events row is its terminal frame $ahead_frame, at the sequence
# after that frame's, stated by the writer of that frame's value. The census names each as one
# difference of its kind naming its run, two in all: the copy, a node no row projects, and the row
# of run $run_ahead's frame $ahead_frame, whose node's frames no longer digest as its rows do. Both
# are deleted, exactly as planted, and the census reads zero again.
want_value C "run $run_ahead holds one going_events row, its terminal frame $ahead_frame, and its node one frame value" "1|1" \
  "select (select count(*) from going_events where request_id = '$run_ahead') || '|'
          || (select count(distinct t.to_id) from graph_tuples t
              where t.from_kind = 'interaction' and t.from_id = '$run_ahead' and t.kind = 'frame' and t.to_kind = 'value' and t.asserted)"
with_vars C "plant an interaction node no going_requests row projects, a copy of run $run_bob's node" "
  with planted as (select gen_random_uuid()::text as id),
  node as (
    insert into graph_nodes
    select copied.*
    from graph_nodes n cross join planted
    cross join lateral jsonb_populate_record(null::graph_nodes, to_jsonb(n) || jsonb_build_object('id', planted.id)) copied
    where n.kind = 'interaction' and n.id = :'source'
    returning id
  ),
  tuples as (
    insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
    select t.from_kind, node.id, t.kind, t.to_kind, t.to_id, t.at, t.who, t.source_event, t.legacy_source_seq, t.asserted
    from graph_tuples t cross join node
    where t.from_kind = 'interaction' and t.from_id = :'source' and t.to_kind = 'value' and t.kind <> 'frame'
    returning seq
  )
  select node.id || '|' || (select string_agg(seq::text, ',' order by seq) from tuples) from node" "source=$run_bob"
IFS='|' read -r planted_run planted_seqs <<<"$answer"
[[ "$planted_run" =~ ^[0-9a-f-]{36}$ && "$planted_seqs" =~ ^[0-9]+(,[0-9]+)*$ ]] ||
  fail C "plant an interaction node no going_requests row projects, a copy of run $run_bob's node" "psql answered '$answer'"
readonly planted_run planted_seqs
want_value C "no going_requests row names the planted run $planted_run, and its node holds a visibility and a state tuple" "0|1|1" \
  "select (select count(*) from going_requests where id::text = '$planted_run') || '|'
          || (select count(*) from graph_tuples where from_kind = 'interaction' and from_id = '$planted_run' and kind = 'visibility') || '|'
          || (select count(*) from graph_tuples where from_kind = 'interaction' and from_id = '$planted_run' and kind = 'state')"
with_vars C "plant a frame value of run $run_ahead that no going_events row projects" "
  insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
  select 'interaction', :'run', 'frame', 'value',
         jsonb_build_object('sequence', max(e.sequence) + 1, 'name', 'end',
                            'data', to_jsonb('A frame value no going_events row projects.'::text))::text,
         now(),
         (select t.who from graph_tuples t
          where t.from_kind = 'interaction' and t.from_id = :'run' and t.kind = 'frame' order by t.seq limit 1),
         0, null, true
  from going_events e where e.request_id = :'run'::uuid
  returning seq || '|' || (to_id::jsonb ->> 'sequence')" "run=$run_ahead"
IFS='|' read -r planted_frame_seq planted_frame <<<"$answer"
[[ "$planted_frame_seq" =~ ^[0-9]+$ && "$planted_frame" == $((ahead_frame + 1)) ]] ||
  fail C "plant a frame value of run $run_ahead at sequence $((ahead_frame + 1)), which no going_events row holds" "psql answered '$answer'"
readonly planted_frame_seq planted_frame
pass C "planted the interaction node $planted_run, a copy of run $run_bob's node under an id no row names, and a frame value of run $run_ahead at sequence $planted_frame, which no going_events row holds"

take_census C plants 1
census_kind "$work/census.plants" interaction ||
  fail C "the census reads the kind interaction" "no line of it in: $(<"$work/census.plants")"
found="$(grep -E '^census: store kind=interaction difference at ' "$work/census.plants" || true)"
want_re="^census: store kind=interaction difference at interaction $planted_run: an? [a-z]+ node no row projects\$"
[[ "$c_row_differences" == 0 && "$c_store_differences" == 1 && "$found" =~ $want_re ]] ||
  fail C "the census reads the planted node as one difference of the kind interaction, naming the run $planted_run" \
    "$c_row_differences row(s) without a node and $c_store_differences node(s) without a row: ${found:-no difference named}"
pass C "the census reads the planted node as one difference of the kind interaction, naming the run: $found"
census_kind "$work/census.plants" frame ||
  fail C "the census reads the kind frame" "no line of it in: $(<"$work/census.plants")"
found="$(grep -E '^census: store kind=frame difference at ' "$work/census.plants" || true)"
want_re="^census: store kind=frame difference at frame $run_ahead $ahead_frame: the row and its node disagree: the row digests [0-9a-f]{32} and the node [0-9a-f]{32}\$"
[[ "$c_row_differences" == 1 && "$c_store_differences" == 0 && "$found" =~ $want_re ]] ||
  fail C "the census reads the planted frame value as one difference of the kind frame, naming the run $run_ahead" \
    "$c_row_differences row(s) without a node and $c_store_differences node(s) without a row: ${found:-no difference named}"
pass C "the census reads the planted frame value as one difference of the kind frame, naming the run: $found"
census_zero C plants interaction_visibility_state
line="$(grep -m 1 -E '^census: store rows_checked=' "$work/census.plants")" ||
  fail C "the census reads its total of differences" "no total in: $(<"$work/census.plants")"
[[ "$line" =~ differences=2\ read_seq=[0-9]+$ ]] ||
  fail C "the census reads the two planted differences and no other" "$line: $(grep -E '^census: store kind=[a-z_]+ difference' "$work/census.plants" | tr '\n' ' ')"
pass C "the census reads the two planted differences and no other: $line"

IFS=, read -r -a planted_tuples <<<"$planted_seqs,$planted_frame_seq"
with_vars C "delete the planted node, its tuples and the planted frame value" "
  with tuples as (
    delete from graph_tuples where seq = any(string_to_array(:'seqs', ',')::bigint[]) returning 1
  ),
  node as (
    delete from graph_nodes where kind = 'interaction' and id = :'planted' returning 1
  )
  select (select count(*) from tuples) || '|' || (select count(*) from node)" \
  "seqs=$planted_seqs,$planted_frame_seq" "planted=$planted_run"
[[ "$answer" == "${#planted_tuples[@]}|1" ]] ||
  fail C "delete exactly the ${#planted_tuples[@]} planted tuples and the planted node" "psql deleted '$answer' (tuples|nodes)"
want_value C "no node and no tuple names the planted run $planted_run, and run $run_ahead's node holds its one frame value again" "0|0|1" \
  "select (select count(*) from graph_nodes where kind = 'interaction' and id = '$planted_run') || '|'
          || (select count(*) from graph_tuples where (from_kind = 'interaction' and from_id = '$planted_run')
                                                   or (to_kind = 'interaction' and to_id = '$planted_run')) || '|'
          || (select count(distinct t.to_id) from graph_tuples t
              where t.from_kind = 'interaction' and t.from_id = '$run_ahead' and t.kind = 'frame' and t.to_kind = 'value' and t.asserted)"
take_census C unplanted 0
census_zero C unplanted interaction
census_zero C unplanted frame
census_zero C unplanted interaction_visibility_state
census_differences C unplanted

# --- S: the store-only kinds against the inventory of the writes and the migration's seed ---------

query S "read the store-only kinds and their tuples" "with current as (
    select distinct on (t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id)
           t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted
    from graph_tuples t
    where t.kind in ('constrained_by', 'has_kind', 'triggered_by', 'prevented_by')
    order by t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.seq desc
  ),
  stated as (
    select c.kind, c.from_kind || ':' || c.from_id || ' ' || c.kind || ' ' || c.to_kind || ':' || c.to_id as tuple
    from current c where c.asserted
  ),
  nodes as (
    select case when n.kind = 'block' then 'finding' else n.kind end as kind, n.id
    from graph_nodes n
    where n.state <> 'gone'
      and (n.kind in ('lens', 'risk', 'constraint', 'lens_kind') or (n.kind = 'block' and n.id like 'lens/%'))
  )
  select json_object_agg(listed.kind, listed.items) from (
    select kinds.kind, coalesce((select json_agg(nodes.id order by nodes.id) from nodes where nodes.kind = kinds.kind), '[]') as items
    from unnest(array['lens', 'finding', 'risk', 'constraint', 'lens_kind']) kinds (kind)
    union all
    select kinds.kind, coalesce((select json_agg(stated.tuple order by stated.tuple) from stated where stated.kind = kinds.kind), '[]')
    from unnest(array['constrained_by', 'has_kind', 'triggered_by', 'prevented_by']) kinds (kind)
  ) listed"
printf '%s\n' "$answer" >"$work/store-only.json"
for kind in lens finding risk constraint lens_kind constrained_by has_kind triggered_by prevented_by; do
  want="$(jq -c --arg k "$kind" '.[$k] | sort' "$work/writes.json")"
  got="$(jq -c --arg k "$kind" '.[$k] | sort' "$work/store-only.json")"
  [[ "$got" == "$want" ]] ||
    fail S "the store's $kind is the inventory's" "the store holds $got, the inventory of the writes and the seed $want"
  pass S "the store holds the $(jq 'length' <<<"$want") $kind the inventory of the routes' writes and the migration's seed holds, and no other"
done

# --- I: the identity table against each made_by, authored_by and author --------------------------

for entry in "${identity_runs[@]}"; do
  IFS='|' read -r id form wanted person what <<<"$entry"
  with_vars I "read run $id" \
    "select json_build_object('made_by', n.made_by, 'author', r.author,
       'authored_by', (select coalesce(json_agg(held.to_kind || ':' || held.to_id order by held.to_id), '[]') from (
          select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted from graph_tuples t
          where t.from_kind = 'interaction' and t.from_id = n.id and t.kind = 'authored_by'
          order by t.to_kind, t.to_id, t.seq desc) held where held.asserted),
       'person', (select count(*) from graph_nodes p where p.kind = 'person' and p.id = :'person'),
       'alias', (select count(*) from graph_tuples a where a.from_kind = 'session' and a.from_id = split_part(:'wanted', ':', 2)
                   and a.kind = 'alias_of' and a.to_kind = 'person' and a.to_id = :'person' and a.asserted))
     from graph_nodes n left join going_requests r on r.id::text = n.id
     where n.kind = 'interaction' and n.id = :'id'" "id=$id" "person=$person" "wanted=$wanted"
  [[ -n "$answer" ]] || fail I "the store holds run $id ($what)" "no interaction node of it"
  printf '%s\n' "$answer" >"$work/identity.json"
  jq -e --argjson form "$form" '.made_by == $form' "$work/identity.json" >/dev/null ||
    fail I "run $id's made_by is the form the table wrote ($what)" "made_by $(jq -c '.made_by' "$work/identity.json"), the table $form"
  jq -e --argjson form "$form" '.author == $form' "$work/identity.json" >/dev/null ||
    fail I "run $id's going_requests.author, which projects its made_by, is the form the table wrote ($what)" \
      "author $(jq -c '.author' "$work/identity.json"), the table $form"
  jq -e --arg w "$wanted" '.authored_by == [$w] and .person == 1' "$work/identity.json" >/dev/null ||
    fail I "run $id ($what) is authored by $wanted, and $person is a person node" "$(<"$work/identity.json")"
  [[ "$wanted" != session:* ]] || jq -e '.alias == 1' "$work/identity.json" >/dev/null ||
    fail I "the session $wanted is tied to $person by alias_of" "$(<"$work/identity.json")"
  pass I "$what: run $id's made_by and author are $form, and its authored_by names $wanted, $person"
done

# --- G: no reconcile or rebuild of these kinds, beside a planted one -----------------------------

readonly name_pattern='(reconcile|rebuild)[-_.a-z0-9]*(interaction|lens|risk|constraint)|(interaction|lens|risk|constraint)[-_.a-z0-9]*(reconcile|rebuild)'
# named_in TREE: every name TREE's packages/envoy/cmd/dispatch and packages/envoy/scripts hold - the
# path of every file and directory under them, and every subcommand the table in cmd/dispatch
# declares - into $named, and each of them whose name holds the pattern into $hits.
named_in() {
  local path name
  named=()
  hits=()
  while IFS= read -r path; do
    named+=("file $path")
  done < <(cd -- "$1/packages/envoy" && find cmd/dispatch scripts -mindepth 1 -print)
  while IFS= read -r name; do
    named+=("subcommand $name")
  done < <(sed -n 's/^[[:space:]]*{"\([a-z0-9-]*\)", func.*/\1/p' "$1"/packages/envoy/cmd/dispatch/*.go)
  for name in "${named[@]}"; do
    path="${name#* }"
    path="${path##*/}"
    [[ ! "${path,,}" =~ $name_pattern ]] || hits+=("$name")
  done
}
mkdir -p "$work/planted/packages/envoy/cmd"
cp -R "$tree/packages/envoy/cmd/dispatch" "$work/planted/packages/envoy/cmd/" ||
  fail G "copy packages/envoy/cmd/dispatch and packages/envoy/scripts under $work/planted" "cp failed"
cp -R "$tree/packages/envoy/scripts" "$work/planted/packages/envoy/" ||
  fail G "copy packages/envoy/cmd/dispatch and packages/envoy/scripts under $work/planted" "cp failed"
printf '#!/usr/bin/env bash\n# Rebuilds the lens nodes from a projection: planted for the grep below.\n' \
  >"$work/planted/packages/envoy/scripts/rebuild-lens.sh"
printf 'G: the pattern, over each name'"'"'s last path element in lower case: %s\n' "$name_pattern"
named_in "$work/planted"
printf 'G: the copy %s with a planted scripts/rebuild-lens.sh: %d name(s) read, %d hit(s): %s\n' \
  "$work/planted" "${#named[@]}" "${#hits[@]}" "${hits[*]:-none}"
[[ "${#hits[@]}" == 1 && "${hits[0]}" == "file scripts/rebuild-lens.sh" ]] ||
  fail G "the grep finds the planted rebuild-lens.sh in the copy, and nothing else" "it found ${#hits[@]}: ${hits[*]:-none}"
pass G "the grep finds the planted scripts/rebuild-lens.sh in the copy under $work/planted"
named_in "$tree"
printf 'G: this tree, %s: %d name(s) read, %d hit(s): %s\n' "$tree" "${#named[@]}" "${#hits[@]}" "${hits[*]:-none}"
[[ " ${named[*]} " == *" subcommand lens-kind-add "* && " ${named[*]} " == *" subcommand backfill-graph-store "* ]] ||
  fail G "the grep reads the subcommand table of cmd/dispatch" "it read no lens-kind-add and no backfill-graph-store"
[[ "${#hits[@]}" == 0 ]] ||
  fail G "no subcommand and no file of packages/envoy/cmd/dispatch or packages/envoy/scripts holds reconcile or rebuild beside interaction, lens, risk or constraint" \
    "${hits[*]}"
pass G "no subcommand and no file of packages/envoy/cmd/dispatch or packages/envoy/scripts holds reconcile or rebuild beside interaction, lens, risk or constraint (0 of ${#named[@]} names)"

# --- P: the predicate's sets, against the store's own -------------------------------------------

sign_in P "$alice" " at $base"
sign_in P "$bob" " at $base"
# Every node of this child's kinds the store holds present, its address, and whether the store's
# columns alone admit it to :'person': a node at project, or one reserved for its author whose
# made_by names that person - the person a credential proves for a lens, a finding, a risk and a
# constraint, and the whole credential, a cookie's user, for a run. A lens kind is project's.
readonly candidates_sql="select coalesce(json_agg(json_build_object(
    'node', n.kind || ':' || n.id,
    'address', case when n.kind = 'block' then 'dispatch://' || split_part(n.id, '#', 1) || '#b-' || split_part(n.id, '#', 2)
                    else 'dispatch://' || n.kind || '/' || n.id end,
    'admits', n.visibility = 'project' or (n.visibility = 'author' and case
        when n.kind = 'lens_kind' then false
        when n.kind = 'interaction' then n.made_by->>'kind' = 'user' and n.made_by->>'id' = :'person'
             and n.made_by->'owner' is null and n.made_by->'service' is null
        else user_threads_login(coalesce(case when n.made_by->>'kind' = 'user' then n.made_by->>'id' end,
                                         n.made_by->>'owner', '')) = :'person'
      end)) order by n.kind, n.id), '[]')
  from graph_nodes n
  where n.state <> 'gone'
    and (n.kind in ('lens', 'risk', 'constraint', 'lens_kind', 'interaction') or (n.kind = 'block' and n.id like 'lens/%'))"
for person in "$alice" "$bob"; do
  with_vars P "read the store's nodes of this child's kinds" "$candidates_sql" "person=$person"
  printf '%s\n' "$answer" >"$work/candidates.$person.json"
  : >"$work/predicate.$person"
  while IFS=$'\t' read -r node address; do
    request "cookie:$person" GET "/api/v1/references?to=$(q "$address")"
    case "$status" in
      200) printf '%s\n' "$node" >>"$work/predicate.$person" ;;
      404) ;;
      *) fail P "$person reads $address" "HTTP $status: $(body)" ;;
    esac
  done < <(jq -r '.[] | [.node, .address] | @tsv' "$work/candidates.$person.json")
  jq -r '.[] | select(.admits) | .node' "$work/candidates.$person.json" >"$work/store.$person"
  [[ "$(<"$work/predicate.$person")" == "$(<"$work/store.$person")" ]] ||
    fail P "the nodes the predicate admits to $person are the nodes the store alone admits" \
      "the predicate only: $(comm -23 <(sort "$work/predicate.$person") <(sort "$work/store.$person") | tr '\n' ' '); the store only: $(comm -13 <(sort "$work/predicate.$person") <(sort "$work/store.$person") | tr '\n' ' ')"
  pass P "the predicate admits $(wc -l <"$work/predicate.$person") of $(jq 'length' "$work/candidates.$person.json") nodes to $person, the set the store's columns alone admit"
done
own_alice="$(comm -23 <(sort "$work/predicate.$alice") <(sort "$work/predicate.$bob") | tr '\n' ' ')"
own_bob="$(comm -13 <(sort "$work/predicate.$alice") <(sort "$work/predicate.$bob") | tr '\n' ' ')"
[[ " $own_alice " == *" lens:$premortem_id "* && " $own_alice " == *" risk:$risk_id "* && " $own_alice " == *" interaction:$run_alice "* &&
  " $own_bob " == *" constraint:$constraint_id "* && " $own_bob " == *" interaction:$run_bob "* ]] ||
  fail P "each person's set holds what that person alone wrote reserved for its author" "$alice alone: $own_alice; $bob alone: $own_bob"
pass P "$alice alone reads $own_alice; $bob alone reads $own_bob"

# --- M: the .census.sql of each migration this head applied ---------------------------------------

censused=0
for version in $head_migrations; do
  files=("$tree/packages/envoy/internal/dispatch/store/migrations/${version}_"*.census.sql)
  [[ -f "${files[0]}" ]] || fail M "migration $version has a .census.sql" "none in $tree/packages/envoy/internal/dispatch/store/migrations"
  psql_run M "run ${files[0]##*/}" -f "${files[0]}"
  [[ "$(<"$work/psql.out")" == 0 ]] || fail M "${files[0]##*/} reads 0" "it read: $(<"$work/psql.out")"
  pass M "${files[0]##*/}, the select run before a deploy, reads 0"
  censused=$((censused + 1))
done
[[ "$censused" -gt 0 ]] || fail M "run this head's migrations' .census.sql" "this head applied no migration the base build had not"

pass "$driver" complete
