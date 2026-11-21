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

# issue_family_rest is the tokens after the issue family's own three of a filter that reaches the
# family, or null: each of its first three tokens is the family's (`notifications`, `dispatch`,
# `issue`) or `*`, so notifications.dispatch.*.*.comment.created reaches it as surely as
# notifications.dispatch.issue.*.comment.created does; a `>` among them hears both forms of every
# family, and is no issue filter.
def issue_family_rest:
  split(".") as $t
  | if ($t | length) >= 3
      and ($t[0] == "notifications" or $t[0] == "*")
      and ($t[1] == "dispatch" or $t[1] == "*")
      and ($t[2] == "issue" or $t[2] == "*")
    then $t[3:] else null end;

# old_form says whether a subject or a topic is in the old form of a name the rename moved, or is a
# filter that hears old-form names alone: an issue event whose fourth token is an issue key (it
# holds `-`, which no project key does, ProjectKeyPattern `^[A-Z][A-Z0-9]{1,9}$`), or an issue
# filter that matches some old-form issue event's name (the key, then the type) and no new-form
# one (the key's project, the key, then the type), such as
# notifications.dispatch.issue.*.comment.created, whose fixed length is the old form's alone, and
# such a filter reaching the family through a wildcard in the family's own tokens
# (issue_family_rest, notifications.dispatch.*.*.comment.created); an agent's or a role's name with
# no project (three tokens); a kept lane with no project (five tokens); and a failed-delivery
# report of any of them. A wildcard in the position the old form names the session, the role or
# the lane's role hears old-form names alone, since the new form has one token more, so it is old
# too; `>` there hears both forms and is not, as an issue filter that matches both forms
# (notifications.dispatch.issue.*.*.>) is not. This is the gate's own rule, written out so the gate
# stays right after the old form's readers are deleted. The listener's counter holds a name to
# contracts.CanonicalTopic and an issue filter to contracts.LegacyIssueFilter, the same rule as
# this one's; the two agree on every issue filter, and the counter is narrower on one shape: it
# reads a single-token wildcard in the agent or role lane as its own new form.
def old_form:
  if startswith("notifications.envoy.exceptions.") then ltrimstr("notifications.envoy.exceptions.") | old_form
  elif issue_family_rest != null then
    issue_family_rest as $t
    | ($t[0] // "" | contains("-"))
      or (($t | issue_filter_matches(["key"])) and ($t | issue_filter_matches(["project", "key"]) | not))
  elif startswith("notifications.agent.") or startswith("notifications.role.") then split(".") | length == 3 and .[2] != ">"
  elif test("^notifications\\.envoy\\.kept\\.(role|cursor)\\.") then split(".") | length == 5 and .[4] != ">"
  else false
  end;

# role_project is the project a role's name says it works for (contracts.RoleProject): a Legion
# role, legion-<token>-…, works for the project whose key is its token in upper case, when the token
# is lower-case letters and digits and that is a project key, and every other role for none (`_`).
def role_project:
  if startswith("legion-") then
    (ltrimstr("legion-") | split("-")) as $p
    | if ($p | length) >= 2 and ($p[1:] | join("-")) != "" and ($p[0] | test("^[a-z0-9]+$"))
        and ($p[0] | ascii_upcase | test("^[A-Z][A-Z0-9]{1,9}$"))
      then $p[0] | ascii_upcase else "_" end
  else "_" end;

# twin_slot is where an old-form filter's new-form twin puts its project token, and the one
# project every name the filter matches carries, when there is one: an issue key's own project (an
# issue key belongs to one project), a role's own project (role_project), and none for an agent's
# name or a wildcard key or role, whose names carry any project. Null for a filter of no family the
# rename moved.
def twin_slot:
  if startswith("notifications.envoy.exceptions.") then
    (ltrimstr("notifications.envoy.exceptions.") | twin_slot) as $s
    | if $s == null then null else {at: ($s.at + 3), project: $s.project} end
  elif issue_family_rest != null then
    {at: 3, project: (split(".")[3] // "" | if test("^[A-Z][A-Z0-9]{1,9}-[0-9]+$") then split("-")[0] else null end)}
  elif startswith("notifications.agent.") then {at: 2, project: null}
  elif startswith("notifications.role.") then
    {at: 2, project: (split(".")[2] // "" | if . == "*" or . == ">" then null else role_project end)}
  elif test("^notifications\\.envoy\\.kept\\.(role|cursor)\\.") then
    {at: 4, project: (split(".")[4] // "" | if . == "*" or . == ">" then null else role_project end)}
  else null
  end;

# has_twin($filters) says whether an old-form filter is matched in $filters by its new form that
# covers every name it matches: the same tokens with a project token put in at twin_slot's place,
# a wildcard, or the one project all its names carry. A session consumer filters both forms of each
# of its topics until the old form is refused (bus.SessionConsumers, contracts.LegacyTwin), so its
# old filter loses it nothing a new-form publish carries. A twin under one project of names that
# carry any (notifications.dispatch.issue.ACME.*.comment.created beside
# notifications.dispatch.issue.*.comment.created) covers that project alone, and is no twin; a
# filter with no twin hears the old form alone.
def has_twin($filters):
  twin_slot as $s
  | split(".") as $a
  | $s != null
    and any($filters[]; split(".") as $b
      | ($b | length) == ($a | length) + 1
        and $b[:$s.at] == $a[:$s.at] and $b[$s.at + 1:] == $a[$s.at:]
        and ($b[$s.at] == "*" or ($s.project != null and $b[$s.at] == $s.project)));
