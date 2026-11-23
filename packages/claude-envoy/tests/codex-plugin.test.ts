// The Codex plugin's package, `@sjawhar/codex-envoy`, as scripts/build.ts writes it:
// the rules of Codex's own plugin validator (`validate_plugin.py` of the `plugin-creator` skill
// Codex CLI 0.154.0 installs), which CI cannot run since nothing in CI installs Codex, held here on
// a build of this tree. The real validator and the real binary are run by hand and by
// tests/codex-real.test.ts.

import { beforeAll, describe, expect, test } from "bun:test"
import { lstatSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { buildCodexPlugin, CODEX_EXPLICIT_SKILLS, codexSkill, parseMode } from "../scripts/build"

const packageRoot = resolve(import.meta.dir, "..")

describe("codexSkill", () => {
  const body = "\n# Title\n\nText.\n"
  const skill = (fields: string): string => `---\nname: x\ndescription: d\n${fields}---${body}`

  test("leaves a skill that sets no invocation field as it is", () => {
    const source = skill("")
    expect(codexSkill("x", source)).toEqual({ skill: source, openaiYaml: null })
  })

  test("drops `disable-model-invocation: false` and writes no openai.yaml", () => {
    expect(codexSkill("x", skill("disable-model-invocation: false\n"))).toEqual({
      skill: skill(""),
      openaiYaml: null,
    })
  })

  test("turns `disable-model-invocation: true` into Codex's explicit-only policy", () => {
    const name = "thermonuclear-deep-review"
    const shown = CODEX_EXPLICIT_SKILLS[name]
    const result = codexSkill(name, skill("disable-model-invocation: true\n"))
    expect(result.skill).toBe(skill(""))
    expect(result.openaiYaml).toBe(
      [
        "interface:",
        `  display_name: ${JSON.stringify(shown?.displayName)}`,
        `  short_description: ${JSON.stringify(shown?.shortDescription)}`,
        "policy:",
        "  allow_implicit_invocation: false",
        "",
      ].join("\n"),
    )
  })

  test("refuses an explicit-only skill CODEX_EXPLICIT_SKILLS does not describe", () => {
    expect(() => codexSkill("new-skill", skill("disable-model-invocation: true\n"))).toThrow(
      /CODEX_EXPLICIT_SKILLS must describe it/,
    )
  })

  test("refuses a value other than true or false", () => {
    expect(() => codexSkill("x", skill("disable-model-invocation: yes\n"))).toThrow(
      /not true or false/,
    )
  })

  test("describes each explicit-only skill in Codex's 25 to 64 characters", () => {
    for (const shown of Object.values(CODEX_EXPLICIT_SKILLS)) {
      expect(shown.displayName.trim()).not.toBe("")
      expect(shown.shortDescription.length).toBeGreaterThanOrEqual(25)
      expect(shown.shortDescription.length).toBeLessThanOrEqual(64)
    }
  })
})

describe("parseMode", () => {
  test("reads each mode and refuses anything else", () => {
    expect(parseMode(["--out", "a"])).toEqual({ kind: "out", dir: "a" })
    expect(parseMode(["--codex-out", "a"])).toEqual({ kind: "codex-out", dir: "a" })
    expect(parseMode(["--codex-out", "a", "--dist", "d"])).toEqual({
      kind: "codex-out",
      dir: "a",
      dist: "d",
    })
    for (const args of [
      [],
      ["--out"],
      ["--out", "--codex-out"],
      ["--out", "a", "--dist", "d"],
      ["--codex-out", "a", "--dist"],
      ["--codex-out", "a", "--other", "d"],
      ["--codex-out", "a", "--dist", "d", "e"],
    ]) {
      expect(parseMode(args)).toBeNull()
    }
  })
})

describe("the built Codex plugin", () => {
  const root = temporaryDirectory("codex-plugin")
  const out = join(root, "codex-envoy")
  // biome-ignore lint/suspicious/noExplicitAny: the manifest is read as JSON, as Codex reads it.
  let manifest: any

  beforeAll(async () => {
    const dist = join(root, "dist-in")
    mkdirSync(dist, { recursive: true })
    writeFileSync(join(dist, "envoy-channel.js"), "// a stand-in bundle\n")
    // Under umask 0002, the one a systemd user unit runs with, so every file and directory the
    // build writes would be group-writable but for the build's own mode step.
    const umask = process.umask(0o002)
    try {
      await buildCodexPlugin(out, dist)
    } finally {
      process.umask(umask)
    }
    manifest = JSON.parse(readFileSync(join(out, ".codex-plugin", "plugin.json"), "utf8"))
  })

  // The pack keeps each entry's mode, so a build's umask would be the published modes.
  test("no entry is writable by group or others, though the build ran under umask 0002", () => {
    const entries = [
      out,
      ...readdirSync(out, { recursive: true }).map((entry) => join(out, String(entry))),
    ]
    const writable = entries.filter((path) => {
      const entry = lstatSync(path)
      return !entry.isSymbolicLink() && (entry.mode & 0o022) !== 0
    })
    expect(writable.map((path) => path.slice(out.length))).toEqual([])
    expect(lstatSync(join(out, "package.json")).mode & 0o777).toBe(0o644)
    // The `dispatch` command keeps execute for every user, which a terminal and npm's bin need.
    expect(lstatSync(join(out, "bin", "dispatch")).mode & 0o777).toBe(0o755)
  })

  test("holds the manifests, the hooks, the skills and the given dist/", () => {
    expect(readdirSync(out).sort()).toEqual(
      [".codex-plugin", "README.md", "bin", "dist", "hooks", "package.json", "skills"].sort(),
    )
    expect(readFileSync(join(out, "dist", "envoy-channel.js"), "utf8")).toBe(
      "// a stand-in bundle\n",
    )
    expect(readdirSync(join(out, "skills")).sort()).toEqual(
      readdirSync(join(packageRoot, "skills")).sort(),
    )
    const hooks = JSON.parse(readFileSync(join(out, "hooks", "hooks.json"), "utf8"))
    expect(Object.keys(hooks.hooks).sort()).toEqual(["PostToolUse", "SessionEnd", "SessionStart"])
    const commands: string[] = Object.values(hooks.hooks)
      .flat()
      .flatMap((group) =>
        (group as { hooks: { command: string }[] }).hooks.map((hook) => hook.command),
      )
    // Every session-hook.js call reads the Codex host's envoy.json, not Claude Code's.
    const sessionHooks = commands.filter((command) => command.includes("session-hook.js"))
    expect(sessionHooks.length).toBeGreaterThan(0)
    for (const command of sessionHooks) expect(command).toEndWith("--host codex")
    expect(commands.some((command) => command.endsWith('codex-hook.js" session-end'))).toBe(true)
  })

  test("has the manifest shape Codex's validator accepts", () => {
    // validate_plugin.py's allowed top-level fields; `hooks` is not among them.
    const allowed = [
      "id",
      "name",
      "version",
      "description",
      "skills",
      "apps",
      "mcpServers",
      "interface",
      "author",
      "homepage",
      "repository",
      "license",
      "keywords",
    ]
    for (const key of Object.keys(manifest)) expect(allowed).toContain(key)
    expect(manifest.name).toMatch(/^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$/)
    expect(manifest.version).toMatch(/^\d+\.\d+\.\d+$/)
    expect(manifest.author.name).toBeTruthy()
    expect(manifest.skills).toBe("./skills/")
    for (const field of [
      "displayName",
      "shortDescription",
      "longDescription",
      "developerName",
      "category",
    ]) {
      expect(typeof manifest.interface[field]).toBe("string")
      expect(manifest.interface[field].trim()).not.toBe("")
    }
    expect(manifest.interface.defaultPrompt.length).toBeGreaterThan(0)
    for (const prompt of manifest.interface.defaultPrompt) expect(prompt.trim()).not.toBe("")
  })

  test("sets no skill to disable-model-invocation, and keeps each explicit-only one out of context", () => {
    for (const name of readdirSync(join(out, "skills"))) {
      const text = readFileSync(join(out, "skills", name, "SKILL.md"), "utf8")
      const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(text)?.[1] ?? ""
      expect(frontmatter).toMatch(/^name: \S/m)
      expect(frontmatter).toMatch(/^description: \S/m)
      expect(frontmatter).not.toMatch(/disable[-_]model[-_]invocation/)
    }
    for (const name of Object.keys(CODEX_EXPLICIT_SKILLS)) {
      const yaml = readFileSync(join(out, "skills", name, "agents", "openai.yaml"), "utf8")
      expect(yaml).toContain("allow_implicit_invocation: false")
    }
  })

  test("names the package the Codex marketplace installs", () => {
    const pkg = JSON.parse(readFileSync(join(out, "package.json"), "utf8"))
    expect(pkg.name).toBe("@sjawhar/codex-envoy")
    expect(pkg.version).toBe(manifest.version)
  })
})
