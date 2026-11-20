# packages/envoy/scripts/lib/legacy-subjects-gate-forms.jq
#
# The names legacy-subjects-gate.sh judges, as a jq module its judgement (legacy-subjects-gate-
# judge.jq) includes and its test calls row by row (legacy-subjects-gate.test.sh).

# hex2 is a byte, 0 to 255, as two lowercase hex digits.
def hex2: [(. / 16 | floor), (. % 16)] | map(. as $n | "0123456789abcdef"[$n:$n + 1]) | add;

# printable is a text read from outside the gate with each control character written as \x<hh>,
# so no name a cluster or a bus answers can start or rewrite a line of the gate's own.
def printable: tostring | gsub("(?<c>[[:cntrl:]])"; "\\x" + (.c | explode[0] | hex2));

# old_form says whether a subject or a topic is in the old form of a name the rename moved: an issue
# event whose fourth token is an issue key (it holds `-`, which no project key does, ProjectKeyPattern
# `^[A-Z][A-Z0-9]{1,9}$`), an agent's or a role's name with no project (three tokens), a kept lane
# with no project (five tokens), and a failed-delivery report of any of them. A wildcard in the
# position the old form names the session, the role or the lane's role hears old-form names alone,
# since the new form has one token more, so it is old too; `>` there hears both forms and is not.
# This is the gate's own rule, written out so the gate stays right after the old form's readers are
# deleted (contracts.CanonicalTopic agrees on every literal name, and is narrower on two shapes:
# it reads `notifications.dispatch.issue.A-1.>` as no issue key, since `A` is no project key, and a
# single-token wildcard as its own new form).
def old_form:
  if startswith("notifications.envoy.exceptions.") then ltrimstr("notifications.envoy.exceptions.") | old_form
  elif startswith("notifications.dispatch.issue.") then (split(".")[3] // "") | contains("-")
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

