/** `bun outside-agent-codex-provenance.ts <command> [arguments]`: the reads of the Codex leg's row C0
 *  (`outside-agent-codex.sh`) that a shell cannot make well: the npm registry's provenance
 *  attestation of the published plugin, and the byte comparison of two builds' bundles. Each
 *  command prints one line of compact JSON whose values are plain words (no quote, backslash or
 *  control character), so the leg reads it with `json_field`, and exits 0; it prints
 *  `{"unknown": "<what it could not read>"}` and exits 2 when it read nothing. It holds no
 *  credential.
 *
 *  Commands:
 *    attestation <package> <version>
 *                     the registry's record of <package>@<version> (`OUTSIDE_NPM_REGISTRY`,
 *                     default https://registry.npmjs.org): `integrity`, the tarball's; and its SLSA
 *                     provenance attestation, which `npm publish --provenance` writes: `provenance`
 *                     true or false (a hand-published version has none), `repository` and
 *                     `workflow`, the repository and the workflow file that built it, `ref`,
 *                     `commit`, the source commit, and `subject_matches`, whether the attested
 *                     tarball's sha512 is the registry's integrity.
 *    compare <built> <installed>
 *                     the sha256 of each `dist/*.js` of two plugin directories: `equal`, how many
 *                     match, `differ`, `only_built` and `only_installed`, the names of the rest, or
 *                     "" for none. */

import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

/** The SLSA provenance predicate's type, as npm writes it. */
const slsa = "https://slsa.dev/provenance/v1";

/** `value` as a plain word: printable ASCII alone, with no quote or backslash, at most 200
 *  characters. */
function word(value: unknown): string {
  return (typeof value === "string" ? value : "").replace(/[^ -~]+|["\\]+/g, " ").slice(0, 200);
}

function field(value: unknown, ...path: string[]): unknown {
  let at = value;
  for (const name of path) {
    at = typeof at === "object" && at !== null ? Reflect.get(at, name) : undefined;
  }
  return at;
}

/** What an attestations answer says of provenance: the workflow and source commit of its SLSA
 *  predicate, and whether its subject is the tarball whose integrity is `integrity`
 *  (`sha512-<base64>`). Exported for the leg's tests. */
export function provenanceOf(
  attestations: unknown,
  integrity: string
): Record<string, string | boolean> {
  const list = field(attestations, "attestations");
  const entry = Array.isArray(list)
    ? list.find((each) => field(each, "predicateType") === slsa)
    : undefined;
  if (entry === undefined) return { provenance: false };
  const payload = field(entry, "bundle", "dsseEnvelope", "payload");
  let statement: unknown;
  try {
    statement = JSON.parse(Buffer.from(String(payload ?? ""), "base64").toString("utf8"));
  } catch {
    return { provenance: false, unread: "the attestation's payload is no JSON" };
  }
  const workflow = field(
    statement,
    "predicate",
    "buildDefinition",
    "externalParameters",
    "workflow"
  );
  const dependencies = field(statement, "predicate", "buildDefinition", "resolvedDependencies");
  const source = Array.isArray(dependencies)
    ? dependencies.find((each) => typeof field(each, "digest", "gitCommit") === "string")
    : undefined;
  const subjects = field(statement, "subject");
  const attested = Array.isArray(subjects) ? field(subjects[0], "digest", "sha512") : undefined;
  const expected = integrity.startsWith("sha512-")
    ? Buffer.from(integrity.slice("sha512-".length), "base64").toString("hex")
    : "";
  return {
    provenance: true,
    repository: word(field(workflow, "repository")),
    workflow: word(field(workflow, "path")),
    ref: word(field(workflow, "ref")),
    commit: word(field(source, "digest", "gitCommit")),
    subject_matches: expected !== "" && attested === expected,
  };
}

/** The sha256 of each `dist/*.js` of a plugin directory, by name. */
function bundles(dir: string): Map<string, string> {
  const hashes = new Map<string, string>();
  for (const name of readdirSync(join(dir, "dist")).sort()) {
    if (!name.endsWith(".js")) continue;
    hashes.set(
      name,
      createHash("sha256")
        .update(readFileSync(join(dir, "dist", name)))
        .digest("hex")
    );
  }
  return hashes;
}

/** How two plugin directories' bundles compare. Exported for the leg's tests. */
export function compareBundles(built: string, installed: string): Record<string, string | number> {
  const a = bundles(built);
  const b = bundles(installed);
  const differ = [...a.keys()].filter((name) => b.has(name) && b.get(name) !== a.get(name));
  return {
    equal: [...a.keys()].filter((name) => b.get(name) === a.get(name)).length,
    differ: differ.join(" "),
    only_built: [...a.keys()].filter((name) => !b.has(name)).join(" "),
    only_installed: [...b.keys()].filter((name) => !a.has(name)).join(" "),
  };
}

async function get(url: string): Promise<{ status: number; body: unknown }> {
  const response = await fetch(url, { signal: AbortSignal.timeout(15_000) });
  const text = await response.text();
  try {
    return { status: response.status, body: JSON.parse(text) };
  } catch {
    return { status: response.status, body: text };
  }
}

async function main(): Promise<Record<string, unknown>> {
  const [command, first = "", second = ""] = Bun.argv.slice(2);
  if (command === "compare") {
    if (first === "" || second === "") return { unknown: "compare needs two directories" };
    return compareBundles(first, second);
  }
  if (command !== "attestation") return { unknown: `unknown command ${word(command)}` };
  if (first === "" || second === "")
    return { unknown: "attestation needs a package and a version" };
  const registry = (process.env.OUTSIDE_NPM_REGISTRY ?? "https://registry.npmjs.org").replace(
    /\/+$/,
    ""
  );
  const name = encodeURIComponent(first).replace(/^%40/, "@");
  const record = await get(`${registry}/${name}/${encodeURIComponent(second)}`);
  if (record.status !== 200) {
    return { unknown: `the registry answered ${record.status} for ${word(first)}@${word(second)}` };
  }
  const integrity = word(field(record.body, "dist", "integrity"));
  const url = field(record.body, "dist", "attestations", "url");
  if (typeof url !== "string") return { status: record.status, integrity, provenance: false };
  const attestations = await get(url);
  if (attestations.status === 404) return { status: record.status, integrity, provenance: false };
  if (attestations.status !== 200) {
    return { unknown: `the registry's attestations answered ${attestations.status}` };
  }
  return { status: record.status, integrity, ...provenanceOf(attestations.body, integrity) };
}

if (import.meta.main) {
  let answer: Record<string, unknown>;
  try {
    answer = await main();
  } catch (error) {
    answer = { unknown: word(error instanceof Error ? error.message : String(error)) };
  }
  process.stdout.write(`${JSON.stringify(answer)}\n`);
  process.exit("unknown" in answer ? 2 : 0);
}
