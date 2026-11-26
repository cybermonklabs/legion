# Skills

## What a skill holds

- `SKILL.md` opens with frontmatter holding `name` and `description`, each on one line.

- Write a skill as [`writing-agent-skills`](https://github.com/sjawhar/dotfiles/blob/main/plugins/sjawhar/skills/writing-agent-skills/SKILL.md) says: its `description`, and how long a `SKILL.md` and each reference may be.

- The `SKILL.md` body holds the routing, and the rules the model would not follow by default.

- A procedure is numbered steps, with no narrative.

- Detail goes in `references/`, one level deep.
  `SKILL.md` names each reference where it is needed, with the condition for reading it ("Read `skill://<name>/references/x.md` when ...").
  Merge a small reference into `SKILL.md`, and delete one that nothing names.

- No history: no dates, incidents, issue keys or pull request numbers.
  No emphasis words used as force: a rule that needs force is a check.

- A fact lives in one skill, and the others link to it.

- Keep each skill file path that code, a prompt or another skill names, or change the namer in the same change.
  The skills guards resolve `skill://` names only in skills and daemon prompts, not in Go or TypeScript source.

## Links and names

- Name a file of a skill as `skill://<name>/<path>`, never by a relative path, inside one skill too: a `skill://` read returns the whole file, and each plugin installs its skills apart, so a relative link out of one skill dangles.

- A skill, a daemon prompt or a Legion agent definition names a task agent only as `task(agent="<name>")`, whose result it reads with `wait`, and a skill only as `skill://<name>`.
  The daemon's boot gate and the skills guards read only those forms, so a name written any other way reaches a worker unchecked.

Where this file and the code disagree, the code is right: follow the code and correct this file.
