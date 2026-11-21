/**
 * The scan of the fixtures for keys and private names (testdata/CAPTURE.md, "The scan"):
 *
 *   bun scripts/skill-cuts/fixturescan.ts --key-file <file> --terms <file> <dir>
 *
 * It reads every file under `<dir>`, and the decoded content of each GitHub contents answer (a
 * JSON file whose `encoding` is `base64`), and reports as a hit:
 *
 * 1. any piece of 16 characters of the key `--key-file` holds (the key is read here and printed
 *    nowhere);
 * 2. any key or token shape (SHAPES): an `sk-` key, a GitHub token or fine-grained token prefix,
 *    an AWS key id, a PEM block header, a `Bearer` credential, the key header's name, an IP
 *    address other than 127.0.0.1, or an email address other than the example and no-reply ones;
 * 3. any private name: each line of `--terms` is a regular expression, matched in any case (a
 *    blank line, or one that starts with `#`, is skipped). The terms file is the operator's and
 *    stays outside the repository, since the names it lists are private; a hit names the file and
 *    the terms file's line, never the text it matched;
 * 4. any run of 32 or more hex digits, or of 16 or more characters mixing capitals, small letters
 *    and digits, that is not the value of a JSON key of a known kind (KNOWN_KINDS), nor a run that
 *    a file under `<dir>` holds as the value of a known kind (a git object id a GitHub answer gives
 *    as its `sha`, or an id in a pull row's piece of a line cut in the middle).
 *
 * Shapes and private names are checked in each file's path too. A JSON file is read value by
 * value, a JSONL file and a psql capture (`.txt`) line by line, an SSE file by its `data:` lines;
 * a string value that holds JSON, or JSON lines, is read the same way. Prints each hit, then how
 * many runs of each known kind it accounted for. Exit 0 when nothing is found, 1 on any hit, 2 on
 * a refusal before the scan (the usage, or a key, terms or directory it cannot read).
 */
import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";

/** Key and token shapes, and addresses, that no fixture may hold. */
const SHAPES: readonly (readonly [string, RegExp])[] = [
  ["sk- key", /(?<![A-Za-z0-9])sk-[A-Za-z0-9_-]{6,}/g],
  ["GitHub token", /\b(ghp|gho|ghs|ghu|ghr)_[A-Za-z0-9]{10,}|github_pat_/g],
  ["AWS key id", /AKIA[0-9A-Z]{12,}/g],
  ["private key block", /-----BEGIN/g],
  ["Bearer credential", /Bearer [A-Za-z0-9._~+/-]{8,}/g],
  ["key header", /x-api-key/gi],
  ["IP address", /(?<![\d.])\d{1,3}(?:\.\d{1,3}){3}(?![\d.])/g],
  ["email address", /[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}/g],
];

/** Matches of SHAPES that name nothing private: the loopback address, and the example and
 *  no-reply addresses. */
const ALLOWED: Readonly<Record<string, true>> = {
  "127.0.0.1": true,
  "owner@example.com": true,
  "noreply@anthropic.com": true,
  "noreply@github.com": true,
  "acme-agent[bot]@users.noreply.github.com": true,
};

/** The JSON keys whose values may be long random-looking runs, by kind. */
const KNOWN_KINDS: readonly (readonly [string, RegExp])[] = [
  [
    "an id",
    /^(id|uuid|node_id|sha|session_id|request_id|device_id|item_id|call_id|tool_use_id|[a-zA-Z]*(Id|ID|Uuid|UUID)|(llm_provider-)?(x-)?request-id|x-claude-code-session-id)$/,
  ],
  ["Claude Code's metadata.user_id (a JSON string holding its random device id)", /^user_id$/],
  ["a git object id in a GitHub URL", /(^|_)url$|^(git|html|self)$/],
  ["a thinking signature", /^signature$/],
  ["a Responses reasoning item's encrypted content", /^encrypted_content$/],
  ["a Claude Code request digest or blob hash", /^(requestDigest|shapeHash|hash|tail)$/],
];

const USAGE = "usage: fixturescan.ts --key-file <file> --terms <file> <dir>";

/** One string value of a file, with the JSON key it was found under (`(text)` for plain text). */
interface Value {
  readonly key: string;
  readonly text: string;
}

/** Each string value of `value`, under its nearest JSON key; a string that holds JSON or JSON
 *  lines is read too. */
function walk(value: unknown, key: string, out: Value[]): void {
  if (typeof value === "string") {
    out.push({ key, text: value });
    if (/^\s*[[{]/.test(value)) readJson(value, key, out);
  } else if (Array.isArray(value)) {
    for (const item of value) walk(item, key, out);
  } else if (typeof value === "object" && value !== null) {
    for (const [name, item] of Object.entries(value)) walk(item, name, out);
  }
}

/** `text` read as one JSON value, or else as JSON lines; a line that is not JSON is kept as text
 *  under `key`. */
function readJson(text: string, key: string, out: Value[]): void {
  try {
    walk(JSON.parse(text), key, out);
    return;
  } catch {
    // Not one JSON value: read it line by line.
  }
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) continue;
    try {
      walk(JSON.parse(trimmed), key, out);
    } catch {
      out.push({ key, text: line });
    }
  }
}

/** Every file below `dir`. */
function filesBelow(dir: string): string[] {
  return readdirSync(dir)
    .sort()
    .flatMap((name) => {
      const file = path.join(dir, name);
      return statSync(file).isDirectory() ? filesBelow(file) : [file];
    });
}

/** The private-name patterns of the terms file's text, each with its line number. */
function termsOf(text: string): { line: number; pattern: RegExp }[] {
  return text.split("\n").flatMap((raw, index) => {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) return [];
    return [{ line: index + 1, pattern: new RegExp(line, "i") }];
  });
}

/** The command. Returns the exit status: 0 nothing found, 1 a hit, 2 refused. */
function main(
  argv: readonly string[],
  io: { out(text: string): void; err(text: string): void }
): number {
  const refuse = (why: string) => {
    io.err(`fixturescan: ${why}`);
    return 2;
  };
  let values: { "key-file"?: string; terms?: string };
  let positionals: string[];
  try {
    ({ values, positionals } = parseArgs({
      args: [...argv],
      options: { "key-file": { type: "string" }, terms: { type: "string" } },
      strict: true,
      allowPositionals: true,
    }));
  } catch (error) {
    return refuse(`${(error as Error).message}. ${USAGE}`);
  }
  const root = positionals[0];
  if (values["key-file"] === undefined || values.terms === undefined || root === undefined) {
    return refuse(`--key-file, --terms and one directory are required. ${USAGE}`);
  }
  if (positionals.length > 1) return refuse(`one directory only. ${USAGE}`);
  let key: string;
  let terms: { line: number; pattern: RegExp }[];
  let files: string[];
  try {
    key = readFileSync(values["key-file"], "utf8").trim();
    terms = termsOf(readFileSync(values.terms, "utf8"));
    files = filesBelow(root);
  } catch (error) {
    return refuse((error as Error).message);
  }
  if (key.length < 16) return refuse("the key file holds fewer than 16 characters");
  if (terms.length === 0) return refuse("the terms file lists no private name");

  let hits = 0;
  const hit = (line: string) => {
    io.out(line);
    hits += 1;
  };
  const counts = new Map<string, number>();
  const count = (kind: string) => counts.set(kind, (counts.get(kind) ?? 0) + 1);
  const knownRuns = new Set<string>();
  const unknown: { name: string; key: string; run: string }[] = [];

  /** Hits of the shapes and the private names in `text` of the file `name` (`where` says what
   *  part of it the text is). */
  const checkText = (name: string, where: string, text: string) => {
    for (const [kind, shape] of SHAPES) {
      for (const found of text.matchAll(shape)) {
        if (ALLOWED[found[0]] !== true) hit(`${name}${where}: ${kind}: ${found[0].slice(0, 40)}`);
      }
    }
    for (const term of terms) {
      if (term.pattern.test(text)) {
        hit(`${name}${where}: the private name of terms line ${term.line}`);
      }
    }
  };

  for (const file of files) {
    const name = path.relative(root, file);
    const raw = readFileSync(file, "utf8");
    const texts = [raw];
    const found: Value[] = [];
    try {
      if (file.endsWith(".json")) {
        const parsed = JSON.parse(raw);
        if (parsed?.encoding === "base64" && typeof parsed.content === "string") {
          texts.push(Buffer.from(parsed.content, "base64").toString("utf8"));
          count("a GitHub contents answer's base64 file content");
          parsed.content = "";
        }
        walk(parsed, "", found);
      } else if (file.endsWith(".jsonl")) {
        for (const line of raw.split("\n")) if (line !== "") walk(JSON.parse(line), "", found);
      } else if (file.endsWith(".sse")) {
        for (const line of raw.split("\n")) {
          if (line.startsWith("data: ") && line !== "data: [DONE]") {
            walk(JSON.parse(line.slice("data: ".length)), "", found);
          }
        }
      } else if (file.endsWith(".txt")) {
        readJson(raw, "(text)", found);
      } else {
        found.push({ key: "(text)", text: raw });
      }
    } catch (error) {
      hit(`${name}: does not parse as its extension says (${String(error).slice(0, 80)})`);
    }
    for (const text of texts) {
      for (let at = 0; at + 16 <= key.length; at += 1) {
        if (text.includes(key.slice(at, at + 16))) {
          hit(`${name}: holds a piece of the key`);
          break;
        }
      }
      checkText(name, "", text);
    }
    checkText(name, " (its path)", name);
    for (const value of found) {
      for (const run of value.text.split(/[^A-Za-z0-9+=]+/)) {
        const hex = /^[0-9a-fA-F]{32,}$/.test(run);
        const mixed =
          run.length >= 16 && /[A-Z]/.test(run) && /[a-z]/.test(run) && /[0-9]/.test(run);
        if (!hex && !mixed) continue;
        const kind = KNOWN_KINDS.find(([, keys]) => keys.test(value.key))?.[0];
        if (kind === undefined) {
          unknown.push({ name, key: value.key, run });
          continue;
        }
        count(kind);
        knownRuns.add(run);
      }
    }
  }
  for (const { name, key: where, run } of unknown) {
    if (knownRuns.has(run)) {
      count("a run another value here holds as a known kind");
    } else {
      hit(
        `${name}: a random-looking run of ${run.length} characters in "${where}": ${run.slice(0, 6)}…`
      );
    }
  }
  io.out(`scanned ${files.length} files under ${root}`);
  for (const [kind, n] of counts) io.out(`accounted for: ${n} x ${kind}`);
  io.out(
    hits === 0 ? "clean: no key piece, no key or token shape, no private name" : `${hits} hits`
  );
  return hits === 0 ? 0 : 1;
}

if (import.meta.main) {
  process.exit(
    main(Bun.argv.slice(2), {
      out: (text) => console.log(text),
      err: (text) => console.error(text),
    })
  );
}
