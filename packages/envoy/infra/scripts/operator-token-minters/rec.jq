# rec: a record passed between operator-token-minters' steps, its fields joined by the unit
# separator, never by tabs: bash's read splits on a tab as whitespace, so an empty field (a group's
# empty user, a secret's unnamed project) would shift every field after it. A program reads this
# module with `include "rec";` and jq's -L at this directory.
def rec: map(tostring | gsub("[[:cntrl:]]"; " ")) | join("\u001f");
# one_array($slurped; $what): the one JSON array a --slurpfile variable holds, and an error naming
# WHAT for anything else. An empty file, which a step that failed or wrote nothing leaves, holds no
# value, so the program fails rather than read it as none.
def one_array($slurped; $what):
  if ($slurped | length) == 1 and ($slurped[0] | type) == "array" then $slurped[0]
  else error("\($what) is not one JSON array: the file holds \($slurped | length) value(s)") end;
# objs: the objects of a render (a List, an array or a stream of objects) or of a cluster's list,
# as code-rbac.jq and broker-fence.jq read them.
def objs: if type == "array" then .[] | objs
          elif type == "object" and (.kind // "") == "List" then (.items // [])[] | objs
          elif type == "object" then . else empty end;
