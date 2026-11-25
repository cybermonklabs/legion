// Reading the notices a plugin writes into its own session (`noticeText` of
// `@legion/envoy-client/notice-text`). It lives in this package because the notices are built in
// envoy-client, pi-shared, pi-envoy and pi-legion, each of which tests its own by the same rule:
// each notice states the fields of its kind, and no field holds words that are the same in every
// notice of its kind.

/** A notice kind as its package declares it: the key its lines sit under, its kind, its fields. */
export interface DeclaredNotice {
  readonly root: string;
  readonly kind: string;
  readonly fields: readonly string[];
}

/** The longest run of characters two renderings of one field may share. */
export const SHARED_RUN_LIMIT = 40;

/** One variant of a notice kind (the fields that its notices state together), rendered twice. */
export type RenderedVariant = readonly [string, string];

/** A `key: value` line under a notice's root, as TOON writes a field (a list as `key[n]: …`). */
const FIELD_LINE = /^ {2}([A-Za-z_][A-Za-z0-9_]*)(?:\[\d+\])?: (.*)$/;

/** The longest run of characters that both `a` and `b` hold. */
function sharedRun(a: string, b: string): string {
  let best = "";
  let previous = new Array<number>(b.length + 1).fill(0);
  for (let i = 1; i <= a.length; i += 1) {
    const current = new Array<number>(b.length + 1).fill(0);
    for (let j = 1; j <= b.length; j += 1) {
      if (a[i - 1] !== b[j - 1]) continue;
      const run = (previous[j - 1] ?? 0) + 1;
      current[j] = run;
      if (run > best.length) best = a.slice(i - run, i);
    }
    previous = current;
  }
  return best;
}

/**
 * The fields of a notice's text by key, each value as the text writes it (TOON quotes a value
 * that holds a new line or would read as structure). Throws on a line below the root that is no
 * `key: value` line.
 */
export function noticeFields(text: string): Record<string, string> {
  const fields: Record<string, string> = {};
  for (const line of text.split("\n").slice(1)) {
    const match = FIELD_LINE.exec(line);
    const key = match?.[1];
    const value = match?.[2];
    if (key === undefined || value === undefined) {
      throw new Error(`${JSON.stringify(line)} is no key: value line of a notice`);
    }
    fields[key] = value;
  }
  return fields;
}

/**
 * What the renderings of `declared` break of the rule that a notice states the fields of its
 * kind. `variants` holds each variant of the kind rendered twice, from different values in every
 * field but the one, if any, that names the variant. Each rendering opens with its root line,
 * and every line after it is a `key: value` line whose key the kind declares (or `notice`),
 * with no key twice and one line naming the kind (`notice: <kind>`).
 * Both renderings of a variant state the same fields, so each field is compared with another
 * rendering of it. Every declared field is stated by one variant at least. No field holds the same
 * value in every rendering that states it. No two renderings of a field share a run of more than
 * `SHARED_RUN_LIMIT` characters, which is what a sentence the same in every notice of the kind
 * would share. Empty when they break none of it.
 */
export function noticeProblems(
  declared: DeclaredNotice,
  variants: readonly RenderedVariant[]
): string[] {
  const problems: string[] = [];
  const named = new Set(["notice", ...declared.fields]);
  const parse = (text: string, which: string): Map<string, string> => {
    const [root, ...lines] = text.split("\n");
    if (root !== `${declared.root}:`) {
      problems.push(`${which}: its first line is ${JSON.stringify(root)}, not "${declared.root}:"`);
    }
    const values = new Map<string, string>();
    for (const line of lines) {
      const match = FIELD_LINE.exec(line);
      const key = match?.[1];
      const value = match?.[2];
      if (key === undefined || value === undefined) {
        problems.push(`${which}: ${JSON.stringify(line)} is no key: value line`);
        continue;
      }
      if (!named.has(key))
        problems.push(`${which}: ${key} is no field that ${declared.kind} declares`);
      if (values.has(key)) problems.push(`${which}: ${key} comes twice`);
      values.set(key, value);
    }
    if (values.get("notice") !== declared.kind) {
      problems.push(`${which}: no line names its kind (notice: ${declared.kind})`);
    }
    return values;
  };
  // Each field's values, from every rendering that states it.
  const stated = new Map<string, string[]>();
  variants.forEach(([a, b], index) => {
    const variant = `${declared.kind}, variant ${index + 1}`;
    const first = parse(a, `${variant}, first rendering`);
    const second = parse(b, `${variant}, second rendering`);
    for (const key of new Set([...first.keys(), ...second.keys()])) {
      if (first.has(key) !== second.has(key)) {
        problems.push(`${variant}: one rendering states ${key} and the other does not`);
      }
    }
    for (const rendering of [first, second]) {
      for (const [key, value] of rendering) {
        if (key !== "notice") stated.set(key, [...(stated.get(key) ?? []), value]);
      }
    }
  });
  for (const field of declared.fields) {
    if (!stated.has(field)) problems.push(`${declared.kind}: no rendering states ${field}`);
  }
  for (const [key, values] of stated) {
    const [one, ...others] = values;
    if (others.length > 0 && others.every((value) => value === one)) {
      problems.push(`${declared.kind}: every rendering holds the same ${key}: ${one}`);
    }
    let longest = "";
    values.forEach((a, index) => {
      for (const b of values.slice(index + 1)) {
        const run = sharedRun(a, b);
        if (run.length > longest.length) longest = run;
      }
    });
    if (longest.length > SHARED_RUN_LIMIT) {
      problems.push(
        `${declared.kind}: two renderings of ${key} share a run of ${longest.length} characters: ${JSON.stringify(longest)}`
      );
    }
  }
  return problems;
}
