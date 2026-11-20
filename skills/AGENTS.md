# Skills

Each directory here is one skill that Oh My Pi sessions load.
`scripts/pi-plugin-prepack.sh` puts each skill in exactly one plugin, `@sjawhar/pi-envoy` or `@sjawhar/pi-legion`.
A new skill joins one list there and that plugin's version arguments in `.github/workflows/release.yaml` (`PI_ENVOY_VERSION_ARGS` or `PI_LEGION_VERSION_ARGS`).
A Legion pane installs both plugins; any other session has only the skills of `@sjawhar/pi-envoy`.

## What a skill holds

- Write a skill as [`writing-agent-skills`](https://github.com/sjawhar/dotfiles/blob/main/plugins/sjawhar/skills/writing-agent-skills/SKILL.md) says: its `description`, how long a `SKILL.md` and each reference may be, and how to add, test and remove a rule.

- `SKILL.md` opens with frontmatter: `name`, the directory's name, and `description`, each on one line.

## Links and names

- Link into a skill as `skill://<name>/<path>`, never by relative path: each plugin's skills install on their own, so a relative link out of one dangles.

- A Legion prompt (a skill, a role prompt in `packages/daemon/internal/prompts/roles/`, or an agent in `packages/pi-legion/agents/`) names a task agent only as `task(agent="<name>")` and a skill only as `skill://<name>`:
  the daemon's boot gate checks only those forms.

- An agent or a skill that a Legion prompt names ships here or in `packages/pi-legion/agents/`, unless Oh My Pi bundles it.

- A task agent runs as a background job, so a prompt that dispatches one also names `wait`, the tool that reads its result.

- `packages/pi-shared/test/skills-guard.ts` holds the checks each plugin's tests run over its staged skills.
