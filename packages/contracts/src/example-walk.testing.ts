import {
  FORM_PROBES,
  type Form,
  formOf,
  objectSchema,
  schemaNode,
  takenForms,
  takes,
  VALUE_FORMS,
  valueSet,
} from "./schema-inspection.testing";

export type { Form } from "./schema-inspection.testing";

/**
 * How the Dispatch contract tests hold a shared example file to the schemas that read it: the one
 * walk that `dispatch-issue-events.test.ts` (the issue event envelopes Dispatch's outbox
 * publishes) and `dispatch-inbound-parity.test.ts` (the targeted-delivery fixture) share. Each
 * test keeps its own schemas, which of them it walks where, and its exception tables with their
 * reasons; this module holds the rule. It is used by tests alone, and nothing of the package
 * exports it.
 *
 * - **Keys.** Each object of an example must hold every key its schema declares, except a key
 *   `never` names for that object (one the writer never writes there) or the example's `absent`
 *   names for that key's path (one that write leaves out), and must hold no key `never` names.
 *   An object a writer's strict schema reads must hold no key that schema does not declare, and
 *   the schema itself must refuse an object holding one, unless `loose` names the object with
 *   the reason it takes one (`object`). An exception no walk used is stale, and fails too
 *   (`staleExceptions`).
 * - **Values** (`unheldValues`). Each closed set of values a walked schema declares - an enum, a
 *   literal, a union of them, a boolean - must have each of its values held by some example,
 *   unless the test's table names the value at one of the places the set is read, with the
 *   reason. The sets of the union members no example takes count too. So a schema that drops a
 *   value fails to parse the example that holds it, and one that gains a value fails here.
 * - **Closed sets** (`closedSets`). Each place a walked schema reads a closed set of values at -
 *   an enum, a literal or a union of them, through its wrappers, a union member's no example
 *   takes included - must be one the test's list names, and each place the list names must read
 *   a closed set alone and refuse a value outside it. So a set that is opened - turned into a
 *   string, or joined with one, or given a fallback that takes any value - fails here by its
 *   place, and so does a new closed set until the list names it.
 * - **Forms** (`widenings`). The value each walk starts at, each key of every object a walk
 *   reads, each element of a list it reads and each value of a map must be refused by its schema
 *   in every form no example holds there - a key absent, a value null, a string, an empty string,
 *   a whole number above zero, zero, a negative one, a fraction, a whole number past the exact
 *   range, a boolean, an object, a list, or any value at all - unless the test's table names the
 *   form with the reason the schema takes it. Which forms a schema takes is asked of zod's own
 *   parse of the place's probe values (`takenForms`), never read from its definition, so a schema
 *   that starts taking a form its writer never writes - a required key made optional, a value
 *   made nullable, a number that takes a string, a list whose element takes null, a refinement
 *   dropped, a fallback that takes anything, and each of these written as a coercion or a
 *   preprocess as well as a union - fails here, whichever schema it is: an envelope's, an
 *   event's, a payload's or a tolerant reader's.
 * - **The daemon's decoder** (`daemonAgreement`). For a test whose examples a Go reader decodes,
 *   each form a schema takes at a place an example holds must be one that reader's decoder takes
 *   there, and each form the decoder takes must be one the schema takes, unless the test's table
 *   names the form at that place with the reason the two differ. A place where the decoder takes
 *   every form a value holds is one it reads nothing of, and asks nothing; a key left out is
 *   compared at a key alone.
 * - **Union members no example takes** are walked with a value built from their own schema
 *   (`untaken`): their keys, values, closed sets and forms are held as an example's are, none of
 *   the built value's forms counting as held. So a member's key made nullable, or a closed set
 *   added at any depth of it, fails as it would in a member an example takes.
 *
 * Every exception is by the name of the object it is for (`labels`, or the place the walk reaches
 * an unnamed object at), so each name must be one schema's: two schemas the walk reads under one
 * name, which an exception for one would excuse in the other, fail too.
 */

/** One example a walk reads: its name in the problems, and each key a schema declares that the
 *  example leaves out, by the key's path, with the reason. */
export interface WalkedExample {
  readonly name: string;
  readonly absent: Readonly<Record<string, string>>;
}

/** The rules of one test's walks. */
export interface WalkRules {
  /** Each key a schema declares that its writer never writes, by the name of the object that
   *  declares it, with the reason. */
  readonly never: Readonly<Record<string, Readonly<Record<string, string>>>>;
  /** The name each object schema is known by; an object not named here is named by the place
   *  the walk reaches it at. A name is one schema's. */
  readonly labels: ReadonlyMap<unknown, string>;
  /** What the test calls its table of `absent` entries, as the problems name it. */
  readonly absentTable: string;
  /** Each object a strict walk reads whose schema takes a key it does not declare, by its name,
   *  with the reason; every other such object's schema must refuse one. */
  readonly loose: Readonly<Record<string, string>>;
}

/** How a walk reads its value: `tolerant` for a reader's schema, which keeps the keys it declares
 *  and passes the others over. */
export interface WalkMode {
  readonly tolerant: boolean;
}

/** A test's table of the forms its schemas take that no example holds, by the place
 *  (`<object>.<key>`, `<place>[]` for a list's element, `<place>.*` for a map's value, the object's
 *  name alone for the value a walk starts at), then by the form, each with the reason the schema
 *  takes it. `daemonAgreement` reads one of these too, by the same places, for the forms the
 *  schema and the daemon's decoder differ on; an entry there may also name `<place> @ <path>`. */
export type Wider = Readonly<Record<string, Readonly<Partial<Record<Form, string>>>>>;

/** What the Legion daemon's decoder takes, as its own test wrote it
 *  (`packages/daemon/internal/intake/testdata/dispatch/daemon-verdicts.json`): the probe values of
 *  each form, and for each example and each place it holds (`<example> <path>`), each form the
 *  decoder takes there. */
export interface DaemonVerdicts {
  readonly probes: Readonly<Record<string, readonly unknown[]>>;
  readonly verdicts: Readonly<Record<string, readonly string[]>>;
}

/** The values a built value of a union member no example takes holds, by the JSON kind its
 *  schema declares: the first its schema takes is used. */
const BUILT: Readonly<Record<string, readonly unknown[]>> = {
  string: ["x", "1"],
  number: [1],
  boolean: [true],
  null: [null],
  any: [{}],
};

/** One closed set of values a schema declares: its values, the places it is read at, and the
 *  values the examples hold there, each as its JSON. */
interface ValueSet {
  readonly values: readonly unknown[];
  readonly places: Set<string>;
  readonly held: Set<string>;
}

/** What the walks show of one place's forms: each its schema takes, and each an example holds. */
interface PlaceForms {
  readonly taken: Set<Form>;
  readonly held: Set<Form>;
}

/** What the walks show of the set of values one place reads: its closed set's values, or
 *  undefined where the schema reads an open set there, and each value outside that set the
 *  schema took, as its JSON. */
interface PlaceSet {
  readonly values: readonly unknown[] | undefined;
  readonly outside: Set<string>;
}

/** The forms one place's schema takes at one path of one example that holds it, for the
 *  daemon's verdicts at that path. */
interface PlaceRow {
  readonly place: string;
  readonly example: string;
  readonly path: string;
  /** A key an object's schema declares (which can be left out), another value, or a map's value. */
  readonly kind: "key" | "value" | "map";
  /** Whether a reader's schema (a tolerant walk) reads the place, rather than a producer's. */
  readonly reader: boolean;
  readonly taken: Set<Form>;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** One value of each kind a closed set holds that is not in the set: a string and a number
 *  beside its own, and the boolean it does not hold. */
function outsideValues(values: readonly unknown[]): readonly unknown[] {
  const outside: unknown[] = [];
  if (values.some((value) => typeof value === "string")) {
    let text = "outside the set";
    while (values.includes(text)) text += "+";
    outside.push(text);
  }
  const numbers = values.filter((value): value is number => typeof value === "number");
  if (numbers.length > 0) outside.push(Math.max(...numbers) + 1);
  if (values.some((value) => typeof value === "boolean")) {
    outside.push(...[true, false].filter((value) => !values.includes(value)));
  }
  return outside;
}

/** How a problem names a form a schema takes. */
function described(form: Form): string {
  switch (form) {
    case "absent":
    case "null":
      return `it ${form}`;
    case "any":
      return "any value";
    case "object":
    case "array":
    case "empty string":
      return `an ${form}`;
    case "number":
      return "a whole number above zero";
    case "zero":
      return "zero";
    case "negative":
      return "a negative whole number";
    case "past range":
      return "a whole number past the exact range";
    default:
      return `a ${form}`;
  }
}

/**
 * The walks of one test over its examples, and the problems they found. `walk` reads one value of
 * an example with the schema it is read by; once every value is walked, `staleExceptions`,
 * `closedSets` and `widenings`, and for a test that asks for them `unheldValues` and
 * `daemonAgreement`, add what only every walk together can show.
 */
export class ExampleWalk {
  /** Each problem the walks found, in the order they found it. */
  readonly problems: string[] = [];
  private readonly declared = new Map<string, Set<string>>();
  private readonly excused = new Map<string, Set<string>>();
  private readonly values = new Map<unknown, ValueSet>();
  private readonly forms = new Map<string, PlaceForms>();
  private readonly sets = new Map<string, PlaceSet>();
  private readonly rows = new Map<string, PlaceRow>();
  /** The schema each name stands for, as the walks met it, and each name met for two. */
  private readonly named = new Map<string, unknown>();
  private readonly namedTwice = new Set<string>();
  /** The objects a strict walk asked whether their schema takes an undeclared key, and the
   *  `loose` entries that answered for one. */
  private readonly strictJudged = new Set<string>();
  private readonly looseUsed = new Set<string>();
  /** The unions a member of which no value could be built for, each reported once. */
  private readonly unbuilt = new Set<string>();
  /** Whether the walk under way is a reader's (tolerant), for the rows it records. */
  private reading = false;

  constructor(private readonly rules: WalkRules) {}

  /**
   * Walks one value of an example at `path` (its place in the example) with the schema it is read
   * by, `name` naming the object there when `labels` does not. The value itself is held to the
   * forms the examples hold at that start (a schema made nullable, or given a fallback, at its
   * root fails as one a key down does). A union is read by the member that takes the value, as a
   * reader reads it, and each other member by a value built from its own schema; a null holds no
   * key to walk (the schema's own parse took it). A schema kind the inspection does not read is
   * an error, so no part of an example goes unread.
   */
  walk(
    schema: unknown,
    value: unknown,
    example: WalkedExample,
    path: string,
    name: string,
    mode: WalkMode
  ): void {
    const where = `${example.name} ${path}`;
    this.reading = mode.tolerant;
    const place = this.rules.labels.get(objectSchema(schema, where)) ?? name;
    const take = (probe: unknown) => takes(schema, probe);
    this.record(
      place,
      example,
      path,
      takenForms(take, [value]),
      this.builtForm(schema, take, where, place),
      [formOf(value)],
      "value"
    );
    this.visit(schema, value, example, path, name, mode, false);
  }

  /** Walks one value at `path`; `built` is a value built for a union member no example takes,
   *  whose forms and values count as no example's. */
  private visit(
    schema: unknown,
    value: unknown,
    example: WalkedExample,
    path: string,
    name: string,
    mode: WalkMode,
    built: boolean
  ): void {
    const where = `${example.name} ${path}`;
    const set = valueSet(schema, where);
    if (set !== undefined) {
      this.hold(set, name, built ? undefined : value);
      return;
    }
    if (value === null) return;
    const node = schemaNode(schema, where);
    switch (node.kind) {
      case "wrapper":
        this.visit(node.inner, value, example, path, name, mode, built);
        return;
      case "array":
        if (Array.isArray(value)) {
          this.inside(
            node.element,
            (probe) => takes(schema, [probe]),
            value,
            `${name}[]`,
            example,
            `${path}[]`,
            built,
            "value"
          );
          value.forEach((item, index) => {
            this.visit(node.element, item, example, `${path}[${index}]`, `${name}[]`, mode, built);
          });
        }
        return;
      case "record":
        if (isObject(value)) {
          for (const [key, entry] of Object.entries(value)) {
            this.inside(
              node.value,
              (probe) => takes(schema, { ...value, [key]: probe }),
              [entry],
              `${name}.*`,
              example,
              `${path}.${key}`,
              built,
              "map"
            );
            this.visit(node.value, entry, example, `${path}.${key}`, `${name}.*`, mode, built);
          }
        }
        return;
      case "union": {
        const member = node.options.find((option) => takes(option, value));
        if (member === undefined) {
          this.problems.push(`${example.name}'s ${path} is taken by no member of its union`);
          return;
        }
        for (const option of node.options) {
          if (option !== member) this.untaken(option, example, path, name, mode);
        }
        this.visit(member, value, example, path, name, mode, built);
        return;
      }
      case "intersection":
        // Each part would read the object's keys as its own, so neither part's strictness says
        // which keys the object may hold: the walk does not read one, and says so.
        throw new Error(
          `${example.name}'s ${path} is read by an intersection, which this walk does not read`
        );
      case "leaf":
        return;
      case "object":
        this.object(schema, node.shape, value, example, path, name, mode, built);
    }
  }

  private object(
    schema: unknown,
    shape: Readonly<Record<string, unknown>>,
    value: unknown,
    example: WalkedExample,
    path: string,
    name: string,
    mode: WalkMode,
    built: boolean
  ): void {
    if (!isObject(value)) return;
    const label = this.rules.labels.get(schema) ?? name;
    const known = this.named.get(label);
    if (known === undefined) {
      this.named.set(label, schema);
    } else if (known !== schema && !this.namedTwice.has(label)) {
      this.namedTwice.add(label);
      this.problems.push(
        `two schemas are read as the ${label}, so an exception for one would excuse the other: give each a name of its own`
      );
    }
    if (!mode.tolerant) this.strict(schema, shape, value, label);
    const declared = this.declared.get(label) ?? new Set<string>();
    this.declared.set(label, declared);
    const never = this.rules.never[label] ?? {};
    for (const [key, field] of Object.entries(shape)) {
      declared.add(key);
      const at = `${path}.${key}`;
      const written = never[key] === undefined;
      if (written) {
        this.form(schema, field, value, `${label}.${key}`, key, example, at, built);
      }
      if (key in value) {
        if (!written) {
          this.problems.push(
            `${example.name}'s ${at} is the ${label}'s "${key}", which NEVER says is never written`
          );
        }
        this.visit(field, value[key], example, at, `${label}.${key}`, mode, built);
        continue;
      }
      if (written) {
        const set = valueSet(field, `${example.name} ${at}`);
        if (set !== undefined) this.hold(set, `${label}.${key}`, undefined);
      }
      if (built) continue;
      if (example.absent[at] !== undefined) {
        const excused = this.excused.get(example.name) ?? new Set<string>();
        this.excused.set(example.name, excused);
        excused.add(at);
      } else if (written) {
        this.problems.push(
          `${example.name} holds no ${at}, which the ${label} declares and neither NEVER nor ${this.rules.absentTable} names`
        );
      }
    }
    if (!mode.tolerant) {
      for (const key of Object.keys(value)) {
        if (!(key in shape)) {
          this.problems.push(
            `${example.name}'s ${path} holds "${key}", which the ${label} does not declare`
          );
        }
      }
    }
  }

  /** Asks, once per object a strict walk reads, whether its schema takes the object with one key
   *  added that it does not declare: its writer writes no such key, so the schema must refuse
   *  one, unless `loose` names the object with the reason it takes one. */
  private strict(
    schema: unknown,
    shape: Readonly<Record<string, unknown>>,
    value: Readonly<Record<string, unknown>>,
    label: string
  ): void {
    if (this.strictJudged.has(label)) return;
    this.strictJudged.add(label);
    let fresh = "undeclared";
    while (fresh in shape || fresh in value) fresh += "_";
    const takesOne = takes(schema, { ...value, [fresh]: "x" });
    if (this.rules.loose[label] !== undefined) {
      this.looseUsed.add(label);
      if (!takesOne) {
        this.problems.push(
          `LOOSE names the ${label}, whose schema refuses a key it does not declare`
        );
      }
    } else if (takesOne) {
      this.problems.push(
        `the schema of the ${label} takes a key it does not declare ("${fresh}"), which its writer never writes, and LOOSE gives no reason it may`
      );
    }
  }

  /** Walks a union member no example's value took with a value built from its own schema, so
   *  its keys, values, closed sets and forms at every depth are held as a taken member's are; a
   *  member that is a set of values is recorded as that set, read at the union's place. */
  private untaken(
    option: unknown,
    example: WalkedExample,
    path: string,
    name: string,
    mode: WalkMode
  ): void {
    const where = `${example.name} ${path}`;
    const own = valueSet(option, where);
    if (own !== undefined) {
      this.hold(own, name, undefined);
      this.readSet(option, name, where, (outside) => takes(option, outside));
      return;
    }
    const value = this.build(option, where, name);
    if (value === undefined || !takes(option, value)) {
      if (!this.unbuilt.has(name)) {
        this.unbuilt.add(name);
        this.problems.push(
          `no value could be built of a member of the union at ${where} that no example takes, so its forms are held by nothing`
        );
      }
      return;
    }
    this.visit(option, value, example, path, name, mode, true);
  }

  /** A value of `schema` built from its own declarations: an object holding every key its name's
   *  NEVER entry does not name, a list and a map of one, a union's first member that takes one,
   *  and for a leaf its first value, or the first of `BUILT`'s values of its kinds it takes.
   *  Undefined when no such value is found. */
  private build(schema: unknown, where: string, name: string): unknown {
    const node = schemaNode(schema, where);
    switch (node.kind) {
      case "wrapper":
        return this.build(node.inner, where, name);
      case "leaf": {
        const candidates = node.closed
          ? node.values
          : node.json.flatMap((kind) => BUILT[kind] ?? []);
        return candidates.find((candidate) => takes(schema, candidate));
      }
      case "object": {
        const label = this.rules.labels.get(schema) ?? name;
        const never = this.rules.never[label] ?? {};
        const built: Record<string, unknown> = {};
        for (const [key, field] of Object.entries(node.shape)) {
          if (never[key] !== undefined) continue;
          const value = this.build(field, `${where}.${key}`, `${label}.${key}`);
          if (value === undefined) return undefined;
          built[key] = value;
        }
        return built;
      }
      case "array": {
        const element = this.build(node.element, `${where}[]`, `${name}[]`);
        return element === undefined ? undefined : [element];
      }
      case "record": {
        const entry = this.build(node.value, `${where}.*`, `${name}.*`);
        return entry === undefined ? undefined : { key: entry };
      }
      case "union":
        for (const option of node.options) {
          const value = this.build(option, where, name);
          if (value !== undefined && takes(option, value)) return value;
        }
        return undefined;
      case "intersection":
        return undefined;
    }
  }

  /** Records that `place` reads `set`, and holds `value` there when it is a value. */
  private hold(
    set: { readonly schema: unknown; readonly values: readonly unknown[] },
    place: string,
    value: unknown
  ): void {
    const entry = this.values.get(set.schema) ?? {
      values: set.values,
      places: new Set<string>(),
      held: new Set<string>(),
    };
    this.values.set(set.schema, entry);
    entry.places.add(place);
    if (value !== null && value !== undefined) entry.held.add(JSON.stringify(value));
  }

  /** The forms one place's record holds. */
  private placeForms(place: string): PlaceForms {
    const forms = this.forms.get(place) ?? { taken: new Set<Form>(), held: new Set<Form>() };
    this.forms.set(place, forms);
    return forms;
  }

  /** The form of a value built from the schema at a place (`build`) where zod's parse of the
   *  place (`take`) takes it: a probe of the form the schema declares there whatever refinements
   *  it states, which a fixed probe may miss (an object with required keys, a closed set's
   *  value). Undefined when no value is built or the parse refuses it. */
  private builtForm(
    schema: unknown,
    take: (probe: unknown) => boolean,
    where: string,
    name: string
  ): Form | undefined {
    const value = this.build(schema, where, name);
    return value !== undefined && take(value) ? formOf(value) : undefined;
  }

  /** Records the forms `place`'s schema takes - `taken`, of the fixed probes and those the held
   *  values add, and the form of a value built from its own schema - and those an example holds
   *  there; and, where `row` says the example holds the place (as an object's `key`, which can be
   *  left out, or as another `value`), the forms `taken` alone finds at that example's `path`,
   *  which are the probes the daemon's verdicts asked too. */
  private record(
    place: string,
    example: WalkedExample,
    path: string,
    taken: ReadonlySet<Form>,
    built: Form | undefined,
    held: readonly Form[],
    row: "key" | "value" | "map" | "none"
  ): void {
    const forms = this.placeForms(place);
    for (const form of taken) forms.taken.add(form);
    if (built !== undefined) forms.taken.add(built);
    for (const form of held) forms.held.add(form);
    if (row === "none") return;
    // Each list index is written `[]`, as the daemon's verdicts name a place.
    const at = path.replace(/\[[0-9]+\]/g, "[]");
    const key = `${place}\t${example.name}\t${at}`;
    const entry = this.rows.get(key) ?? {
      place,
      example: example.name,
      path: at,
      kind: row,
      reader: this.reading,
      taken: new Set<Form>(),
    };
    this.rows.set(key, entry);
    for (const form of taken) entry.taken.add(form);
  }

  /** Records the set of values `field` reads at `place`, and each value outside a closed set of
   *  it that `takesOutside` says the schema reading it there takes. */
  private readSet(
    field: unknown,
    place: string,
    where: string,
    takesOutside: (outside: unknown) => boolean
  ): void {
    const set = valueSet(field, where);
    const values = set?.closed ? set.values : undefined;
    const entry = this.sets.get(place) ?? { values, outside: new Set<string>() };
    this.sets.set(place, entry);
    for (const outside of values === undefined ? [] : outsideValues(values)) {
      if (takesOutside(outside)) entry.outside.add(JSON.stringify(outside));
    }
  }

  /** Records what one object shows of its key's forms - each form zod's parse of the object takes
   *  with a probe of it at the key, or with the key left out, and the form the object holds - and
   *  the set of values the key reads. */
  private form(
    schema: unknown,
    field: unknown,
    value: Readonly<Record<string, unknown>>,
    place: string,
    key: string,
    example: WalkedExample,
    at: string,
    built: boolean
  ): void {
    const present = key in value;
    const where = `${example.name} ${at}`;
    const take = (probe: unknown) => takes(schema, { ...value, [key]: probe });
    const taken = takenForms(take, present ? [value[key]] : []);
    const without = Object.fromEntries(Object.entries(value).filter(([held]) => held !== key));
    if (takes(schema, without)) taken.add("absent");
    const held: Form[] = built ? [] : [present ? formOf(value[key]) : "absent"];
    const own = this.builtForm(field, take, where, place);
    this.record(place, example, at, taken, own, held, present && !built ? "key" : "none");
    this.readSet(field, place, where, (outside) => present && take(outside));
  }

  /** Records what a list's elements or a map's value shows at `place`: each form zod's parse of
   *  the list or map takes with a probe there (`take`), each form the items hold, and the set of
   *  values `element` reads. */
  private inside(
    element: unknown,
    take: (probe: unknown) => boolean,
    items: readonly unknown[],
    place: string,
    example: WalkedExample,
    path: string,
    built: boolean,
    row: "value" | "map"
  ): void {
    const where = `${example.name} ${path}`;
    this.record(
      place,
      example,
      path,
      takenForms(take, items),
      this.builtForm(element, take, where, place),
      built ? [] : items.map(formOf),
      built ? "none" : row
    );
    this.readSet(element, place, where, (outside) => takes(element, outside));
  }

  /** Adds a problem for each exception no walk used: an example's `absent` entry for a key the
   *  example holds or a path no schema reaches, a `never` entry for a key no schema these examples
   *  are read by declares, and a `loose` entry for an object no strict walk read. */
  staleExceptions(examples: readonly WalkedExample[]): void {
    for (const example of examples) {
      const excused = this.excused.get(example.name);
      for (const at of Object.keys(example.absent)) {
        if (!excused?.has(at)) {
          this.problems.push(
            `${this.rules.absentTable} names ${example.name}'s ${at}, which the example holds or no schema it is read by declares there`
          );
        }
      }
    }
    for (const [label, keys] of Object.entries(this.rules.never)) {
      for (const key of Object.keys(keys)) {
        if (!this.declared.get(label)?.has(key)) {
          this.problems.push(
            `NEVER names the ${label}'s "${key}", which no schema these examples are read by declares`
          );
        }
      }
    }
    for (const label of Object.keys(this.rules.loose)) {
      if (!this.looseUsed.has(label)) {
        this.problems.push(`LOOSE names the ${label}, which no strict walk read`);
      }
    }
  }

  /**
   * Adds a problem for each value of a closed set that no example holds and `unheld` gives no
   * reason for, and for each `unheld` entry that names a value an example holds, or a value no
   * set read at that place declares. `unheld` is by place (`<object>.<key>`), then by the value's
   * JSON (`"system"`, `true`, `3`).
   */
  unheldValues(unheld: Readonly<Record<string, Readonly<Record<string, string>>>>): void {
    const used = new Set<string>();
    for (const set of this.values.values()) {
      const places = [...set.places].sort();
      for (const value of set.values) {
        const shown = JSON.stringify(value);
        const named = places.filter((place) => unheld[place]?.[shown] !== undefined);
        for (const place of named) used.add(`${place} ${shown}`);
        if (set.held.has(shown)) {
          for (const place of named) {
            this.problems.push(`UNHELD names ${shown} at ${place}, which an example holds`);
          }
        } else if (named.length === 0) {
          this.problems.push(
            `no example holds ${shown} at ${places.join(", ")}, a value its schema declares, and UNHELD gives no reason`
          );
        }
      }
    }
    for (const [place, values] of Object.entries(unheld)) {
      for (const shown of Object.keys(values)) {
        if (!used.has(`${place} ${shown}`)) {
          this.problems.push(`UNHELD names ${shown} at ${place}, which no schema declares there`);
        }
      }
    }
  }

  /**
   * Adds a problem for each place a walk read a closed set of values at that `closed` does not
   * name; and for each place `closed` names that no walk read, where the schema reads an open set
   * of values (a string, a closed set joined with an open one), or where the schema reading it
   * took a value outside its set. `closed` is by place (`<object>.<key>`, `<place>[]`, `<place>.*`).
   */
  closedSets(closed: readonly string[]): void {
    const listed = new Set(closed);
    for (const [place, set] of [...this.sets].sort(([a], [b]) => a.localeCompare(b))) {
      if (!listed.has(place)) {
        if (set.values !== undefined) {
          this.problems.push(
            `${place} reads the closed set ${JSON.stringify(set.values)}, which CLOSED does not name`
          );
        }
        continue;
      }
      if (set.values === undefined) {
        this.problems.push(`CLOSED names ${place}, where its schema reads an open set of values`);
        continue;
      }
      for (const outside of [...set.outside].sort()) {
        this.problems.push(
          `the schema of ${place} takes ${outside}, a value outside its closed set ${JSON.stringify(set.values)}`
        );
      }
    }
    for (const place of closed) {
      if (!this.sets.has(place)) this.problems.push(`CLOSED names ${place}, which no walk read`);
    }
  }

  /**
   * Adds a problem for each form - a key absent, a value null, a value of another form, any value
   * - that the schema of a place some walk read takes, that no example holds, and that `wider`
   * gives no reason for; and for each `wider` entry for a form its schema refuses, an example
   * holds, or no walk read. A schema that takes any value at a place is read as that one form,
   * `any`, there.
   */
  widenings(wider: Wider): void {
    const used = new Set<string>();
    for (const [place, forms] of [...this.forms].sort(([a], [b]) => a.localeCompare(b))) {
      const kinds: readonly Form[] = forms.taken.has("any") ? ["any"] : VALUE_FORMS;
      for (const form of ["absent", ...kinds] as const) {
        const taken = forms.taken.has(form);
        const held = forms.held.has(form);
        const reason = wider[place]?.[form];
        if (reason === undefined) {
          if (taken && !held) {
            this.problems.push(
              `the schema of ${place} takes ${described(form)}, a form no example holds, and WIDER gives no reason it may`
            );
          }
          continue;
        }
        used.add(`${place} ${form}`);
        if (held) this.problems.push(`WIDER names ${place} ${form}, which an example holds`);
        else if (!taken)
          this.problems.push(`WIDER names ${place} ${form}, which its schema refuses`);
      }
    }
    for (const [place, forms] of Object.entries(wider)) {
      for (const form of Object.keys(forms)) {
        if (!used.has(`${place} ${form}`)) {
          this.problems.push(`WIDER names ${place} ${form}, which no walk read`);
        }
      }
    }
  }

  /**
   * Holds each place an example holds - each value a walk read there, the walk's start among
   * them - to the daemon's verdicts at that example's path, by the rule of the two directions:
   *
   * 1. A producer's schema (one a strict walk reads) never takes a form the daemon's decoder
   *    refuses. No table excuses it: the schema is tightened, or the producer that writes the form
   *    is fixed.
   * 2. Where the decoder takes a form the schema refuses - a JSON null or a key left out read as an
   *    empty Go value, a value at a field the decoder only reads loosely - `differ` names the form
   *    with the reason, a rule stricter than the decoder that every producer keeps. A reader's
   *    schema (one a tolerant walk reads), which reads more than the daemon's issue events, gets
   *    the same treatment for the forms it takes and the decoder refuses: `readerWider` names each.
   *
   * A problem is added for each disagreement neither rule allows, for each table entry no
   * disagreement used, for each such place the verdicts hold no row for, and for each row of the
   * verdicts no walk compared. The verdicts' probes must be `FORM_PROBES`, so both sides asked the
   * same values. A place where the decoder takes every form a value holds is one it reads nothing
   * of (the daemon's own walk test holds which fields it reads), and is compared with nothing; a key
   * left out is compared at a key alone, since a list's element cannot be one; and a map's values
   * are compared where the schema of the event's type declares each key (`PublishedEventSchema`'s
   * payload is a map of any value the type's own schema reads). A table names a place as
   * `widenings` does, or as `<place> @ <path>` for one path of it; a `readerWider` entry `any`
   * answers for every form a reader that takes any value takes there.
   */
  daemonAgreement(daemon: DaemonVerdicts, differ: Wider, readerWider: Wider): void {
    const shown = (probes: Readonly<Record<string, readonly unknown[]>>) =>
      JSON.stringify(Object.entries(probes).sort(([a], [b]) => a.localeCompare(b)));
    if (shown(daemon.probes) !== shown(FORM_PROBES)) {
      this.problems.push(
        `the daemon's verdicts were made with the probes ${shown(daemon.probes)}, and the walk's are ${shown(FORM_PROBES)}`
      );
    }
    const compared = new Set<string>();
    const used = new Set<string>();
    const lookup = (table: Wider, name: string, row: PlaceRow, form: Form) =>
      [`${row.place} @ ${row.path}`, row.place].flatMap((key) => {
        if (table[key]?.[form] !== undefined) return [`${name} ${key} ${form}`];
        if (form !== "absent" && row.taken.has("any") && table[key]?.any !== undefined) {
          return [`${name} ${key} any`];
        }
        return [];
      })[0];
    for (const row of [...this.rows.values()].sort((a, b) =>
      `${a.example} ${a.path} ${a.place}`.localeCompare(`${b.example} ${b.path} ${b.place}`)
    )) {
      if (row.kind === "map") continue;
      const at = `${row.example} ${row.path}`;
      const verdict = daemon.verdicts[at];
      if (verdict === undefined) {
        this.problems.push(
          `the daemon's verdicts hold no row for ${row.example}'s ${row.path}, which the schema of ${row.place} reads`
        );
        continue;
      }
      compared.add(at);
      const decoded = new Set<string>(verdict);
      if (VALUE_FORMS.every((form) => decoded.has(form))) continue;
      for (const form of row.kind === "key" ? ["absent" as const, ...VALUE_FORMS] : VALUE_FORMS) {
        const schema = row.taken.has(form) || (form !== "absent" && row.taken.has("any"));
        if (schema === decoded.has(form)) continue;
        if (schema && !row.reader) {
          this.problems.push(
            `the schema of ${row.place} takes ${described(form)} at ${row.example}'s ${row.path}, which the daemon's decoder refuses: a producer's schema never takes what the daemon refuses`
          );
          continue;
        }
        const named = schema
          ? lookup(readerWider, "READER", row, form)
          : lookup(differ, "DAEMON", row, form);
        if (named !== undefined) {
          used.add(named);
          continue;
        }
        this.problems.push(
          schema
            ? `the reader's schema of ${row.place} takes ${described(form)} at ${row.example}'s ${row.path}, which the daemon's decoder refuses, and READER_WIDER gives no reason`
            : `the daemon's decoder takes ${described(form)} at ${row.example}'s ${row.path}, which the schema of ${row.place} refuses, and DAEMON gives no reason`
        );
      }
    }
    for (const [name, table] of [
      ["DAEMON", differ],
      ["READER", readerWider],
    ] as const) {
      for (const [place, forms] of Object.entries(table)) {
        for (const form of Object.keys(forms)) {
          if (!used.has(`${name} ${place} ${form}`)) {
            this.problems.push(
              `${name === "DAEMON" ? "DAEMON" : "READER_WIDER"} names ${place} ${form}, where the schema and the daemon's decoder do not differ that way`
            );
          }
        }
      }
    }
    for (const at of Object.keys(daemon.verdicts).sort()) {
      if (!compared.has(at)) {
        this.problems.push(
          `the daemon's verdicts hold ${at}, which no schema these examples are read by reads`
        );
      }
    }
  }
}
