import { expect, test } from "bun:test"
import { readdirSync } from "node:fs"
import { join } from "node:path"
import { bunChildren } from "./bun-children"

// Every Bun process a test of this package starts with an environment of its own turns the
// transpiler cache off (`./transpiler-cache.ts`): a child that kept the cache under the empty HOME
// a test gives it wrote one file and one rename per module before its own code ran, and on a
// loaded disk that start decided rows (dispatch://INFRA-3904). A test that starts one without it
// fails here by file and line, unforced, however fast its own disk is.
test("every Bun child a test of this package starts with an environment of its own turns the transpiler cache off", () => {
  // A child that inherits this process's environment inherits the cache off: the package's test
  // preload sets it (`./no-transpiler-cache-preload.ts`, bunfig.toml).
  expect(process.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"]).toBe("0")
  const files = readdirSync(import.meta.dir, { recursive: true })
    .map(String)
    .filter((file) => file.endsWith(".ts") && !file.includes("node_modules"))
  const children = files.flatMap((file) =>
    bunChildren(join(import.meta.dir, file)).map((child) => ({ file, ...child })),
  )
  // The scan read the tests: the open-asks hook's runner and the channel server's are among them.
  expect(children.map((child) => child.file)).toEqual(
    expect.arrayContaining(["open-asks-hook.test.ts", "envoy-channel-server.test.ts"]),
  )
  expect(
    children
      .filter((child) => !child.cacheOff)
      .map((child) => `tests/${child.file}:${child.line} ${child.text}`),
    `Bun children with an environment that keeps the cache, of ${children.length} in ${files.length} files`,
  ).toEqual([])
})
