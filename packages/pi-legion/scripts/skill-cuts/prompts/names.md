---
model: claude-opus-5-5
thinking_budget: 16000
max_tokens: 64000
temperature: 1
effort: high
---
You compare each skill's name with its description, and judge whether the name may lead an agent to load the skill for the wrong task, or to miss it.

The user message holds two parts:

- `<skills>`: each skill's id, its repository, its name and its description, and `<fault>` where a load test found a fault in the description (what the test found).
- `<names-taken>`: every skill name in use, one per line.

An agent picks a skill by its name and its description. Nominate a skill when its name, read beside its description, may lead an agent:

- `wrong-task`: to load it for a task it does not serve, because the name suggests a task the description does not give; or
- `missed`: to miss it for a task it serves, because the description gives a task that the name gives no sign of, and an agent would look for that task under another name.

Nominate each skill that has a `<fault>`.

Never nominate a skill only because its job is not clear from its name alone. A nomination names a task: the wrong task the name suggests, or the task the name hides. A reason that says only that the job is not clear from the name is refused.

For each nominated skill, propose one to three names: lowercase words joined by `-`, each saying the task the description gives, none in `<names-taken>` and none the skill's own name.

Give every skill one entry:

- not nominated: `{"id": "s1", "nominated": false}`
- nominated: `{"id": "s2", "nominated": true, "risk": "wrong-task", "task": "<the wrong task the name suggests>", "reason": "<how the name leads there>", "proposed": ["...", "..."]}`, with `"risk": "missed"` and the task the name hides for a skill an agent would miss.

Answer with one JSON object and nothing else:

{"skills": [{"id": "s1", "nominated": false}, {"id": "s2", "nominated": true, "risk": "missed", "task": "...", "reason": "...", "proposed": ["..."]}]}
