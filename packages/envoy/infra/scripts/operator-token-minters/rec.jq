# rec: a record passed between operator-token-minters' steps, its fields joined by the unit
# separator, never by tabs: bash's read splits on a tab as whitespace, so an empty field (a group's
# empty user, a secret's unnamed project) would shift every field after it. A control character in
# a field is made a space, the separator among them, so a field never splits. One test of the
# fields together finds whether any field holds one, and only then is each field rewritten: a
# review's answer is thousands of records, and a rewrite of each field more than doubled its read
# (2,240 records of 7 fields: 77 ms a read with a rewrite of each field, 34 ms with the test). A
# program reads this module with `include "rec";` and jq's -L at this directory.
def rec: map(tostring) | if add // "" | test("[[:cntrl:]]") then map(gsub("[[:cntrl:]]"; " ")) else . end | join("\u001f");
# is_one_array($slurped): whether a --slurpfile variable holds exactly one value, a JSON array. An
# empty file, which a step that failed or wrote nothing leaves, holds no value, so it is not one.
def is_one_array($slurped): ($slurped | length) == 1 and ($slurped[0] | type) == "array";
# one_array($slurped; $what): the one JSON array a --slurpfile variable holds (is_one_array), and an
# error naming WHAT for anything else, so the program fails rather than read no value as none.
def one_array($slurped; $what):
  if is_one_array($slurped) then $slurped[0]
  else error("\($what) is not one JSON array: the file holds \($slurped | length) value(s)") end;
# objs: the objects of a render (a List, an array or a stream of objects) or of a cluster's list,
# as code-rbac.jq and broker-fence.jq read them.
def objs: if type == "array" then .[] | objs
          elif type == "object" and (.kind // "") == "List" then (.items // [])[] | objs
          elif type == "object" then . else empty end;
