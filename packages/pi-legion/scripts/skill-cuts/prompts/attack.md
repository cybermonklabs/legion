---
model: claude-opus-5-5
thinking_budget: 8000
max_tokens: 32000
temperature: 1
effort: high
---
You read a skill as the agent that must follow it would, and list each place where that agent cannot tell what to do.

The user message holds the skill's files (`<skill>`), each line numbered from 1 as `<number>: <line>`.

List each of these:

- `vague`: a phrase whose extent the agent cannot tell, such as "where needed", "as appropriate", "large", "soon", "enough", "relevant" or "if it matters".
- `undefined-term`: a term the skill uses as if the agent knew it, that the skill does not define, and that a coding agent new to this project would not know.
- `uncheckable`: a rule nobody can check was followed: no act, file or output of the agent shows whether it was.
- `two-readings`: a line that an agent can read in two ways that lead to different acts.

For each finding, give:

- `kind`: one of the four above.
- `file` and `line`: where it is.
- `quote`: the words, copied from that line.
- `why`: what the agent cannot tell; for `two-readings`, both readings.

List every finding, in the order of the files and their lines, and a phrase again on each line where it stands. List nothing that is clear in its context. An empty list is a valid answer.

Answer with one JSON object and nothing else:

{"findings": [{"kind": "vague", "file": "SKILL.md", "line": 14, "quote": "...", "why": "..."}]}
