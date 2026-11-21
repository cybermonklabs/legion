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

# family_token($name) says whether a filter's token reaches a family's token: it is the name, or
# `*`. A `>` hears both forms of every family below it, and reaches no family here.
def family_token($name): . == $name or . == "*";

# issue_family_rest is the tokens after the issue family's own three of a filter that reaches the
# family, or null: each of its first three tokens is the family's (`notifications`, `dispatch`,
# `issue`) or `*`, so notifications.dispatch.*.*.comment.created reaches it as surely as
# notifications.dispatch.issue.*.comment.created does.
def issue_family_rest:
  split(".") as $t
  | if ($t | length) >= 3 and ($t[0] | family_token("notifications")) and ($t[1] | family_token("dispatch"))
      and ($t[2] | family_token("issue"))
    then $t[3:] else null end;

# issue_old says whether a filter that reaches the issue family hears old-form issue events alone:
# its fourth token is an issue key (it holds `-`, which no project key does, ProjectKeyPattern
# `^[A-Z][A-Z0-9]{1,9}$`), or it matches some old-form issue event's name (the key, then the type)
# and no new-form one (the key's project, the key, then the type).
def issue_old:
  issue_family_rest as $t
  | $t != null
    and (($t[0] // "" | contains("-"))
      or (($t | issue_filter_matches(["key"])) and ($t | issue_filter_matches(["project", "key"]) | not)));

# agent_role_old says whether a filter reaching the agent or the role family (its first two tokens
# each `notifications` or `*`, `agent`, `role` or `*`) names a session or a role with no project:
# three tokens, the third no `>`. The new form has one token more.
def agent_role_old:
  split(".") as $t
  | ($t | length) == 3 and ($t[0] | family_token("notifications"))
    and ($t[1] == "agent" or $t[1] == "role" or $t[1] == "*") and $t[2] != ">";

# kept_old says whether a filter reaching a kept lane (its first four tokens each `notifications`,
# `envoy`, `kept` or `*`, and `role`, `cursor` or `*`) names its role with no project: five tokens,
# the fifth no `>`. The new form has one token more.
def kept_old:
  split(".") as $t
  | ($t | length) == 5 and ($t[0] | family_token("notifications")) and ($t[1] | family_token("envoy"))
    and ($t[2] | family_token("kept")) and ($t[3] == "role" or $t[3] == "cursor" or $t[3] == "*") and $t[4] != ">";

# old_form says whether a subject or a topic is in the old form of a name the rename moved, or is a
# filter that matches some old-form name of a family the rename moved and no new-form name of that
# family, so that its reader hears that family no more once the old form is refused: an old-only
# issue filter (issue_old), such as notifications.dispatch.issue.*.comment.created, whose fixed
# length is the old form's alone; an agent's or a role's name with no project (agent_role_old); a
# kept lane with no project (kept_old); and a failed-delivery report of any of them. A filter
# reaches a family through a wildcard in the family's own tokens as surely as through its name
# (notifications.dispatch.*.*.comment.created, notifications.*.reviewer, *.role.reviewer,
# notifications.envoy.*.role.x, notifications.envoy.kept.*.x), though it may hear another family's
# names besides. A wildcard in the position the old form names the session, the role or the lane's
# role matches old-form names of the family alone, since the new form has one token more, so it is
# old too; `>` there hears both forms and is not, as an issue filter that matches both forms
# (notifications.dispatch.issue.*.*.>) is not. This is the gate's own rule, written out so the gate
# stays right after the old form's readers are deleted. The listener's counter holds a name to
# contracts.CanonicalTopic and an issue filter to contracts.LegacyIssueFilter, which agree with this
# rule on every literal name and every issue filter; the counter is narrower on these shapes, which
# this rule takes as old and it does not: a single-token wildcard where the old form names a
# session or a role (notifications.agent.*, notifications.role.*, notifications.envoy.kept.role.*,
# notifications.envoy.kept.cursor.*, and a failed-delivery report of notifications.agent.*), and a
# wildcard in the agent, role or kept family's own tokens.
def old_form:
  if startswith("notifications.envoy.exceptions.") then ltrimstr("notifications.envoy.exceptions.") | old_form
  else issue_old or agent_role_old or kept_old
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
# issue key belongs to one project) and a Legion role's own project (role_project), and none for an
# agent's name, a wildcard key, role or family token, or a free-form role (lead-reviewer), whose
# names carry any project: a free-form role's kept lane is keyed by the project of the event it
# keeps (notifications.envoy.kept.role.PUB.reviewer), so `_` there covers some of its copies alone.
# The slot is the one of the family the filter is old in (issue_old, agent_role_old, kept_old).
# Null for a filter old in no family.
def twin_slot:
  # role_owner is a role token's one project: a Legion role's own, and none (null) for a free-form
  # role or a wildcard.
  def role_owner: if . == "*" or . == ">" then null else role_project | if . == "_" then null else . end end;
  split(".") as $t
  | if startswith("notifications.envoy.exceptions.") then
      (ltrimstr("notifications.envoy.exceptions.") | twin_slot) as $s
      | if $s == null then null else {at: ($s.at + 3), project: $s.project} end
    elif issue_old then
      {at: 3, project: ($t[3] // "" | if test("^[A-Z][A-Z0-9]{1,9}-[0-9]+$") then split("-")[0] else null end)}
    elif agent_role_old then {at: 2, project: (if $t[1] == "role" then $t[2] | role_owner else null end)}
    elif kept_old then {at: 4, project: ($t[4] | role_owner)}
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
