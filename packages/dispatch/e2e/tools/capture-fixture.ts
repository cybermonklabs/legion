/** `bun e2e/tools/capture-fixture.ts --issue <KEY> --out <path>`: captures one live issue - its
 *  title, its `spec` and `decisions-md` documents at their highest version, and every ask - into
 *  a JSON fixture, so a test keeps reading those documents after the issue itself is gone.
 *
 *  `--premortem <slug>` captures one document instead: the whole text, the verdict, and the
 *  eleven risk sections (`## Risk 0, ...` and the ten `### N\. ...` under `## The ten ways it
 *  goes wrong`), each with its heading's text and its body verbatim.
 *
 *  `--verify <path>` is the fixture's own acceptance test: it re-reads every source the file
 *  names - each document by the `artifactId` and `version` the file records, never re-resolved
 *  by slug - and prints `same` or `differs` for each compared field, leaving on the first
 *  difference with a non-zero status.
 *
 *  The server is `DISPATCH_URL`, and the bearer token is the contents of the file
 *  `DISPATCH_TOKEN_FILE` names. The token is read, never printed and never written to a fixture.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { parseArgs } from "node:util";

const usage =
  "usage: bun e2e/tools/capture-fixture.ts --issue <KEY> --out <path>\n" +
  "       bun e2e/tools/capture-fixture.ts --issue <KEY> --premortem <slug> --out <path>\n" +
  "       bun e2e/tools/capture-fixture.ts --verify <path>\n" +
  "  DISPATCH_URL         the Dispatch server's base URL\n" +
  "  DISPATCH_TOKEN_FILE  a file holding the bearer token (the token itself is never printed)";

/** The one `## ` section the ten numbered risks sit under. */
const tenWaysHeading = "The ten ways it goes wrong";

interface Server {
  /** `DISPATCH_URL` without a trailing slash. */
  readonly base: string;
  /** The host a fixture records it was captured from: no scheme, no credentials. */
  readonly host: string;
  readonly token: string;
}

/** The two answers the API gives about an artifact, and the two it gives about an issue. */
interface IssueRead {
  readonly key: string;
  readonly title: string;
}

interface ArtifactSummary {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
}

interface ArtifactMeta {
  readonly id: string;
  readonly name: string;
  readonly versions: readonly { readonly number: number }[];
}

interface ArtifactVersionRead {
  readonly number: number;
  readonly markdown: string;
}

/** An ask as the API answers it. An answer carries the chosen option's id as `id`, the person as
 *  `user`, their labels as `selected` and anything they typed as `text`; an ask hangs on a
 *  document through `artifact_id` and on one of its blocks through `block_id`. */
interface AskAnswerRead {
  readonly id: string | null;
  readonly user: string | null;
  readonly selected: readonly string[] | null;
  readonly text: string | null;
  readonly at: string | null;
}

interface AskRead {
  readonly id: string;
  readonly kind: string;
  readonly state: string;
  readonly question: string;
  readonly options: readonly unknown[];
  readonly artifact_id: string | null;
  readonly block_id: string | null;
  readonly answer: AskAnswerRead | null;
}

export interface CapturedDocument {
  readonly artifactId: string;
  readonly name: string;
  readonly version: number;
  readonly markdown: string;
}

export interface CapturedAnswer {
  readonly optionId: string | null;
  readonly selected: readonly string[] | null;
  readonly text: string | null;
  readonly by: string | null;
  readonly at: string | null;
}

export interface CapturedAsk {
  readonly id: string;
  readonly kind: string;
  readonly state: string;
  readonly question: string;
  readonly options: readonly unknown[];
  readonly artifactId: string | null;
  readonly blockId: string | null;
  readonly answer: CapturedAnswer | null;
}

export interface IssueFixture {
  readonly issue: string;
  readonly title: string;
  readonly capturedAt: string;
  readonly capturedFrom: string;
  readonly documents: {
    readonly spec: CapturedDocument;
    readonly decisions: CapturedDocument;
  };
  readonly asks: readonly CapturedAsk[];
}

export interface CapturedRisk {
  readonly index: number;
  readonly title: string;
  readonly markdown: string;
}

export interface PremortemFixture {
  readonly issue: string;
  readonly capturedAt: string;
  readonly capturedFrom: string;
  readonly document: CapturedDocument;
  readonly verdict: string;
  readonly risks: readonly CapturedRisk[];
}

/** The server and the token, or the missing variable's name and exit 2. */
function readServer(): Server {
  const url = process.env.DISPATCH_URL;
  if (url === undefined || url === "") {
    console.error("DISPATCH_URL is unset");
    process.exit(2);
  }
  const tokenFile = process.env.DISPATCH_TOKEN_FILE;
  if (tokenFile === undefined || tokenFile === "") {
    console.error("DISPATCH_TOKEN_FILE is unset");
    process.exit(2);
  }
  const token = readFileSync(tokenFile, "utf8").trim();
  if (token === "") {
    console.error(`DISPATCH_TOKEN_FILE names ${tokenFile}, which holds no token`);
    process.exit(2);
  }
  return { base: url.replace(/\/+$/u, ""), host: new URL(url).host, token };
}

/** One GET, authorized. The path is in the message of a failure; the token never is. */
async function get<T>(server: Server, path: string): Promise<T> {
  const response = await fetch(`${server.base}${path}`, {
    headers: { Authorization: `Bearer ${server.token}` },
  });
  if (!response.ok) {
    throw new Error(`GET ${path} answered ${response.status} ${response.statusText}`);
  }
  return (await response.json()) as T;
}

/** One document at one version: its name from the artifact's metadata and its text from that
 *  version, exactly as the API answers it. `latest` takes the highest version the metadata
 *  lists, which is what a capture pins; a verification passes the number the fixture records. */
async function readDocument(
  server: Server,
  artifactId: string,
  version: number | "latest"
): Promise<CapturedDocument> {
  const meta = await get<ArtifactMeta>(server, `/api/v1/artifacts/${artifactId}`);
  if (meta.versions.length === 0) {
    throw new Error(`artifact ${artifactId} lists no version`);
  }
  const number =
    version === "latest" ? Math.max(...meta.versions.map((one) => one.number)) : version;
  const read = await get<ArtifactVersionRead>(
    server,
    `/api/v1/artifacts/${artifactId}/versions/${number}`
  );
  return { artifactId, name: meta.name, version: number, markdown: read.markdown };
}

/** The issue's document with that slug, at its highest version. */
async function readDocumentBySlug(
  server: Server,
  issue: string,
  slug: string
): Promise<CapturedDocument> {
  const artifacts = await get<readonly ArtifactSummary[]>(
    server,
    `/api/v1/issues/${issue}/artifacts`
  );
  const found = artifacts.find((artifact) => artifact.slug === slug);
  if (found === undefined) {
    const slugs = artifacts.map((artifact) => artifact.slug).join(", ");
    throw new Error(`${issue} has no document whose slug is ${slug}; it has: ${slugs}`);
  }
  return await readDocument(server, found.id, "latest");
}

/** Every ask of the issue, each keeping what the API holds: the chosen option, the labels, the
 *  typed text, the person and the time, and the document or block it hangs on. */
async function readAsks(server: Server, issue: string): Promise<readonly CapturedAsk[]> {
  const asks = await get<readonly AskRead[]>(server, `/api/v1/issues/${issue}/asks`);
  return asks.map((ask) => ({
    id: ask.id,
    kind: ask.kind,
    state: ask.state,
    question: ask.question,
    options: ask.options,
    artifactId: ask.artifact_id,
    blockId: ask.block_id,
    answer:
      ask.answer == null
        ? null
        : {
            optionId: ask.answer.id,
            selected: ask.answer.selected,
            text: ask.answer.text,
            by: ask.answer.user,
            at: ask.answer.at,
          },
  }));
}

interface Section {
  readonly level: number;
  readonly title: string;
  readonly body: string;
}

const headingPattern = /^(?<hashes>#{1,6})\s+(?<title>.*?)\s*$/u;
const fencePattern = /^\s*(?:```|~~~)/u;

/** A section's body: the lines between its heading and the next heading at its level or above,
 *  verbatim, without the blank lines that only separate it from the headings around it. */
function bodyOf(lines: readonly string[]): string {
  let first = 0;
  let last = lines.length;
  while (first < last && lines[first].trim() === "") first += 1;
  while (last > first && lines[last - 1].trim() === "") last -= 1;
  return lines.slice(first, last).join("\n");
}

/** Every heading of the document, in order, with its body. A `#` inside a fenced code block
 *  opens no section. */
export function sectionsOf(markdown: string): readonly Section[] {
  const lines = markdown.split("\n");
  const heads: { level: number; title: string; start: number }[] = [];
  let fenced = false;
  for (const [index, line] of lines.entries()) {
    if (fencePattern.test(line)) {
      fenced = !fenced;
      continue;
    }
    if (fenced) continue;
    const match = headingPattern.exec(line);
    if (match?.groups !== undefined) {
      heads.push({ level: match.groups.hashes.length, title: match.groups.title, start: index });
    }
  }
  return heads.map((head, index) => {
    const next = heads.slice(index + 1).find((other) => other.level <= head.level);
    const end = next === undefined ? lines.length : next.start;
    return {
      level: head.level,
      title: head.title,
      body: bodyOf(lines.slice(head.start + 1, end)),
    };
  });
}

/** The text under `## Verdict`, verbatim. */
export function verdictOf(sections: readonly Section[]): string {
  const verdict = sections.find(
    (section) => section.level === 2 && /^Verdict\b/u.test(section.title)
  );
  if (verdict === undefined) {
    throw new Error("the document has no '## Verdict' heading");
  }
  return verdict.body;
}

/** The eleven risks as one flat list: `## Risk 0, ...` first, then the ten `### ` sections under
 *  `## The ten ways it goes wrong`. Any other count is a changed document, not a fixture. */
export function risksOf(sections: readonly Section[]): readonly CapturedRisk[] {
  const found: { title: string; markdown: string }[] = [];
  const riskZero = sections.find(
    (section) => section.level === 2 && /^Risk 0\b/u.test(section.title)
  );
  if (riskZero !== undefined) {
    found.push({ title: riskZero.title, markdown: riskZero.body });
  }
  const tenWays = sections.findIndex(
    (section) => section.level === 2 && section.title === tenWaysHeading
  );
  if (tenWays === -1) {
    throw new Error(`the document has no '## ${tenWaysHeading}' heading`);
  }
  for (const section of sections.slice(tenWays + 1)) {
    if (section.level <= 2) break;
    if (section.level === 3) {
      found.push({ title: section.title, markdown: section.body });
    }
  }
  if (found.length !== 11) {
    throw new Error(
      `the document holds ${found.length} risk sections, not 11: ` +
        `${riskZero === undefined ? "no" : "one"} '## Risk 0' section and ` +
        `${riskZero === undefined ? found.length : found.length - 1} under '## ${tenWaysHeading}'`
    );
  }
  return found.map((risk, index) => ({ index, title: risk.title, markdown: risk.markdown }));
}

/** The repository's own formatter, so a fixture passes `biome check` as it is written. */
async function formatJson(value: unknown, path: string): Promise<string> {
  const text = `${JSON.stringify(value, null, 2)}\n`;
  const biome = Bun.spawn(["bunx", "biome", "format", `--stdin-file-path=${path}`], {
    stdin: new TextEncoder().encode(text),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [formatted, failure, code] = await Promise.all([
    new Response(biome.stdout).text(),
    new Response(biome.stderr).text(),
    biome.exited,
  ]);
  if (code !== 0) {
    throw new Error(`biome format answered ${code}: ${failure.trim()}`);
  }
  return formatted;
}

async function captureIssue(server: Server, issue: string, out: string): Promise<void> {
  const [read, spec, decisions, asks] = await Promise.all([
    get<IssueRead>(server, `/api/v1/issues/${issue}`),
    readDocumentBySlug(server, issue, "spec"),
    readDocumentBySlug(server, issue, "decisions-md"),
    readAsks(server, issue),
  ]);
  const fixture: IssueFixture = {
    issue: read.key,
    title: read.title,
    capturedAt: new Date().toISOString(),
    capturedFrom: server.host,
    documents: { spec, decisions },
    asks,
  };
  writeFileSync(out, await formatJson(fixture, out));
  console.log(
    `${out}: ${issue}, spec v${spec.version}, decisions v${decisions.version}, ${asks.length} asks`
  );
}

async function capturePremortem(
  server: Server,
  issue: string,
  slug: string,
  out: string
): Promise<void> {
  const document = await readDocumentBySlug(server, issue, slug);
  const sections = sectionsOf(document.markdown);
  const fixture: PremortemFixture = {
    issue,
    capturedAt: new Date().toISOString(),
    capturedFrom: server.host,
    document,
    verdict: verdictOf(sections),
    risks: risksOf(sections),
  };
  writeFileSync(out, await formatJson(fixture, out));
  console.log(`${out}: ${issue}/${slug} v${document.version}, ${fixture.risks.length} risks`);
}

/** One compared field. A difference is the last line printed: the status says which file is
 *  stale, and nothing after it would be read anyway. */
function compare(field: string, captured: unknown, live: unknown): void {
  const same = JSON.stringify(captured) === JSON.stringify(live);
  console.log(`${field}: ${same ? "same" : "differs"}`);
  if (!same) process.exit(1);
}

function compareDocument(field: string, captured: CapturedDocument, live: CapturedDocument): void {
  compare(`${field}.name`, captured.name, live.name);
  compare(`${field}.markdown`, captured.markdown, live.markdown);
}

async function verifyIssue(server: Server, fixture: IssueFixture): Promise<void> {
  const [read, spec, decisions, asks] = await Promise.all([
    get<IssueRead>(server, `/api/v1/issues/${fixture.issue}`),
    readDocument(server, fixture.documents.spec.artifactId, fixture.documents.spec.version),
    readDocument(
      server,
      fixture.documents.decisions.artifactId,
      fixture.documents.decisions.version
    ),
    readAsks(server, fixture.issue),
  ]);
  compare("title", fixture.title, read.title);
  compareDocument("documents.spec", fixture.documents.spec, spec);
  compareDocument("documents.decisions", fixture.documents.decisions, decisions);
  compare("asks.count", fixture.asks.length, asks.length);
  for (const [index, ask] of fixture.asks.entries()) {
    compare(`asks[${index}] ${ask.id}`, ask, asks[index]);
  }
}

async function verifyPremortem(server: Server, fixture: PremortemFixture): Promise<void> {
  const document = await readDocument(
    server,
    fixture.document.artifactId,
    fixture.document.version
  );
  compareDocument("document", fixture.document, document);
  const sections = sectionsOf(document.markdown);
  compare("verdict", fixture.verdict, verdictOf(sections));
  const risks = risksOf(sections);
  compare("risks.count", fixture.risks.length, risks.length);
  for (const [index, risk] of fixture.risks.entries()) {
    compare(`risks[${index}].title`, risk.title, risks[index].title);
    compare(`risks[${index}].markdown`, risk.markdown, risks[index].markdown);
  }
}

async function verify(server: Server, path: string): Promise<void> {
  const fixture = JSON.parse(readFileSync(path, "utf8")) as IssueFixture | PremortemFixture;
  if ("documents" in fixture) {
    await verifyIssue(server, fixture);
    return;
  }
  if ("document" in fixture) {
    await verifyPremortem(server, fixture);
    return;
  }
  throw new Error(`${path} holds neither a "documents" nor a "document" field`);
}

/** The command line, or the usage and exit 2. `parseArgs` refuses an unknown flag and a flag
 *  whose value is missing, which is the same bad command line as the combinations refused here,
 *  so both leave the same way. */
function readArguments(): {
  issue?: string;
  premortem?: string;
  out?: string;
  verify?: string;
} {
  let values: { issue?: string; premortem?: string; out?: string; verify?: string };
  try {
    values = parseArgs({
      options: {
        issue: { type: "string" },
        premortem: { type: "string" },
        out: { type: "string" },
        verify: { type: "string" },
      },
      allowPositionals: false,
    }).values;
  } catch (error) {
    console.error(`${error instanceof Error ? error.message : String(error)}\n${usage}`);
    process.exit(2);
  }
  const capturing = values.issue !== undefined || values.out !== undefined;
  if (values.verify !== undefined && capturing) {
    console.error(`--verify captures nothing, so it takes no --issue and no --out\n${usage}`);
    process.exit(2);
  }
  if (values.verify === undefined && (values.issue === undefined || values.out === undefined)) {
    console.error(`a capture needs both --issue and --out\n${usage}`);
    process.exit(2);
  }
  return values;
}

async function main(): Promise<void> {
  const values = readArguments();
  const server = readServer();
  if (values.verify !== undefined) {
    await verify(server, values.verify);
    return;
  }
  const issue = values.issue as string;
  const out = values.out as string;
  if (values.premortem !== undefined) {
    await capturePremortem(server, issue, values.premortem, out);
    return;
  }
  await captureIssue(server, issue, out);
}

if (import.meta.main) {
  main().catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exit(1);
  });
}
