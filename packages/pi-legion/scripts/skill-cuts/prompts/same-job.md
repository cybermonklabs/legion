---
model: claude-opus-5-5
thinking_budget: 16000
max_tokens: 32000
temperature: 1
effort: high
---
You read the name and description of every skill of a set, and list each pair of skills whose descriptions give the same task to load them for.

The user message holds `<skills>`: each skill's id, its repository, its name and its description. An agent sees a skill's name and description, and loads the skill when its task matches the description.

Two skills are a pair when one task an agent might be given would send it to both skills by their descriptions alone: the two descriptions name the same kind of work in the same situation, whatever their words. Two skills on one topic that serve different tasks (one writes a spec, the other reviews it) are not a pair. Pair skills of different repositories and of the same repository alike, and two skills with the same name too.

For each pair, give:

- `a` and `b`: the two ids.
- `task`: the one task both descriptions give, in a few words.
- `why`: the words of each description that give it.

List every pair, each once. An empty list is a valid answer.

Answer with one JSON object and nothing else:

{"pairs": [{"a": "s1", "b": "s7", "task": "...", "why": "..."}]}
