/** `bun outside-agent-codex-provenance.ts <command> [arguments]`: the reads of the Codex leg's row C0
 *  (`outside-agent-codex.sh`) that a shell cannot make well: the npm registry's provenance
 *  attestation of the published plugin, its tarball, and a byte comparison of two plugin trees. Each
 *  command prints one line of compact JSON whose values are plain words (no quote, backslash or
 *  control character), so the leg reads it with `json_field`, and exits 0; it prints
 *  `{"unknown": "<what it could not read>"}` and exits 2 when it read nothing. It holds no
 *  credential.
 *
 *  Commands:
 *    attestation <package> <version> <tarball file>
 *                     the registry's record of <package>@<version> (npm's own,
 *                     https://registry.npmjs.org; `OUTSIDE_NPM_REGISTRY` names another for this
 *                     file's tests, an https:// origin or one on 127.0.0.1, and the leg never sets
 *                     it): `integrity`, the tarball's; the tarball itself, written to <tarball
 *                     file>, and `tarball_matches`, whether its sha512 is that integrity; and its
 *                     SLSA provenance attestation, which `npm publish --provenance` writes:
 *                     `provenance` true or false (a hand-published version has none),
 *                     `repository` and `workflow`, the repository and the workflow file that
 *                     built it, `ref`, `commit`, the source commit, `subject_matches`, whether
 *                     the attested tarball's sha512 is the registry's integrity, and
 *                     `certificate_matches`, whether the attestation's signing certificate names
 *                     the same repository, workflow, ref and commit in its Fulcio extensions
 *                     (`certificate_problem` says how not, `certificate_unread` that it was not
 *                     read). `npm audit signatures` verifies that certificate's chain and the
 *                     signature; this holds the payload to the certificate.
 *    tree <tarball> <installed>
 *                     every entry of two plugin trees by its path in the tree, a file by its
 *                     sha256 and a symbolic link by its target: `equal`, how many match,
 *                     `differ` (a different file, target or type), `only_tarball` and
 *                     `only_installed`, the paths of the rest, or "" for none. */

import { createHash } from "node:crypto";
import { lstatSync, readdirSync, readFileSync, readlinkSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";

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

/** One DER element: its tag, and where its contents start and end in the buffer. */
interface Der {
  readonly tag: number;
  readonly start: number;
  readonly end: number;
}

/** The DER element at `at`, which must end by `limit`; throws on one that does not. */
function derAt(der: Uint8Array, at: number, limit: number): Der {
  let length = der[at + 1] ?? 0;
  let start = at + 2;
  if (length & 0x80) {
    const count = length & 0x7f;
    if (count === 0 || count > 4) throw new Error("a DER length of no definite form");
    length = 0;
    for (let i = 0; i < count; i += 1) length = length * 256 + (der[start + i] ?? 0);
    start += count;
  }
  if (start + length > limit) throw new Error("a DER element runs past its parent");
  return { tag: der[at] ?? 0, start, end: start + length };
}

/** The elements inside `parent`. */
function derChildren(der: Uint8Array, parent: Der): Der[] {
  const children: Der[] = [];
  for (let at = parent.start; at < parent.end; ) {
    const child = derAt(der, at, parent.end);
    children.push(child);
    at = child.end;
  }
  return children;
}

/** The Fulcio extensions this reads (OID 1.3.6.1.4.1.57264.1.<n>), by their last arc. */
const fulcio = { repository: 12, commit: 13, ref: 14, buildConfig: 18 } as const;

/** The UTF-8 value of each Fulcio extension of a DER X.509 certificate, by the last arc of its
 *  OID. Throws on a certificate it cannot walk. Exported for the leg's tests. */
export function fulcioExtensions(der: Uint8Array): Map<number, string> {
  const prefix = Buffer.from([0x2b, 0x06, 0x01, 0x04, 0x01, 0x83, 0xbf, 0x30, 0x01]);
  const certificate = derAt(der, 0, der.length);
  const tbs = derChildren(der, certificate)[0];
  const wrapper =
    tbs === undefined ? undefined : derChildren(der, tbs).find((each) => each.tag === 0xa3);
  const list = wrapper === undefined ? undefined : derChildren(der, wrapper)[0];
  if (list === undefined) throw new Error("the certificate holds no extensions");
  const found = new Map<number, string>();
  for (const extension of derChildren(der, list)) {
    const [id, ...rest] = derChildren(der, extension);
    const value = rest.at(-1);
    if (id === undefined || value === undefined || id.tag !== 0x06) continue;
    const oid = Buffer.from(der.subarray(id.start, id.end));
    if (oid.length !== prefix.length + 1 || !oid.subarray(0, prefix.length).equals(prefix))
      continue;
    // Fulcio's extensions from 1.8 on hold a DER UTF8String inside the extension's octet string;
    // the older ones (1.1-1.6) hold the raw string, which this reads none of.
    if (der[value.start] !== 0x0c) continue;
    const inner = derAt(der, value.start, value.end);
    found.set(
      oid[prefix.length] ?? 0,
      Buffer.from(der.subarray(inner.start, inner.end)).toString("utf8")
    );
  }
  return found;
}

/** How the certificate's Fulcio extensions differ from the payload's repository, workflow, ref
 *  and commit, in words: an extension missing or different, each named; "" where all match.
 *  Exported for the leg's tests. */
export function certificateProblem(
  extensions: ReadonlyMap<number, string>,
  payload: { repository: string; workflow: string; ref: string; commit: string }
): string {
  const wanted: [string, number, string][] = [
    ["source repository", fulcio.repository, payload.repository],
    ["source commit", fulcio.commit, payload.commit],
    ["source ref", fulcio.ref, payload.ref],
    [
      "build config",
      fulcio.buildConfig,
      `${payload.repository}/${payload.workflow}@${payload.ref}`,
    ],
  ];
  return wanted
    .map(([name, arc, value]) => {
      const named = extensions.get(arc);
      if (named === undefined)
        return `the certificate names no ${name} (1.3.6.1.4.1.57264.1.${arc})`;
      return named === value
        ? ""
        : `the certificate names the ${name} ${word(named)}, the payload ${word(value)}`;
    })
    .filter((each) => each !== "")
    .join("; ");
}

/** What an attestations answer says of provenance: the workflow and source commit of its SLSA
 *  predicate, whether its subject is the tarball whose integrity is `integrity`
 *  (`sha512-<base64>`), and whether its signing certificate names the same. Exported for the
 *  leg's tests. */
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
  const read = {
    repository: word(field(workflow, "repository")),
    workflow: word(field(workflow, "path")),
    ref: word(field(workflow, "ref")),
    commit: word(field(source, "digest", "gitCommit")),
  };
  const raw = field(entry, "bundle", "verificationMaterial", "certificate", "rawBytes");
  let certificate: Record<string, string | boolean>;
  try {
    if (typeof raw !== "string" || raw === "") throw new Error("the bundle holds no certificate");
    const problem = certificateProblem(
      fulcioExtensions(new Uint8Array(Buffer.from(raw, "base64"))),
      read
    );
    certificate = { certificate_matches: problem === "", certificate_problem: problem };
  } catch (error) {
    certificate = {
      certificate_matches: false,
      certificate_unread: word(error instanceof Error ? error.message : String(error)),
    };
  }
  return {
    provenance: true,
    ...read,
    subject_matches: expected !== "" && attested === expected,
    ...certificate,
  };
}

/** Every entry under `dir` that is no directory, by its path relative to `dir`: a file as
 *  `file <sha256>`, a symbolic link as `link <target>`, and anything else as its type alone. */
function files(dir: string): Map<string, string> {
  const entries = new Map<string, string>();
  for (const entry of readdirSync(dir, { recursive: true, withFileTypes: true })) {
    if (entry.isDirectory()) continue;
    const path = join(entry.parentPath, entry.name);
    const kind = lstatSync(path);
    entries.set(
      relative(dir, path),
      kind.isSymbolicLink()
        ? `link ${readlinkSync(path)}`
        : kind.isFile()
          ? `file ${createHash("sha256").update(readFileSync(path)).digest("hex")}`
          : "other"
    );
  }
  return entries;
}

/** How a release's tarball tree and the tree Codex installed compare, entry by entry (files by
 *  their bytes, links by their targets, and a type that differs as a difference): the count of
 *  paths that match, and the paths of the rest. Exported for the leg's tests. */
export function compareTrees(tarball: string, installed: string): Record<string, string | number> {
  const a = files(tarball);
  const b = files(installed);
  const names = (list: string[]) => list.sort().join(" ");
  return {
    equal: [...a.keys()].filter((name) => b.get(name) === a.get(name)).length,
    differ: names([...a.keys()].filter((name) => b.has(name) && b.get(name) !== a.get(name))),
    only_tarball: names([...a.keys()].filter((name) => !b.has(name))),
    only_installed: names([...b.keys()].filter((name) => !a.has(name))),
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
  const [command, first = "", second = "", third = ""] = Bun.argv.slice(2);
  if (command === "tree") {
    if (first === "" || second === "") return { unknown: "tree needs two directories" };
    return compareTrees(first, second);
  }
  if (command !== "attestation") return { unknown: `unknown command ${word(command)}` };
  if (first === "" || second === "" || third === "")
    return { unknown: "attestation needs a package, a version and a tarball file" };
  const registry = (process.env.OUTSIDE_NPM_REGISTRY ?? "https://registry.npmjs.org").replace(
    /\/+$/,
    ""
  );
  // Every URL this reads is https, but on 127.0.0.1 for this file's tests: the registry's, and the
  // tarball's and attestations' that its record names.
  const reachable = (url: string): boolean =>
    /^https:\/\/[^/]|^http:\/\/127\.0\.0\.1[:/]/.test(url);
  if (!reachable(`${registry}/`)) {
    return { unknown: `OUTSIDE_NPM_REGISTRY is no https:// origin: ${word(registry)}` };
  }
  const name = encodeURIComponent(first).replace(/^%40/, "@");
  const record = await get(`${registry}/${name}/${encodeURIComponent(second)}`);
  if (record.status !== 200) {
    return { unknown: `the registry answered ${record.status} for ${word(first)}@${word(second)}` };
  }
  const integrity = word(field(record.body, "dist", "integrity"));
  // The tarball the registry serves for the version, and whether it is the bytes its integrity
  // names: the tree Codex installed is compared with this one.
  const tarballUrl = field(record.body, "dist", "tarball");
  if (typeof tarballUrl !== "string" || !reachable(tarballUrl))
    return { unknown: `the registry's record names no https:// tarball: ${word(tarballUrl)}` };
  const tarball = await fetch(tarballUrl, { signal: AbortSignal.timeout(60_000) });
  if (tarball.status !== 200) return { unknown: `the tarball answered ${tarball.status}` };
  const bytes = Buffer.from(await tarball.arrayBuffer());
  writeFileSync(third, bytes);
  const tarball_matches =
    integrity === `sha512-${createHash("sha512").update(bytes).digest("base64")}`;
  const known = { status: record.status, integrity, tarball_matches };
  const url = field(record.body, "dist", "attestations", "url");
  if (typeof url !== "string") return { ...known, provenance: false };
  if (!reachable(url))
    return { unknown: `the registry's record names no https:// attestations: ${word(url)}` };
  const attestations = await get(url);
  if (attestations.status === 404) return { ...known, provenance: false };
  if (attestations.status !== 200) {
    return { unknown: `the registry's attestations answered ${attestations.status}` };
  }
  return { ...known, ...provenanceOf(attestations.body, integrity) };
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
