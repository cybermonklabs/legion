# rec: a record passed between operator-token-minters' steps, its fields joined by the unit
# separator, never by tabs: bash's read splits on a tab as whitespace, so an empty field (a group's
# empty user, a secret's unnamed project) would shift every field after it. A program reads this
# module with `include "rec";` and jq's -L at this directory.
def rec: map(tostring | gsub("[[:cntrl:]]"; " ")) | join("\u001f");
