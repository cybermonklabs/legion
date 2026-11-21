---
model: claude-opus-5-5
thinking_budget: 8000
max_tokens: 16000
temperature: 1
effort: high
---
You pick the tasks of a load test of one skill: does an agent load the skill when a task needs it, and leave it alone when a task only looks like one that does?

The user message holds two parts, and sometimes a third before them:

- `<control>`: present only when every message is already a task of the test (see the end).
- `<skill>`: the skill's name and its description.
- `<first-messages>`: the first user messages of real sessions, each with an id, or `None.`

Give:

- `terms`: one to six words or short phrases to search first user messages for, to find sessions whose work is the work the skill serves, or close to it.
- `tasks`: at least five `near-miss` tasks, and at least five `need` tasks whose first step the skill governs.
  - `need`: a task whose work the skill serves, so an agent doing it should load the skill.
  - `near-miss`: a task close to the skill's work, in its words or its area, that the skill does not serve, so an agent doing it should not load the skill.

Take a task from `<first-messages>` where a message states the work itself, as a person asks for it: give its id with `"from": "session"`, and never change its text. Use each message once. Where the messages give too few tasks, write the task with `"from": "written"`, as a person would ask for that work in one or two sentences: from the work you found in the messages or, with none, from the description. A written task does not name the skill.

Label each task from the description alone, before any run. `why` says what in the description makes it a need or a near miss.

For each `need` task, say whether its first step is one the skill governs, in `firstStep`: `"governed": true` when an agent given the task would read the skill at once, before any other act (reading a file, running a command, asking a question); `"governed": false` when the skill serves the work but only a later step needs it. `reason` says which step comes first and why. A need task whose first step the skill does not govern is left out of the test, so give at least five with `"governed": true`; you may list others with `"governed": false`.

Answer with one JSON object and nothing else:

{"terms": ["..."], "tasks": [{"label": "need", "from": "session", "messageId": "m4", "why": "...", "firstStep": {"governed": true, "reason": "..."}}, {"label": "near-miss", "from": "written", "text": "...", "why": "..."}]}

With `<control>`: each message is the first request of a session whose agent read the skill in its first reply, and each is a `need` task already. Take every message, once and in its order, as a `need` task with `"from": "session"` and its id; write no task, give no `near-miss` task, and give `terms` as an empty list. For each, `why` says what the request asks for, and `firstStep` says, as above, whether its first step is one the skill governs.
