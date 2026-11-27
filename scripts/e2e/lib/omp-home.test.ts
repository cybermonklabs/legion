import { describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
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
