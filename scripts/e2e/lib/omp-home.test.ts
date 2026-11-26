import { describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// omp_setup_wizard_off, and the controller proof's profile writer that calls it, read back as Oh My
// Pi reads the file: one YAML document. Without these rows a wizard left open shows only in a live
// rig, as a pane that sits on "Setup step 1 of 5". The three model-route libraries' own tests read
// the config.yml each one writes.
const lib = import.meta.dir;
const dir = temporaryDirectory("omp-home-test");
const writeControllerProfile = scriptFunctions(join(lib, "..", "controller-start-tmux.sh"))(
  "write_controller_profile"
);

// sourced runs BODY in bash with lib/omp-home.sh sourced, under PATH alone.
function sourced(body: string) {
  const result = Bun.spawnSync(
    ["bash", "-c", `set -euo pipefail\n. ${JSON.stringify(join(lib, "omp-home.sh"))}\n${body}`],
    { env: { PATH: process.env.PATH ?? "" } }
  );
  return { code: result.exitCode, stderr: result.stderr.toString() };
}

describe("omp_setup_wizard_off", () => {
  test("adds startup.setupWizard: false after the mapping a profile writer already wrote", () => {
    const config = join(dir, "after-a-mapping.yml");
    writeFileSync(config, "modelRoles:\n  default: offline/m1\n");
    expect(sourced(`omp_setup_wizard_off ${JSON.stringify(config)}`)).toEqual({
      code: 0,
      stderr: "",
    });
    expect(Bun.YAML.parse(readFileSync(config, "utf8"))).toEqual({
      modelRoles: { default: "offline/m1" },
      startup: { setupWizard: false },
    });
  });
});

// omp_natives_dir prints the natives directory the operator's own Oh My Pi reads when none of
// PI_NATIVES_DIR, OMP_DOWNLOAD_ROOT, OMP_CONFIG_ROOT and PI_CONFIG_DIR is set (getNativesDir,
// @oh-my-pi/pi-natives' native/natives-dir.js): $XDG_CACHE_HOME/omp/natives when XDG_CACHE_HOME is
// set and $XDG_CACHE_HOME/omp exists, else $HOME/.omp/natives. An unset XDG_CACHE_HOME is no
// ~/.cache to that rule, and $XDG_DATA_HOME/omp, which Oh My Pi 18.18.6-sami.20261119-162325 read in
// its place, is nothing to it. make_omp_home and the four e2e scripts that link a home of their own
// link .omp/natives to what it prints.
describe("omp_natives_dir", () => {
  const cases = [
    { name: "an existing $XDG_CACHE_HOME/omp", cache: "set", made: ["cache"], to: "cache" },
    { name: "an XDG_CACHE_HOME with no omp directory", cache: "set", made: [], to: "home" },
    {
      name: "an unset XDG_CACHE_HOME beside an existing ~/.cache/omp",
      cache: "unset",
      made: ["default-cache"],
      to: "home",
    },
    { name: "an existing $XDG_DATA_HOME/omp", cache: "unset", made: ["data"], to: "home" },
  ] as const;
  for (const [index, { name, cache, made, to }] of cases.entries()) {
    test(`names the natives directory Oh My Pi reads for ${name}`, () => {
      const home = join(dir, `home-${index}`);
      const places = {
        cache: join(home, "xdg-cache", "omp"),
        "default-cache": join(home, ".cache", "omp"),
        data: join(home, "xdg-data", "omp"),
      };
      for (const place of made) mkdirSync(places[place], { recursive: true });
      const result = Bun.spawnSync(
        [
          "bash",
          "-c",
          `set -euo pipefail\n. ${JSON.stringify(join(lib, "omp-home.sh"))}\nomp_natives_dir`,
        ],
        {
          env: {
            PATH: process.env.PATH ?? "",
            HOME: home,
            XDG_DATA_HOME: join(home, "xdg-data"),
            ...(cache === "set" ? { XDG_CACHE_HOME: join(home, "xdg-cache") } : {}),
          },
        }
      );
      expect({ code: result.exitCode, stderr: result.stderr.toString() }).toEqual({
        code: 0,
        stderr: "",
      });
      expect(result.stdout.toString()).toBe(
        `${to === "cache" ? join(places.cache, "natives") : join(home, ".omp", "natives")}\n`
      );
    });
  }
});

describe("the controller proof's profile writer", () => {
  test("writes a config.yml that keeps the roles it names and shuts the setup wizard", () => {
    const agent = join(dir, "controller", "agent");
    expect(
      sourced(`${writeControllerProfile}\nwrite_controller_profile ${JSON.stringify(agent)}`)
    ).toEqual({ code: 0, stderr: "" });
    expect(Bun.YAML.parse(readFileSync(join(agent, "config.yml"), "utf8"))).toEqual({
      modelRoles: {
        default: "offline/m1",
        review: "offline/m1",
        oracle: "offline/m1",
        deep: "offline/m1",
      },
      startup: { setupWizard: false },
    });
  });
});
