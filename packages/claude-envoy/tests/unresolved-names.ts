// A `--preload` for a child process that answers its fetch of a name under `.invalid` as the
// system's resolver answers a name it cannot find, and asks no DNS server. RFC 6761 reserves
// `.invalid`: no such name resolves, so the stand-in gives a resolver's answer without a resolver's
// wait, and a row's verdict is what the child does with the failure, not how fast the machine's DNS
// answers. Measured on Bun 1.4.4 (`.bun-version`), a fetch of such a name throws a TypeError
// `getaddrinfo ENOTFOUND <host>` with that code and syscall, and errno 4; the stand-in throws the
// same, so the child's error still names the host, as the real one does. Every other request goes
// to the real fetch. Each name the stand-in answered is written to a file of its own, one per line,
// so a row can check that its request reached the stand-in rather than the machine's resolver.

import { readFile, writeFile } from "node:fs/promises"
import { join } from "node:path"

export interface UnresolvedNames {
  /** The file to give the child as `--preload`. */
  readonly preload: string
  /** The names the stand-in answered, in order. */
  readonly asked: () => Promise<string[]>
}

/** Writes the preload and its log into `directory`, a directory of the row's own. */
export async function unresolvedNames(directory: string): Promise<UnresolvedNames> {
  const log = join(directory, "unresolved-names.log")
  const preload = join(directory, "unresolved-names.preload.ts")
  await writeFile(log, "")
  await writeFile(
    preload,
    [
      `import { appendFileSync } from "node:fs"`,
      `const real = globalThis.fetch`,
      `const unresolved = async (input: string | URL | Request, init?: RequestInit) => {`,
      `  const { hostname } = new URL(input instanceof Request ? input.url : String(input))`,
      `  if (!hostname.endsWith(".invalid")) return await real(input, init)`,
      `  appendFileSync(${JSON.stringify(log)}, \`\${hostname}\\n\`)`,
      `  throw Object.assign(new TypeError(\`getaddrinfo ENOTFOUND \${hostname}\`), {`,
      `    code: "ENOTFOUND",`,
      `    syscall: "getaddrinfo",`,
      `    hostname,`,
      `    errno: 4,`,
      `  })`,
      `}`,
      `globalThis.fetch = Object.assign(unresolved, { preconnect: real.preconnect }) as typeof fetch`,
      "",
    ].join("\n"),
  )
  return {
    preload,
    asked: async () => (await readFile(log, "utf8")).split("\n").filter((name) => name !== ""),
  }
}
