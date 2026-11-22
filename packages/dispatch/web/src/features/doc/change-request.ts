/**
 * One block of The Spec as a person reads it before and after an approval (R22): its type, its id,
 * its attributes of The Spec as label and value, and its body. A node of a change request is read
 * from two texts: the issue's block (`issue`), which names its relations as the issue writes them
 * (`under`, `supports`, `statement`) beside its own place in the conversation (`parent`, `serves`)
 * and the request's mechanics (`base`, `removes`), and the approved version as The Spec's files
 * print it (`approved`, `specgraph` `Print`), which names the same relations `parent` and `serves`.
 * Both read into the issue's names, in one order per type, so a person compares like with like and
 * reads no mechanics of the request as a fact of the node.
 */
export interface BlockParts {
  readonly type: string;
  readonly id: string;
  readonly attributes: readonly (readonly [string, string])[];
  readonly body: string;
}

/** The attributes of The Spec each block type shows, in the issue's names and in this order. */
const SHOWN: Readonly<Record<string, readonly string[]>> = {
  check: ["statement", "subject", "procedure", "runner", "selector", "job", "time_limit"],
  component: ["name", "kind", "under", "joins"],
  requirement: ["kind", "under", "supports", "about"],
};

/** The issue's name of each relation the approved text names otherwise, by block type. */
const APPROVED_NAMES: Readonly<Record<string, Readonly<Record<string, string>>>> = {
  check: { serves: "statement" },
  component: { parent: "under" },
  requirement: { parent: "under", serves: "supports" },
};

/** The opening line of a block: `:::<type>{<attributes>}`. */
const OPENING = /^:::([a-z][a-z0-9_-]*)\{(.*)\}\s*$/;

/** One attribute of an opening line: `#<id>`, or `<name>="<value>"`. */
const ATTRIBUTE = /#([^\s"}]+)|([a-z_][a-z0-9_-]*)="([^"]*)"/g;

/** A value as its opening line escapes it (`specgraph` `valueReferences`), read back. */
function attributeValue(value: string): string {
  return value.replaceAll("&quot;", '"').replaceAll("&amp;", "&");
}

/**
 * The parts of `text`, one block as the issue's version holds it (`issue`) or as the approved spec
 * prints it (`approved`). A text that opens no block reads as its whole text, with no type, no id
 * and no attribute.
 */
export function blockParts(text: string, form: "issue" | "approved"): BlockParts {
  const lines = text.replace(/\n+$/, "").split("\n");
  const opening = OPENING.exec(lines[0] ?? "");
  if (opening === null) return { attributes: [], body: text, id: "", type: "" };
  const [, type = "", written = ""] = opening;
  const closes = lines.length > 1 && lines[lines.length - 1] === ":::";
  const body = lines.slice(1, closes ? -1 : undefined).join("\n");
  let id = "";
  const values = new Map<string, string>();
  const renames = form === "approved" ? (APPROVED_NAMES[type] ?? {}) : {};
  for (const match of written.matchAll(ATTRIBUTE)) {
    const [, named, name, value] = match;
    if (named !== undefined) {
      id = named;
      continue;
    }
    if (name === undefined || value === undefined) continue;
    values.set(renames[name] ?? name, attributeValue(value));
  }
  const attributes = (SHOWN[type] ?? []).flatMap((name) => {
    const value = values.get(name);
    return value === undefined ? [] : [[name, value] as const];
  });
  return { attributes, body, id, type };
}
