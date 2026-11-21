# packages/envoy/scripts/lib/legacy-subjects-gate-forms.jq
#
# The names legacy-subjects-gate.sh judges, as a jq module its judgement (legacy-subjects-gate-
# judge.jq) includes and its test calls row by row (legacy-subjects-gate.test.sh).

# hex2 is a byte, 0 to 255, as two lowercase hex digits.
def hex2: [(. / 16 | floor), (. % 16)] | map(. as $n | "0123456789abcdef"[$n:$n + 1]) | add;

# printable is a text read from outside the gate with each control character written as \x<hh>,
# so no name a cluster or a bus answers can start or rewrite a line of the gate's own.
def printable: tostring | gsub("(?<c>[[:cntrl:]])"; "\\x" + (.c | explode[0] | hex2));

# issue_filter_matches($lead) says whether an issue filter's tokens after the family's prefix match
# some issue event's name whose tokens are of the classes $lead names ("key", "project") and then an
# event type of two tokens or more: every Dispatch event type is a noun and a verb or longer
# (comment.created, settings.repo_project.updated). A wildcard matches any token, `>` as the last
# token one token or more, and a literal what its class takes: a key holds `-`, which no project
# key and no event type's token does; a project is a project key; an event type's token holds no
# `-`.
def issue_filter_matches($lead):
  (if (.[-1] // "") == ">" then {fixed: .[:-1], open: true} else {fixed: ., open: false} end) as $f
  | ($f.open or ($f.fixed | length) >= ($lead | length) + 2)
    and all(range(0; $f.fixed | length); . as $i
      | $f.fixed[$i] as $t
      | ($lead[$i] // "type") as $class
      | $t == "*"
        or ($class == "key" and ($t | contains("-")))
        or ($class == "project" and ($t | test("^[A-Z][A-Z0-9]{1,9}$")))
        or ($class == "type" and ($t | contains("-") | not)));

# old_form says whether a subject or a topic is in the old form of a name the rename moved, or is a
# filter that hears old-form names alone: an issue event whose fourth token is an issue key (it
# holds `-`, which no project key does, ProjectKeyPattern `^[A-Z][A-Z0-9]{1,9}$`), or an issue
# filter that matches some old-form issue event's name (the key, then the type) and no new-form
# one (the key's project, the key, then the type), such as notifications.dispatch.issue.*.comment.created,
# whose fixed length is the old form's alone; an agent's or a role's name with no project (three
# tokens); a kept lane with no project (five tokens); and a failed-delivery report of any of them.
# A wildcard in the position the old form names the session, the role or the lane's role hears
# old-form names alone, since the new form has one token more, so it is old too; `>` there hears
# both forms and is not, as an issue filter that matches both forms
# (notifications.dispatch.issue.*.*.>) is not. This is the gate's own rule, written out so the gate
# stays right after the old form's readers are deleted. The listener's counter holds a name to
# contracts.CanonicalTopic and a wildcard issue filter to contracts.LegacyIssueFilter, the same
# rule as this one's; the two agree on every literal name, and the counter is narrower on two
# shapes: it reads `notifications.dispatch.issue.A-1.>` as no issue key, since `A` is no project
# key, and a single-token wildcard in the agent or role lane as its own new form.
def old_form:
  if startswith("notifications.envoy.exceptions.") then ltrimstr("notifications.envoy.exceptions.") | old_form
  elif startswith("notifications.dispatch.issue.") then
    (ltrimstr("notifications.dispatch.issue.") | split(".")) as $t
    | ($t[0] // "" | contains("-"))
      or (($t | issue_filter_matches(["key"])) and ($t | issue_filter_matches(["project", "key"]) | not))
  elif startswith("notifications.agent.") or startswith("notifications.role.") then split(".") | length == 3 and .[2] != ">"
  elif test("^notifications\\.envoy\\.kept\\.(role|cursor)\\.") then split(".") | length == 5 and .[4] != ">"
  else false
  end;

# project_position says whether a token may stand where the new form names a project: a project
# key, `_` (no project) or `*` (any).
def project_position: test("^([A-Z][A-Z0-9]{1,9}|_|\\*)$");

# has_twin($filters) says whether an old-form filter is matched in $filters by its new form, the
# same tokens with one project position put in: a session consumer filters both forms of each of
# its topics until the old form is refused (bus.SessionConsumers, contracts.LegacyTwin), so its old
# filter loses it nothing a new-form publish carries. A filter with no such twin hears the old form
# alone.
def has_twin($filters):
  split(".") as $a
  | any($filters[]; split(".") as $b
      | ($b | length) == ($a | length) + 1
        and any(range(0; $a | length); . as $k
          | $b[:$k] == $a[:$k] and $b[$k + 1:] == $a[$k:] and ($b[$k] | project_position)));

