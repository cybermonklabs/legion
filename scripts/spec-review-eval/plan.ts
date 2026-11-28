/**
 * The fixed change requests of expected.json, and what the form check of Dispatch must read in each
 * before any agent runs.
 */
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { type Expectation, isKind, label, normal } from "./answer";

/** A root of The Spec, with the text of its terms.md. */
export interface Root {
  readonly name: string;
  readonly key: string;
  readonly terms: string;
  readonly termsText: string;
}

/** A fixture of expected.json, with the text of its spec. */
export interface Fixture extends Expectation {
  readonly id: string;
  readonly root: string;
  readonly spec: string;
  readonly specText: string;
}

export interface Plan {
  readonly roots: readonly Root[];
  readonly fixtures: readonly Fixture[];
}

/** A finding of the form check of Dispatch, by its kind and its node. */
export interface FormFinding {
  readonly kind: string;
  readonly node: string;
}

/**
 * What the evaluation reads of the server's answer to `GET /api/v1/artifacts/{id}/spec-form`: the
 * scope and the findings (formProblems), and what the application's own reader read in the version
 * (readingProblems): each text by its kind and its node, each term that the version defines, the
 * root's list of terms, and each approved version under the root that Dispatch cannot read.
 */
export interface FormRead {
  readonly in_scope: boolean;
  readonly scope: string;
  readonly findings: readonly FormFinding[];
  readonly nodes: readonly { readonly kind: string; readonly node: string }[];
  readonly definitions: readonly { readonly term: string }[];
  readonly terms: readonly { readonly term: string }[] | null;
  readonly unread: readonly { readonly line: string }[] | null;
}

/** expected.json with the texts of its fixtures, read from the directory that holds them. */
export async function readPlan(directory: string = import.meta.dir): Promise<Plan> {
  const raw = JSON.parse(await readFile(join(directory, "expected.json"), "utf8")) as {
    roots: Omit<Root, "termsText">[];
    fixtures: Omit<Fixture, "specText">[];
  };
  const fixtureFiles = join(directory, "fixtures");
  const roots = await Promise.all(
    raw.roots.map(async (root) => ({
      ...root,
      termsText: await readFile(join(fixtureFiles, root.terms), "utf8"),
    }))
  );
  const fixtures = await Promise.all(
    raw.fixtures.map(async (fixture) => {
      if (!roots.some((root) => root.name === fixture.root)) {
        throw new Error(
          `expected.json: fixture ${fixture.id} names the root ${fixture.root}, which it does not hold`
        );
      }
      for (const item of fixture.planted) {
        if (!isKind(item.kind)) {
          throw new Error(`expected.json: fixture ${fixture.id} plants the kind ${item.kind}`);
        }
      }
      return { ...fixture, specText: await readFile(join(fixtureFiles, fixture.spec), "utf8") };
    })
  );
  return { roots, fixtures };
}

/**
 * What is wrong with the form check's read of a fixture's spec: it is out of scope, which also
 * means that DISPATCH_SPEC_ROOTS did not reach the server, or the form check finds a text. A
 * fixture holds only what no program decides.
 */
export function formProblems(fixture: Fixture, read: FormRead): string[] {
  if (!read.in_scope) {
    return [
      `${fixture.id} is out of scope (${read.scope}), so DISPATCH_SPEC_ROOTS did not reach the server`,
    ];
  }
  return read.findings.map(
    (one) => `the form check finds ${one.kind} ${one.node} in ${fixture.id}, which no fixture holds`
  );
}

/**
 * What is wrong with a fixture against what the application's own reader read in its spec: the
 * evaluation takes the ids of the statements and the checks, and the names of the terms, from
 * Dispatch. Each planted and listed node is a text that Dispatch read, and each item at the node
 * `terms` is a term that the request defines. A planted undefined noun is a term of neither the
 * root's list nor the request. A planted other meaning, and a listed noun of a text, is a term of
 * one of them, so the reviewer finds it defined. A planted `form-limit` names a rule of the form,
 * no term, and its node can be `prose`, which Dispatch does not read. An approved version under
 * the root that Dispatch cannot read leaves the list of terms partial, so it is a problem too.
 */
export function readingProblems(fixture: Fixture, read: FormRead): string[] {
  const problems = (read.unread ?? []).map(({ line }) => `${fixture.id}: ${line}`);
  const texts = new Set(read.nodes.map(({ node }) => node));
  const defined = new Set(read.definitions.map(({ term }) => normal(term)));
  const listed = new Set((read.terms ?? []).map(({ term }) => normal(term)));
  for (const item of [...fixture.planted, ...fixture.mustNotName]) {
    const what = `${fixture.id}: ${item.kind ?? "the listed node"} ${label(item.node, item.subject)}`;
    if (item.kind === "form-limit") {
      if (!["prose", "terms"].includes(item.node) && !texts.has(item.node)) {
        problems.push(`${what} is no text that Dispatch read`);
      }
      continue;
    }
    if (item.node === "terms") {
      if (item.subject === undefined || !defined.has(normal(item.subject))) {
        problems.push(`${what} is no term that the request defines`);
      }
      continue;
    }
    if (!texts.has(item.node)) problems.push(`${what} is no text that Dispatch read`);
    if (item.subject === undefined) continue;
    const subject = normal(item.subject);
    const isTerm = defined.has(subject) || listed.has(subject);
    if (item.kind === "undefined-noun" && isTerm) {
      problems.push(`${what} is a term that the list or the request defines`);
    } else if (item.kind !== "undefined-noun" && !isTerm) {
      problems.push(`${what} is no term of the list or of the request`);
    }
  }
  return problems;
}
