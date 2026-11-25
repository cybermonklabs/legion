// An Oh My Pi extension that prints every agent name and every skill name Oh My Pi's own discovery
// finds over the roots LEGION_PROMPT_ROOTS names, one `agent <name>` or `skill <name>` line each on
// stderr. It makes the same two calls probe.mjs makes under LEGION_PROMPT_AGENTS and
// LEGION_PROMPT_SKILLS (discoverAgents and loadSkills, with the same options and the launch's own
// `skills` settings), but lists what they find rather than asking about names, and imports
// nothing of Legion, so it runs on a plain Oh My Pi before any Legion code changes.
// capture-names.sh runs it to write shipped-names.txt. LEGION_PROMPT_ROOTS is the colon-separated
// plugin roots a pod's probe names, as probe.mjs reads it; unset, discovery reads the profile's
// installed plugins, as a pane's does.
export default async function listShippedNames() {
  const explicit = process.env.LEGION_PROMPT_ROOTS;
  const roots = explicit
    ? {
        explicit: explicit.split(":"),
        mode: "explicit-only",
        configured: [],
        configuredLevel: "user",
      }
    : undefined;
  const cwd = process.cwd();

  const { discoverAgents } = await import("@oh-my-pi/pi-coding-agent/task/discovery");
  const { agents } = await discoverAgents(cwd, undefined, roots);

  const { loadSkills } = await import("@oh-my-pi/pi-coding-agent/extensibility/skills");
  const { cfgDisabledExtensions, cfgSkills } = await import(
    "@oh-my-pi/pi-coding-agent/extensibility/settings"
  );
  const { settings } = await import("@oh-my-pi/pi-coding-agent/config/settings");
  const { skills } = await loadSkills({
    ...cfgSkills.get(settings),
    cwd,
    disabledExtensions: cfgDisabledExtensions.get(settings),
    extensionRoots: roots,
  });

  const lines = [
    ...agents.map((agent) => `agent ${agent.name}`),
    ...skills.map((skill) => `skill ${skill.name}`),
  ];
  process.stderr.write(`${lines.join("\n")}\n`);
}
