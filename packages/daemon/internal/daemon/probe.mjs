export default async function probeLegionPluginLoaded(pi) {
  // The three symbols are the wire between the plugins and this probe, read by their exact
  // `Symbol.for` strings (packages/pi-shared/src/interface.ts; packages/pi-shared/AGENTS.md,
  // "Rules", says a rename is a daemon change too): this probe imports nothing of pi-shared, since
  // it must answer about whatever build is installed. The Legion entry's factory sets its marker,
  // an object naming where it loaded from and the Envoy interface version it speaks; the Envoy
  // entry publishes into the interface object, created by whoever touches it first, so an object
  // with no publisher is no Envoy plugin; and the pre-split @sjawhar/pi-legion-envoy sets its own
  // marker, which a pane that still loads that package carries beside pi-legion's.
  const loaded = globalThis[Symbol.for("legion.pi-legion.loaded")];
  const envoy = globalThis[Symbol.for("legion.pi-shared.envoy-plugin-interface")];
  const legacy = globalThis[Symbol.for("legion.pi-envoy.legion-loaded")];
  let answer = loaded
    ? `LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=${loaded.from}\nLEGION_PLUGIN_ENVOY_INTERFACE=${loaded.envoyInterface}\n`
    : "LEGION_PLUGIN_LOADED=no\n";
  // Every Envoy entry that published, in load order: the interface's `publishers` holds each
  // entry's import.meta.url once (publishEnvoyPluginInterface, packages/pi-shared/src/interface.ts),
  // so a plugin both linked into the profile and named by --extension is two publishers — two load
  // paths, two import URLs — while a `task` subagent re-running the factory on the same module
  // instance adds none. Each LEGION_ENVOY_PUBLISHER line is one URL, so the gate can refuse a
  // pod's double load naming every copy (bootgate.go judgeLoaded).
  const publishers = envoy?.publishers ?? [];
  const publisher = publishers[0];
  answer +=
    publisher === undefined
      ? "LEGION_ENVOY_INTERFACE=none\n"
      : `LEGION_ENVOY_INTERFACE=${envoy.version}\nLEGION_ENVOY_LOADED_FROM=${publisher}\n` +
        publishers.map((from) => `LEGION_ENVOY_PUBLISHER=${from}\n`).join("");
  if (legacy !== undefined) answer += `LEGION_LEGACY_PLUGIN_LOADED_FROM=${legacy}\n`;
  process.stderr.write(answer);
  // The task agents and skills Legion's prompts name, each resolved as a worker's use resolves it:
  // Oh My Pi's own agent and skill discovery over this launch's extension roots. A pod names its
  // two plugin roots, the Envoy plugin's then the Legion plugin's, as explicit extensions beside
  // its discovery (LEGION_PROMPT_ROOTS, colon-separated: the only place the plugins' agents and
  // skills live, since the image's profile links neither); a pane discovers its installed plugins.
  // Skills are filtered as a session filters them, by the launch's own `skills` settings and
  // `disabledExtensions` (session-tools.ts #applyDiscoveredSkills at the pin).
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
  // Each discovery runs once at most, for the first of the answers below that asks for it.
  const discoveredAgents = once(async () => {
    const { discoverAgents } = await import("@oh-my-pi/pi-coding-agent/task/discovery");
    return (await discoverAgents(cwd, undefined, roots)).agents;
  });
  const discoveredSkills = once(async () => {
    const { loadSkills } = await import("@oh-my-pi/pi-coding-agent/extensibility/skills");
    const { cfgDisabledExtensions, cfgSkills } = await import(
      "@oh-my-pi/pi-coding-agent/extensibility/settings"
    );
    const { settings } = await import("@oh-my-pi/pi-coding-agent/config/settings");
    const options = {
      ...cfgSkills.get(settings),
      cwd,
      disabledExtensions: cfgDisabledExtensions.get(settings),
      extensionRoots: roots,
    };
    return (await loadSkills(options)).skills;
  });
  const agents = await resolve("LEGION_PROMPT_AGENTS", discoveredAgents);
  // Each of those agents' models, resolved once the session's model registry exists: Oh My Pi hands
  // it to extensions only in an event, and `omp models` emits session_shutdown.
  if (agents && !process.env.LEGION_SKIP_AGENT_MODELS) {
    pi.on("session_shutdown", (_event, ctx) => agentModels(agents, ctx));
  }
  await resolve("LEGION_PROMPT_SKILLS", discoveredSkills);
  // Every agent and every skill the same discovery finds, whatever names were asked for: what the
  // launch ships, which the daemon holds a folder of texts' agents and skills to (bootgate.go,
  // readLoadAnswer).
  if (process.env.LEGION_PROMPT_LIST) await list(discoveredAgents, discoveredSkills);
}

// once is discover run once at most: every call answers the promise of its first run.
function once(discover) {
  let discovered;
  return () => {
    discovered ??= discover();
    return discovered;
  };
}

// list prints every agent and every skill discovery finds, one LEGION_SHIPPED_AGENT or
// LEGION_SHIPPED_SKILL line each, all of them or none: a discovery that fails prints
// LEGION_SHIPPED_UNRESOLVABLE with its first line and no name, so the gate never reads part of the
// list for the whole of it.
async function list(discoveredAgents, discoveredSkills) {
  try {
    const agents = await discoveredAgents();
    const skills = await discoveredSkills();
    process.stderr.write(
      agents.map((agent) => `LEGION_SHIPPED_AGENT=${agent.name}\n`).join("") +
        skills.map((skill) => `LEGION_SHIPPED_SKILL=${skill.name}\n`).join("")
    );
  } catch (error) {
    process.stderr.write(`LEGION_SHIPPED_UNRESOLVABLE=${firstLine(error)}\n`);
  }
}

// resolve answers, under variable, which of the names it lists Oh My Pi's discovery finds, and
// returns the entries found among them.
async function resolve(variable, discover) {
  const wanted = (process.env[variable] ?? "").split(",").filter(Boolean);
  if (wanted.length === 0) return undefined;
  try {
    const discovered = await discover();
    const found = new Set(discovered.map((entry) => entry.name));
    const missing = wanted.filter((name) => !found.has(name));
    process.stderr.write(
      missing.length === 0 ? `${variable}=resolved\n` : `${variable}_MISSING=${missing.join(",")}\n`
    );
    return discovered.filter((entry) => wanted.includes(entry.name));
  } catch (error) {
    process.stderr.write(`${variable}_UNRESOLVABLE=${firstLine(error)}\n`);
    return undefined;
  }
}

// agentModels answers whether each agent runs on its own model, selected and resolved as the task
// tool does a subagent's at the pin (resolveEffectiveAgentModelSelection in config/model-resolver.ts,
// resolveModelOverrideWithAuthFallback from task/executor.ts): the operator's
// `task.agentModelOverrides` entry when it expands to a model, else the agent's frontmatter model,
// expanded through the configured roles, then resolved with the operator's default role as the
// parent's model, then the model's key. Where the task tool would quietly run the agent on the
// parent's model instead (a role with no configured model, or a model whose key does not work) the
// agent is unresolved. An agent that names no model runs on the session's own by design, and is
// not judged.
async function agentModels(agents, ctx) {
  try {
    const resolver = await import("@oh-my-pi/pi-coding-agent/config/model-resolver");
    const { isAuthenticated, kNoAuth } = await import(
      "@oh-my-pi/pi-coding-agent/config/model-registry"
    );
    const { settings } = await import("@oh-my-pi/pi-coding-agent/config/settings");
    const { cfgTaskAgentModelOverrides } = await import("@oh-my-pi/pi-coding-agent/task/settings");
    const overrides = cfgTaskAgentModelOverrides.get(settings);
    const parent = settings.getModelRole("default");

    // problem is why the task tool would not run an agent on the model it declares, or undefined.
    const problem = async (declared) => {
      const patterns = resolver.resolveConfiguredModelPatterns(declared, settings);
      const result =
        patterns.length === 0
          ? {}
          : await resolver.resolveModelOverrideWithAuthFallback(
              patterns,
              parent,
              ctx.modelRegistry,
              settings
            );
      if (!result.model) {
        // A role alias no configured role expands stays an alias: the role is not configured.
        const left = patterns.length > 0 ? patterns : declared;
        const roles = left.filter((pattern) => pattern.startsWith("@"));
        if (roles.length === left.length) {
          return `role ${roles.map((role) => role.slice(1).split(":")[0]).join(",")} is not configured`;
        }
        return `no available model matches ${left.join(",")}`;
      }
      const model = `${result.model.provider}/${result.model.id}`;
      if (result.authFallbackUsed) {
        return `${patterns.join(",")} has no working credentials, so Oh My Pi runs it on the parent's model ${model}`;
      }
      const key = await ctx.modelRegistry.getApiKey(result.model);
      if (key !== kNoAuth && !isAuthenticated(key))
        return `its model ${model} has no working credentials`;
      return undefined;
    };

    const unresolved = [];
    for (const agent of agents) {
      const override = overrides[agent.name];
      const declared = resolver.normalizeModelPatternList(
        resolver.resolveConfiguredModelPatterns(override, settings).length > 0
          ? override
          : agent.model
      );
      if (declared.length === 0) continue;
      const why = await problem(declared);
      if (why) unresolved.push({ agent: agent.name, model: declared.join(","), why });
    }
    process.stderr.write(
      unresolved.length === 0
        ? "LEGION_AGENT_MODELS=resolved\n"
        : unresolved
            .map((entry) => `LEGION_AGENT_MODEL_UNRESOLVED=${JSON.stringify(entry)}\n`)
            .join("")
    );
  } catch (error) {
    process.stderr.write(`LEGION_AGENT_MODELS_UNRESOLVABLE=${firstLine(error)}\n`);
  }
}

function firstLine(error) {
  return String(error?.message ?? error).split("\n")[0];
}
