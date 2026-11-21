---
model: claude-opus-5-5
thinking_budget: 4000
max_tokens: 16000
temperature: 1
effort: high
---
You read skills and find each one that holds the follow-the-code sentence, in any wording.

The user message holds skills (`<skill>`), each with its id and its text, each line numbered from 1 as `<number>: <line>`.

The follow-the-code sentence tells the agent what to do when the skill disagrees with the code: follow the code (or a command's output, a tool's `--help`, or what the agent sees) and not the skill. It may also tell the agent to say which line of the skill is wrong. One wording is "If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong." The words need not hold "disagree" or "code": "Where this text and the program differ, trust the program" is the sentence too. A sentence about two documents that disagree with each other, about a person who disagrees, or about tests that disagree with the code is not it.

For each time a skill holds it, give:

- `id`: the skill's id.
- `line`: the line where the sentence starts.
- `quote`: the sentence, copied.

An empty list is a valid answer.

Answer with one JSON object and nothing else:

{"found": [{"id": "s3", "line": 8, "quote": "..."}]}
