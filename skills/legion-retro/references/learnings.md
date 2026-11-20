# Writing a retro learning

Read this at step 3 of `skill://legion-retro`. The learnings are one `dispatch comment` on the
issue. Its reader is the skill librarian, who turns a point into a rule in a skill, a role prompt
or a repository guide.

## What a point must carry

- **The rule**, in a few imperative lines, in the present tense, about the work and never about
  this pull request.
- **The evidence** below the rule: the file and symbol, or text quoted from the file, read at the
  head you commit, and what went wrong before the rule. Never a line number: the next edit above
  it moves it. Where the head no longer shows what your sentence says, quote the text and name the
  commit you read it at. The head you commit is the approved head you committed as the
  implementer: retro itself commits nothing. A reader takes a story as an instruction, so the
  story never stands in the rule's place.
- **Where it belongs**: the file a reader would look in, for example
  `skills/legion-worker/references/tools.md`; the librarian decides.
- **What it replaces**, when an instruction the repository carries is wrong: quote it and its path.

Drop a point that cites no file, symbol, quoted text or commit of this pull request, and a point
that only restates what a skill already says.

## Find what already covers the topic

1. `dispatch search --query '<two or three words naming the topic>'`.
2. The `grep` tool, case-insensitive, over `docs/solutions/`, and read each hit's H1 and rule: an
   older rule may say it already, or say the opposite, which is itself the point to report.
3. The skills and role prompts the point would change (`skills/`,
   `packages/daemon/internal/prompts/`). A rule one of them states is no learning.

## The comment

One comment holds every kept point, each under its own `###` heading:

```bash
dispatch comment --issue '<KEY>' --body-file - <<'EOF'
## Retro learnings

### <the rule, in one line>

<the rule, a few imperative lines>

Evidence: <file and symbol, or text quoted from it, of this pull request, and what went wrong>.
Belongs in: <the skill, prompt or guide>.
Replaces: <the instruction and its path>, or nothing.
EOF
```
