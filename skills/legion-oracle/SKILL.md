---
name: legion-oracle
description: Use when you are about to ask a human a question about a pattern, convention, solved problem or technical approach that the repository or its docs/solutions/ might already answer.
---

# Legion Oracle

Which questions to research, which to put to a person, and what your answer says about what you
searched: `skill://dispatch-first`, "Research before you ask". These are the research steps in a
Legion repository.

## Research steps

If your deployment instructions (the `# Deployment instructions` part of your system prompt, from
`legion.yaml`'s `instructions:`) name a role that answers research questions (a role, not the
`oracle` task agent), send it the question first with
`envoy_publish(topic="notifications.role.<name>", expects_reply="required")` and end
your turn: the reply starts your next one. When the publish says Envoy only kept the question
because no session holds the role, put the publish's answer in your output and research at once.

Run steps 1 and 2, in parallel if you like. Run step 3 when the question names a library, and
step 4 when it asks for general practice, each only where steps 1 and 2 give no `file:line` source
for some part of the question. Use only `read`, `grep`, `web_search`, `envoy_publish`, and `task`
with `scout` or `oracle`: the daemon's boot gate checks that the agents Legion's prompts name
exist. Each `task` runs as a job of its own: `wait` for each result and answer from what came back.

| Step | Tool | Query |
|------|------|-------|
| 1. Institutional learnings | `grep`, then `read`, over `docs/solutions/` (front-matter `tags` first, then the body) | each name the question gives (module, file, function, error text), then two or three words for the problem |
| 2. Codebase patterns | `task(agent="scout")` for a read-only search; `task(agent="oracle")` when scout's answer spans two or more modules or shows patterns that disagree | how [module] handles [topic] |
| 3. Framework docs | `web_search`, then `read` the library's official docs page | [library] docs [topic] |
| 4. External practices | `web_search`, then `read` what the tool's or standard's maintainer publishes; give its URL | current practice for [topic] |

Before you rely on a step 1 hit, `grep` `docs/solutions/` for its path. If another file lists that
path under its front-matter `supersedes:` key (on that line or as a list item below it), use that
file instead; if one has a line `Extends docs/solutions/<hit path>`, read both. Nothing else
reconciles these links.

## Output

Report as `skill://dispatch-first` says, naming `docs/solutions/` and the codebase queries you ran.
