import { describe, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { buildInto, underDist } from "./package-build";
import { temporaryDirectories } from "./temporary-directories";

const scratch = temporaryDirectories("package-build");

/** A package in a directory of its own whose `build` script bundles one module and whose `fail`
 *  script exits 3 (a program of its own, which takes the `--outdir` that `buildInto` passes on). */
function stubPackage(): string {
  const dir = scratch("package");
  writeFileSync(
    join(dir, "package.json"),
    JSON.stringify({
      name: "stub",
      private: true,
      scripts: {
        build: "bun build src/entry.ts --root src --outdir dist --target bun",
        fail: "bun fail.ts",
      },
    })
  );
  mkdirSync(join(dir, "src"));
  writeFileSync(join(dir, "src", "entry.ts"), "export const answer = 42;\n");
  writeFileSync(join(dir, "fail.ts"), "process.exit(3);\n");
  return dir;
}

describe("buildInto", () => {
  test("lists what the script wrote into the directory it was given", async () => {
    const out = scratch("out");
    expect(await buildInto(stubPackage(), "build", out)).toEqual(["entry.js"]);
  }, 20_000);

  test("throws, naming the script and its exit code, when the script fails", async () => {
    await expect(buildInto(stubPackage(), "fail", scratch("out"))).rejects.toThrow(
      "bun run fail exited 3"
    );
  }, 20_000);
});

describe("underDist", () => {
  test("is the path inside dist/", () => {
    expect(underDist("dist/http-status.js")).toBe("http-status.js");
    expect(underDist("./dist/index.js")).toBe("index.js");
  });

  test("throws for a path outside dist/", () => {
    expect(() => underDist("src/index.ts")).toThrow("src/index.ts is not under dist/");
  });
});
