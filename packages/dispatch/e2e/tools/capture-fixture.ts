/** `bun e2e/tools/capture-fixture.ts --source-project <KEY> --private-words <path> --issue <N>
 *  --out <path>`: captures the live issue `<KEY>-<N>` - its title, its `spec` and `decisions-md`
 *  documents at their highest version, and every ask - into a JSON fixture, so a test keeps
 *  reading those documents after the issue itself is gone.
 *
 *  `--premortem <slug>` captures one document of that issue instead: the whole text, the verdict,
 *  and the eleven risk sections (`## Risk 0, ...` and the ten `### N\. ...` under `## The ten
 *  ways it goes wrong`), each with its heading's text and its body verbatim.
 *
 *  `--verify <path>` is the fixture's own acceptance test: it re-reads every source the file
 *  names - each document by the `artifactId` and `version` the file records, never re-resolved
 *  by slug - and prints `same` or `differs` for each compared field, leaving on the first
 *  difference with a non-zero status.
 *
 *  A fixture is committed to a public repository, so it names nothing of the deployment it was
 *  captured from: not the live project's key, not the company, not the deployment's repository,
 *  not a host. None of those is ever this file's content or a fixture's. Two inputs give them at
 *  run time, and no run starts without both:
 *  - `--source-project <KEY>`, the live project's key. A capture reads `<KEY>-<N>`, and a
 *    verification reads `<KEY>-<n>` for the `ACME-<n>` the file names;
 *  - `--private-words <path>`, a file the operator keeps outside the repository: one
 *    `word=neutral` per line, blank lines and `#` lines skipped, each word one of the
 *    deployment's own and each neutral form what a fixture writes in its place. A refusal of the
 *    file names the file and the line, never a word.
 *  A capture writes what `neutralize` makes of what the server answers, and a verification makes
 *  the same of the server's answer before it compares, so a file that still matches its source
 *  reads `same` for every field. `neutralize` knows what it is told and no more: a word the file
 *  does not list, or another project's key or link, stays as it is, so read a new capture before
 *  committing it.
 *
 *  A fixture records no host: there is no `capturedFrom`. The real host names the deployment,
 *  and a neutral stand-in would be one constant in every file, which tells a reader nothing.
 *  Which server a run reads is DISPATCH_URL's to say, and a verification finds each document by
 *  its artifactId and version, never by where it was read.
 *
 *  The server is `DISPATCH_URL`, and the bearer token is the contents of the file
 *  `DISPATCH_TOKEN_FILE` names. The token is read, never printed and never written to a fixture.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { parseArgs } from "node:util";
import { PROJECT_KEY_PATTERN, PROJECT_KEY_RULE } from "@legion/contracts";

const usage =
  "usage: bun e2e/tools/capture-fixture.ts --source-project <KEY> --private-words <path>\n" +
  "         --issue <N> --out <path>\n" +
  "       bun e2e/tools/capture-fixture.ts --source-project <KEY> --private-words <path>\n" +
  "         --issue <N> --premortem <slug> --out <path>\n" +
  "       bun e2e/tools/capture-fixture.ts --source-project <KEY> --private-words <path>\n" +
  "         --verify <path>\n" +
  "  --source-project     the live project's key: a capture reads <KEY>-<N>, a verification reads\n" +
  "                       <KEY>-<n> for the file's ACME-<n>, and no fixture names <KEY>\n" +
  "  --private-words      a file the operator keeps outside the repository, one word=neutral per\n" +
  "                       line, blank lines and # lines skipped: no fixture holds a word, in any\n" +
  "                       case, and each is written as its neutral form in the case it had\n" +
  "  --issue              the issue's number in that project\n" +
  "  DISPATCH_URL         the Dispatch server's base URL\n" +
  "  DISPATCH_TOKEN_FILE  a file holding the bearer token (the token itself is never printed)";

/** The one `## ` section the ten numbered risks sit under. */
const tenWaysHeading = "The ten ways it goes wrong";

/** What every fixture names in place of the live project's key, and the domain of the address it
 *  writes in place of every address. */
const neutralProject = "ACME";
const neutralDomain = "example.com";

/** An email address: a local part, `@`, and a domain of at least two labels whose last is a
 *  top-level one. `spec@v1` in a document reference has no dot, so it is no address. */
const addressPattern = /[A-Za-z0-9._%+-]+@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}\b/gu;

interface Server {
  /** `DISPATCH_URL` without a trailing slash. */
  readonly base: string;
  readonly token: string;
}

/** What a run is told about the deployment it reads: what no fixture may hold. */
export interface Deployment {
  /** The live project's key, `--source-project`. */
  readonly project: string;
  /** The words of `--private-words`, each lower-cased, with the neutral form a fixture writes in
   *  its place. */
  readonly privateWords: ReadonlyMap<string, string>;
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

/** The fields in the order a file holds them, which is the order `neutralize` numbers addresses
 *  in. The documents come before the title: they are what a verification gates on, so the
 *  numbers their addresses take depend on no unversioned field. */
export interface IssueFixture {
  readonly issue: string;
  readonly capturedAt: string;
  readonly documents: {
    readonly spec: CapturedDocument;
    readonly decisions: CapturedDocument;
  };
  readonly title: string;
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
  return { base: url.replace(/\/+$/u, ""), token };
}

/** The words of the file `--private-words` names, each lower-cased, with its neutral form: or the
 *  refusal and exit 2. A refusal names the file and the line, never a word, because the words are
 *  the private part. A file that lists no word is refused too: it can only be the wrong file. */
export function readPrivateWords(path: string): ReadonlyMap<string, string> {
  let text: string;
  try {
    text = readFileSync(path, "utf8");
  } catch {
    console.error(`--private-words names ${path}, which is no file this run can read\n${usage}`);
    process.exit(2);
  }
  const words = new Map<string, string>();
  const lineOf = new Map<string, number>();
  for (const [index, raw] of text.split("\n").entries()) {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) continue;
    const at = line.indexOf("=");
    const word = at === -1 ? "" : line.slice(0, at).trim().toLowerCase();
    const neutral = at === -1 ? "" : line.slice(at + 1).trim();
    if (word === "" || neutral === "") {
      console.error(`--private-words: line ${index + 1} of ${path} is not word=neutral\n${usage}`);
      process.exit(2);
    }
    const first = lineOf.get(word);
    if (first !== undefined) {
      console.error(
        `--private-words: line ${index + 1} of ${path} repeats the word of line ${first}\n${usage}`
      );
      process.exit(2);
    }
    lineOf.set(word, index + 1);
    words.set(word, neutral);
  }
  if (words.size === 0) {
    console.error(`--private-words names ${path}, which lists no word=neutral line\n${usage}`);
    process.exit(2);
  }
  return words;
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

/** Every string `value` holds, object keys among them, in the order a file writes them. */
function* stringsOf(value: unknown): Generator<string> {
  if (typeof value === "string") {
    yield value;
  } else if (Array.isArray(value)) {
    for (const item of value) yield* stringsOf(item);
  } else if (value !== null && typeof value === "object") {
    for (const [key, item] of Object.entries(value)) {
      yield key;
      yield* stringsOf(item);
    }
  }
}

/** `value` with `rewrite` run on every string it holds, object keys among them. */
function mapStrings(value: unknown, rewrite: (text: string) => string): unknown {
  if (typeof value === "string") return rewrite(value);
  if (Array.isArray(value)) return value.map((item) => mapStrings(item, rewrite));
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value).map(([key, item]) => [rewrite(key), mapStrings(item, rewrite)])
    );
  }
  return value;
}

/** `text` as a pattern that matches it and nothing else. */
function literally(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&");
}

/** `neutral` in the case `found` is written in: upper, lower, or a capital and then lower. A
 *  `found` with no letter that has a case leaves `neutral` as it is. */
function inCaseOf(found: string, neutral: string): string {
  if (found.toUpperCase() === found.toLowerCase()) return neutral;
  if (found === found.toUpperCase()) return neutral.toUpperCase();
  if (found === found.toLowerCase()) return neutral.toLowerCase();
  return `${neutral.charAt(0).toUpperCase()}${neutral.slice(1).toLowerCase()}`;
}

/** `value` with nothing of the deployment it was read from in any string it holds, object keys
 *  among them. A capture runs it on the fixture it writes, and a verification on the fixture it
 *  rebuilds from what the server answers now, so both sides of every comparison have been through
 *  the same rules, in this order:
 *  - every email address becomes `person-<k>@example.com`, `k` counted from 1 in the order each
 *    distinct address (case aside) first appears in the fixture's own order, so the same input
 *    always gives the same output;
 *  - every word of `--private-words`, in any case, wherever no letter or digit touches it on
 *    either side, becomes its neutral form in the case the word was written in: capitals stay
 *    capitals, and a capital then lower stays so. The longer word goes first, so a word that
 *    holds a shorter one is replaced whole before the shorter could be;
 *  - `<KEY>-<n>`, in any case, becomes `ACME-<n>` in that case (`<key>-<n>` becomes `acme-<n>`),
 *    and so does the space form a person types, `<KEY> <n>`;
 *  - `<KEY>` as an upper-case whole word, `<KEY>'s` among them, becomes `ACME`. With the rule
 *    above, a link into the source project comes out as `dispatch://ACME...`.
 *  `deployment.project` is `<KEY>`, which PROJECT_KEY_PATTERN holds to letters and digits, so it
 *  goes into a pattern as it is. */
export function neutralize<T>(value: T, deployment: Deployment): T {
  const people = new Map<string, string>();
  for (const text of stringsOf(value)) {
    for (const [address] of text.matchAll(addressPattern)) {
      const known = address.toLowerCase();
      if (!people.has(known)) people.set(known, `person-${people.size + 1}@${neutralDomain}`);
    }
  }
  // One alternation, the longer word first: at each place it takes the first word that matches,
  // and nothing it writes is read again by a later word.
  const words = [...deployment.privateWords.keys()]
    .sort((one, other) => other.length - one.length || (one < other ? -1 : 1))
    .map(literally);
  const privateWord =
    words.length === 0
      ? undefined
      : new RegExp(`(?<![\\p{L}\\p{N}])(?:${words.join("|")})(?![\\p{L}\\p{N}])`, "giu");
  const numberedKey = new RegExp(`\\b(${deployment.project})([- ])(\\d+)`, "giu");
  const bareKey = new RegExp(`\\b${deployment.project}\\b`, "gu");
  return mapStrings(value, (text) => {
    let out = text.replace(
      addressPattern,
      (address) => people.get(address.toLowerCase()) ?? address
    );
    if (privateWord !== undefined) {
      out = out.replace(privateWord, (found) =>
        inCaseOf(found, deployment.privateWords.get(found.toLowerCase()) ?? found)
      );
    }
    out = out.replace(
      numberedKey,
      (_, key: string, gap: string, number: string) =>
        `${inCaseOf(key, neutralProject)}${gap}${number}`
    );
    return out.replace(bareKey, neutralProject);
  }) as T;
}

/** One issue's fixture, as a capture writes it and as a verification rebuilds it from what the
 *  server answers now: the fields of `raw` in the file's order, through `neutralize`. */
export function issueFixture(deployment: Deployment, raw: IssueFixture): IssueFixture {
  return neutralize(
    {
      issue: raw.issue,
      capturedAt: raw.capturedAt,
      documents: { spec: raw.documents.spec, decisions: raw.documents.decisions },
      title: raw.title,
      asks: raw.asks,
    },
    deployment
  );
}

/** One pre-mortem's fixture, as a capture writes it and as a verification rebuilds it from the
 *  document the server answers now: its verdict and its risks are read out of that document
 *  first, and all of it then goes through `neutralize`. */
export function premortemFixture(
  deployment: Deployment,
  issue: string,
  capturedAt: string,
  document: CapturedDocument
): PremortemFixture {
  const sections = sectionsOf(document.markdown);
  return neutralize(
    { issue, capturedAt, document, verdict: verdictOf(sections), risks: risksOf(sections) },
    deployment
  );
}

/** The live key of the `ACME-<n>` a fixture names: the same number in the source project. */
function liveKeyOf(issue: string, deployment: Deployment): string {
  const number = new RegExp(`^${neutralProject}-([1-9][0-9]*)$`, "u").exec(issue)?.[1];
  if (number === undefined) {
    throw new Error(`the file names the issue ${issue}, not ${neutralProject}-<n>`);
  }
  return `${deployment.project}-${number}`;
}

async function captureIssue(
  server: Server,
  deployment: Deployment,
  number: string,
  out: string
): Promise<void> {
  const issue = `${deployment.project}-${number}`;
  const [read, spec, decisions, asks] = await Promise.all([
    get<IssueRead>(server, `/api/v1/issues/${issue}`),
    readDocumentBySlug(server, issue, "spec"),
    readDocumentBySlug(server, issue, "decisions-md"),
    readAsks(server, issue),
  ]);
  const fixture = issueFixture(deployment, {
    issue: read.key,
    capturedAt: new Date().toISOString(),
    documents: { spec, decisions },
    title: read.title,
    asks,
  });
  writeFileSync(out, await formatJson(fixture, out));
  console.log(
    `${out}: ${fixture.issue}, spec v${spec.version}, decisions v${decisions.version}, ` +
      `${asks.length} asks`
  );
}

async function capturePremortem(
  server: Server,
  deployment: Deployment,
  number: string,
  slug: string,
  out: string
): Promise<void> {
  const issue = `${deployment.project}-${number}`;
  const document = await readDocumentBySlug(server, issue, slug);
  const fixture = premortemFixture(deployment, issue, new Date().toISOString(), document);
  writeFileSync(out, await formatJson(fixture, out));
  console.log(
    `${out}: ${fixture.issue}/${slug} v${document.version}, ${fixture.risks.length} risks`
  );
}

/** One compared field that gates. A difference is the last line printed: the status says which
 *  file is stale, and nothing after it would be read anyway. */
function compare(field: string, captured: unknown, live: unknown): void {
  const same = JSON.stringify(captured) === JSON.stringify(live);
  console.log(`${field}: ${same ? "same" : "differs"}`);
  if (!same) process.exit(1);
}

/** One reading that does not gate. What a capture records of an unversioned thing - the issue's
 *  title, its asks - is true of the moment it was captured and of no moment after it: a person
 *  may open an ask, answer one or rename the issue at any time, and none of that makes the file
 *  a wrong record of its capture. So drift is printed and counted, and the run still passes.
 *  Gating on it would turn this check red for every live issue as the days pass, and a reader
 *  who meets a red that always means nothing stops reading it. */
function drift(field: string, captured: unknown, live: unknown): boolean {
  const same = JSON.stringify(captured) === JSON.stringify(live);
  console.log(`${field}: ${same ? "same" : "drifted since the capture"}`);
  return same;
}

function compareDocument(field: string, captured: CapturedDocument, live: CapturedDocument): void {
  compare(`${field}.name`, captured.name, live.name);
  compare(`${field}.markdown`, captured.markdown, live.markdown);
}

async function verifyIssue(
  server: Server,
  deployment: Deployment,
  fixture: IssueFixture
): Promise<void> {
  const issue = liveKeyOf(fixture.issue, deployment);
  const [read, spec, decisions, asks] = await Promise.all([
    get<IssueRead>(server, `/api/v1/issues/${issue}`),
    readDocument(server, fixture.documents.spec.artifactId, fixture.documents.spec.version),
    readDocument(
      server,
      fixture.documents.decisions.artifactId,
      fixture.documents.decisions.version
    ),
    readAsks(server, issue),
  ]);
  // What the server answers goes through the capture's own neutralization before anything is
  // compared. Its asks go in the file's order, then those opened since: addresses are numbered
  // in that order, so an ask keeps the number the file gave it unless something before it moved.
  const held = new Set(fixture.asks.map((ask) => ask.id));
  const byId = new Map(asks.map((ask) => [ask.id, ask]));
  const live = issueFixture(deployment, {
    issue: read.key,
    capturedAt: fixture.capturedAt,
    documents: { spec, decisions },
    title: read.title,
    asks: [
      ...fixture.asks.flatMap((ask) => byId.get(ask.id) ?? []),
      ...asks.filter((ask) => !held.has(ask.id)),
    ],
  });
  // The documents gate, because a document read back at the version the file records is
  // reproducible by construction: a difference there means the capture is a false record.
  compareDocument("documents.spec", fixture.documents.spec, live.documents.spec);
  compareDocument("documents.decisions", fixture.documents.decisions, live.documents.decisions);

  // Everything unversioned is read by identity rather than by position. Matching asks by their
  // order in the live answer would make one ask opened in the middle report every ask after it
  // as different, which is a false alarm about this file rather than a fact about Dispatch.
  let drifted = drift("title", fixture.title, live.title);
  const liveAsks = new Map(live.asks.map((ask) => [ask.id, ask]));
  for (const ask of fixture.asks) {
    const now = liveAsks.get(ask.id);
    if (now === undefined) {
      console.log(`asks[${ask.id}]: gone from the issue since the capture`);
      drifted = false;
      continue;
    }
    drifted = drift(`asks[${ask.id}]`, ask, now) && drifted;
  }
  const added = live.asks.filter((ask) => !held.has(ask.id));
  console.log(
    `asks: ${fixture.asks.length} captured, ${live.asks.length} on the issue now` +
      (added.length === 0 ? "" : `, ${added.length} opened since the capture`)
  );
  for (const ask of added) {
    console.log(`asks[${ask.id}]: opened since the capture, not in this file`);
  }
  console.log(
    drifted
      ? "the issue still reads as this file captured it"
      : "the documents are unchanged at their recorded versions; the unversioned parts have moved"
  );
}

async function verifyPremortem(
  server: Server,
  deployment: Deployment,
  fixture: PremortemFixture
): Promise<void> {
  const document = await readDocument(
    server,
    fixture.document.artifactId,
    fixture.document.version
  );
  // The document the server answers is sectioned and neutralized as the capture did it.
  const live = premortemFixture(
    deployment,
    liveKeyOf(fixture.issue, deployment),
    fixture.capturedAt,
    document
  );
  compareDocument("document", fixture.document, live.document);
  compare("verdict", fixture.verdict, live.verdict);
  compare("risks.count", fixture.risks.length, live.risks.length);
  for (const [index, risk] of fixture.risks.entries()) {
    compare(`risks[${index}].title`, risk.title, live.risks[index].title);
    compare(`risks[${index}].markdown`, risk.markdown, live.risks[index].markdown);
  }
}

async function verify(server: Server, deployment: Deployment, path: string): Promise<void> {
  const fixture = JSON.parse(readFileSync(path, "utf8")) as IssueFixture | PremortemFixture;
  if ("documents" in fixture) {
    await verifyIssue(server, deployment, fixture);
    return;
  }
  if ("document" in fixture) {
    await verifyPremortem(server, deployment, fixture);
    return;
  }
  throw new Error(`${path} holds neither a "documents" nor a "document" field`);
}

/** The command line, or the usage and exit 2. `parseArgs` refuses an unknown flag and a flag
 *  whose value is missing, which is the same bad command line as the combinations refused here,
 *  so both leave the same way. */
function readArguments(): {
  deployment: Deployment;
  issue?: string;
  premortem?: string;
  out?: string;
  verify?: string;
} {
  let values: {
    "source-project"?: string;
    "private-words"?: string;
    issue?: string;
    premortem?: string;
    out?: string;
    verify?: string;
  };
  try {
    values = parseArgs({
      options: {
        "source-project": { type: "string" },
        "private-words": { type: "string" },
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
  const sourceProject = values["source-project"];
  if (sourceProject === undefined || sourceProject === "") {
    console.error(`--source-project is unset: no run reads a live issue without it\n${usage}`);
    process.exit(2);
  }
  if (!PROJECT_KEY_PATTERN.test(sourceProject)) {
    console.error(
      `--source-project is ${sourceProject}, not a project key: ${PROJECT_KEY_RULE}\n${usage}`
    );
    process.exit(2);
  }
  const privateWords = values["private-words"];
  if (privateWords === undefined || privateWords === "") {
    console.error(`--private-words is unset: no run reads a live issue without it\n${usage}`);
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
  if (values.issue !== undefined && !/^[1-9][0-9]*$/u.test(values.issue)) {
    console.error(
      `--issue is ${values.issue}, not an issue number: the project is --source-project\n${usage}`
    );
    process.exit(2);
  }
  return {
    deployment: { project: sourceProject, privateWords: readPrivateWords(privateWords) },
    issue: values.issue,
    premortem: values.premortem,
    out: values.out,
    verify: values.verify,
  };
}

async function main(): Promise<void> {
  const values = readArguments();
  const server = readServer();
  if (values.verify !== undefined) {
    await verify(server, values.deployment, values.verify);
    return;
  }
  const number = values.issue as string;
  const out = values.out as string;
  if (values.premortem !== undefined) {
    await capturePremortem(server, values.deployment, number, values.premortem, out);
    return;
  }
  await captureIssue(server, values.deployment, number, out);
}

if (import.meta.main) {
  main().catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exit(1);
  });
}
