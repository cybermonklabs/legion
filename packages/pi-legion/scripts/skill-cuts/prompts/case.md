---
model: claude-opus-5-5
thinking_budget: 8000
max_tokens: 16000
temperature: 1
effort: high
---
You read part of a coding agent's session and propose one case for a test of a skill's text.

The user message holds two parts:

- `<skill>`: the lines of the skill's files that a case may name: only the lines of each file that the agent's own read of that file held, each as `<number>: <line>`, its number in the file. A line not shown cannot be a case's line, and a file not shown was not read.
- `<transcript>`: one window of the session from the agent's first read of the skill on, and before it some earlier messages. The first window starts at that read; when you propose no case, the next window starts after the last message of the one before. Each message has its index in the session and its role. Where the agent read a file of the skill, the file's text is left out. A tool call shows as `[tool call: <tool>] <arguments as JSON>`.

The test replays the session up to one step under several texts of the skill (this text, a shorter rewrite of it, this text less the line you name, this text again, and no skill at all), and a judge decides whether the agent's step passes a criterion. Your case names that step, the line of the skill that decides it, and the criterion. Under a text of the skill only the read results of the skill's files change; with no skill, the reads of the skill and their results are taken out. Every other message before the step is the same under each.

Find the step:

- The step is the first assistant message after the read whose act a line shown in `<skill>` decides: the line says what to do, or what not to do, in the situation the agent is in at that message.
- Pick a step where an agent that had not read the line could well act otherwise. A step that any agent would take without the skill (reading a file the user named, answering a greeting) is not decided by the skill.
- When no assistant message of the transcript is decided by a line shown in `<skill>`, propose no case.

Name the line:

- Copy the one line shown in `<skill>` that decides the step, whole and character for character, without its number and without the `: ` after the number. When the rule runs over more lines, copy the shown line that holds its main instruction.
- `file` is the file's path as its `<file path="...">` gives it.

Write the criterion:

- `pass`: what the step must do, at this message, to follow the line. `fail`: what a step that does not follow the line does here.
- The judge sees only the criterion, the line, the last messages before the step, and the step itself (its text and its tool calls, not its thinking). The criterion must let the judge decide from those alone.
- The real agent's step must pass. A reply that only says "I will continue." must fail.
- Quote no line of the skill but the one you copied: the criterion is the same under every text of the skill.

Write the violating step:

- A next assistant message the agent could plausibly send at the step instead, which breaks the line: it meets the criterion's fail statement and not its pass statement.
- In every other way it is like a step that follows the line: the same goal, the same kind of tool calls with arguments of the same kind, about the same length and tone. It is no refusal, no error and no act unrelated to the work, and it does not say that it breaks the line or mention the skill's rule.
- Give its text (an empty string when it holds only tool calls) and its tool calls, each with the tool's name and its arguments as a JSON object, as the transcript shows the agent's tool calls.

List the restatements:

- Every message before the step, other than a read result of the skill's files, whose text states what the line you copied tells the agent to do, in any wording: a quote of the line, a paraphrase, a plan or a note that says to do it (the agent's own "I will write each result on one line" restates "Write each step's result on one line"), a summary, the user's request, or a tool result of another file that says it.
- A message that only does what the line says, without stating it, is no restatement.
- For each, give its index and the words of that message that state it, copied exactly as the transcript shows them.
- An empty list when no message before the step states it.

Answer with one JSON object and nothing else. With a case:

{"step": {"messageIndex": 14, "what": "<the step's act, in a few words>"}, "line": {"file": "SKILL.md", "text": "<the line, copied>"}, "criterion": {"pass": "...", "fail": "..."}, "violatingStep": {"text": "...", "toolCalls": [{"name": "bash", "arguments": {"command": "..."}}]}, "restatements": [{"messageIndex": 9, "quote": "<the words, copied>"}]}

where each `messageIndex` is an index as `<transcript>` gives it. With no case:

{"step": null, "reason": "<why no step of the transcript is decided by a line of the skill>"}
