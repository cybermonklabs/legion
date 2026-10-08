---
title: "Carry review-requested literal text by fence, variable, and byte-compare"
category: legion
tags:
  - review-threads
  - untrusted-input
  - legion-reviewer
  - legion-implementer
  - legion-tester
date: 2026-10-08
status: active
module: legion-phase-workflow
related_issues:
  - "LEGSMOKE-504"
  - "sjawhar/legion-smoke#522"
---

# Carry review-requested literal text by fence, variable, and byte-compare

When a review asks for literal text in a file, move the text by machine at every hop. The reviewer
puts it in a fenced block inside one inline comment on the file and states the expected end state
(line count, byte count, which lines stay unchanged). The implementer applies it from a shell
variable (`line='…'; printf '%s\n' "$line" >> <file>`), never by splicing the text into a command
literal, which a quote character would break. The tester reads the comment back from the
`pulls/comments/<id>` API, extracts the fence, and `cmp`s it against the resulting line.

Key a conditional acceptance clause ("one appended line when a review asks") on an observable event
shape — review event type, file, count of inline comments — and treat anything else (a COMMENT
review, a plain PR comment, a second file, a second round) as a report to the architect, not a
change. When the clause does not fire, write one sentence saying so; never invent a proof entry.

Answer the thread with `Fixed in <sha>: <one line>`, run `legion threads resolve`, and paste its
`left open … not an acceptance` line into the body: that line is correct until the reviewer's
`Accepted:` lands, and the merger's run resolves the thread.

## Evidence

- Review thread `discussion_r4213937479` on `smoke/LEGSMOKE-504.md:1` fenced
  `Legion proof review round 1 (LEGSMOKE)` and stated "2 lines, 95 bytes — line 1 unchanged". The
  round-1 REQUEST_CHANGES body asked for the variable form after its thermonuclear pass flagged the
  plan's `printf '<quoted line>\n' >>` recipe (plan.json T6) as a shell-literal splice.
- Commit `4e1183a9` was made with `line='…'; printf '%s\n' "$line" >>`; its diff is one hunk
  `@@ -1,1 +1,2 @@` adding the line. The tester's round-2 proof ran
  `… pulls/comments/4213937479 --jq .body | awk '/^```$/{f=!f; next} f' | cmp - <(sed -n 2p ac2)`
  → `LINE2_EQUALS_REVIEW_FENCED_QUOTE`.
- A decoy COMMENT review (5450614056, "decides nothing") preceded the REQUEST_CHANGES at the same
  head and correctly did not trigger the append; the tester's round 1 recorded the clause as not
  applicable with no proof entry.
- Thread disposition: implementer reply 4213951397 (`Fixed in 4e1183a9…`) → `legion threads resolve`
  `left open … newest reply by legion-implementer is not an acceptance` → reviewer `Accepted:`
  4213993488 → resolution left to the merger before READY.
