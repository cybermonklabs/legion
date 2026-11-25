/**
 * The composer census (INFRA-1503), its trees: every mount of a field a draft store keeps the
 * reader's typing for (`DRAFT_FIELDS`), the props that name where its draft lives, and every way
 * its address changes or the mount ends, followed up the component tree to the route elements of
 * `app.tsx`. Each mount's own lines come first: the address props passed to it, and what in its
 * host draws, hides or keys it. Then each place a component above a mount is drawn, once: the
 * props it is handed that decide where a mount below lives or whether it is drawn, and what at
 * that place draws, hides or keys it. Each mount and each place is walked up from once
 * (`walk.ts`), and the forms (`forms.ts`) read the same walk.
 */
import * as ts from "typescript";

import {
  attribute,
  type Comp,
  calleeName,
  callsOf,
  componentFunction,
  declOf,
  dependsOn,
  drawnIn,
  lookup,
  modOf,
  moduleRefs,
  type Program,
  readIdentifiers,
  spreads,
  tagOf,
  text,
  where,
} from "./program";
import { type Item, type TreeKind, type Walked, walkUp } from "./walk";

/**
 * The fields the reader types a draft into that the draft stores keep (`draft-store.ts`), and how
 * the census finds their mounts:
 * - `MentionComposer`, every composer's field, whose drafts the two stores of each query client
 *   keep (`unsent-drafts.ts`), and `AskReplyComposer`, the reply form under a settled ask, whose
 *   drafts are the asks' replies (`reply-drafts.ts`): each element drawing one is a mount of the
 *   component it is drawn in, and its address props are the props of its own that its `homes`
 *   calls read. A `MentionComposer` mount is named by its send name.
 * - The answer field the ask body draws (`ask-body/AskBody.tsx`), the shared field
 *   (`MentionField`) under the prop that makes it the answer box (`askAnswer`), which writes the
 *   handle the Inbox finds the box by (`data-ask-answer`), whose drafts are the asks' answers
 *   (`answer-drafts.ts`), held by the form hook (`useAskAnswerForm`): each element under the prop
 *   is a mount of the component it is drawn in, named by the prop, and its address props are what
 *   is handed the hook where the hook's `homes` calls read it - by the host, where it calls the
 *   hook, else by each component that draws the host and hands it the hook's answer as its
 *   `formProp` (the card hands the body its `form`).
 *
 * A composer's mounts are followed up to the routes. The two ask fields' are followed up to the
 * component that holds each and not past it: the answer field's to the ask body (`ASK_BODY`),
 * the reply composer's to the ask card (`ASK_CARD`), whose thread holds it. Every host draws an
 * ask in one card, which draws one body, and their drafts are the ask's, by its id, in stores
 * that outlive every body and card (`answer-drafts.ts`, `reply-drafts.ts`). So a place above the
 * body or the card that hides, ends or keys it, or draws it for another ask, does to its field
 * what the body's or the card's own conditionals do to it, and the body or the card drawn for
 * that ask again holds the ask's drafts. A field drawn outside them is followed up to the
 * routes.
 */
interface FieldBase {
  readonly homes: ReadonlySet<string>;
  /** The component the census follows the field's mounts up to and not past. */
  readonly upTo?: string;
}

type DraftField =
  | (FieldBase & {
      readonly component: string;
      /** The prop a mount is named by among its host's. */
      readonly sendName?: string;
    })
  | (FieldBase & {
      readonly handle: string;
      readonly hook: string;
      /** The prop a host that calls `hook` nowhere takes the hook's answer by, from each
       *  component that draws it. */
      readonly formProp?: string;
    });

/** The ask body, which holds an ask's answer field. */
const ASK_BODY = "features/ask-body/AskBody.tsx#AskBody";

/** The ask card, whose thread holds an ask's reply composer. */
const ASK_CARD = "features/inbox/AskCard.tsx#AskCard";

const DRAFT_FIELDS: readonly DraftField[] = [
  {
    component: "features/conversation/MentionComposer.tsx#MentionComposer",
    homes: new Set(["useComposerSend", "mountDraft", "useKeptDrafts", "useDraftHomes"]),
    sendName: "mutationKey",
  },
  {
    component: "features/inbox/AskThread.tsx#AskReplyComposer",
    homes: new Set(["useReplyDraft"]),
    upTo: ASK_CARD,
  },
  {
    formProp: "form",
    handle: "askAnswer",
    homes: new Set(["useAnswerDraft"]),
    hook: "features/inbox/useAskAnswerForm.ts#useAskAnswerForm",
    upTo: ASK_BODY,
  },
];

/** A mount of a draft field (`DRAFT_FIELDS`), named by its host and the element it is. A mount
 *  drawn in a helper of its module is a mount of each component that draws the helper. */
export interface Mount {
  readonly element: ts.Node;
  readonly host: Comp;
  /** The mount as its host draws it, by its send name or its handle where it has one:
   *  `<MentionComposer mutationKey={…}>`, `<AskReplyComposer>`, `<MentionField askAnswer={true}>`. */
  readonly shown: string;
  /** The host and `shown`, which name the mount among all of them. */
  readonly name: string;
  /** The lines that say where its draft lives as its host draws it. */
  readonly address: readonly Item[];
  /** The component its field is followed up to and not past (`DraftField.upTo`). */
  readonly upTo: string | undefined;
  /** The walk up from it in its host, which the trees and the forms both read. */
  readonly walked: Walked;
}

/** One place a component is drawn by another, above at least one mount. */
export interface Edge {
  readonly child: Comp;
  readonly parent: Comp | undefined;
  readonly element: ts.Node;
  readonly lines: Item[];
  /** The mounts, by name, the place stands above. */
  readonly hosts: Set<string>;
  /** The walk up from the place in its parent, which the trees and the forms both read; none
   *  where no component draws the place (the app's root). */
  readonly walked: Walked | undefined;
  routed: boolean;
}

/** An item of the trees, with the mounts it stands above. */
export interface TreeItem {
  readonly kind: TreeKind;
  /** The component whose code holds the expression. */
  readonly host: string;
  /** The place the item is about: a mount by its name (`Mount.name`), or the component drawn at
   *  a place above mounts. */
  readonly mount: string;
  readonly expression: string;
  readonly at: string;
  readonly mounts: readonly string[];
}

export interface Trees {
  readonly program: Program;
  readonly mounts: readonly Mount[];
  readonly edges: readonly Edge[];
  readonly items: readonly TreeItem[];
  /** What the census met and cannot read, each with where it is: a field's mount it places in no
   *  component, and a component element, a helper or a slot between a mount and the routes whose
   *  drawing it cannot follow. Each is a mount or an item the census may be missing. */
  readonly unread: readonly string[];
}

/** The props `unit` (a field's component, or the hook that holds a field's draft) reads where its
 *  draft lives, through the `homes` calls. */
function readAddressProps(
  unit: Pick<Comp, "cls" | "fn" | "mod">,
  homes: ReadonlySet<string>
): Set<string> {
  const props = new Set<string>();
  /** The props an argument reads, directly or through a local computed from them; a hook's or
   *  a callback's result is the field's own state, not where the host put it. */
  const walk = (node: ts.Node, seen: Set<ts.Node>) => {
    for (const ident of readIdentifiers(node, true)) {
      const found = lookup(ident, unit);
      if (found.kind === "prop") props.add(found.prop);
      if (found.kind !== "local" || seen.has(found.decl)) continue;
      seen.add(found.decl);
      const init = found.decl.initializer;
      if (init === undefined || ts.isArrowFunction(init) || ts.isFunctionExpression(init)) {
        continue;
      }
      const hook = calleeName(init);
      if (hook !== undefined && /^use[A-Z]/.test(hook)) continue;
      walk(init, seen);
    }
  };
  const visit = (node: ts.Node) => {
    if (
      ts.isCallExpression(node) &&
      ts.isIdentifier(node.expression) &&
      homes.has(node.expression.text)
    ) {
      for (const argument of node.arguments) walk(argument, new Set());
    }
    ts.forEachChild(node, visit);
  };
  if (unit.fn.body !== undefined) visit(unit.fn.body);
  return props;
}

/** The props of its component an item reads, which that component's users decide. An item read
 *  inside a slot's receiver reads the receiver's props, not this component's. */
function propsRead(item: Item): Set<string> {
  if (item.inside) return new Set();
  return new Set(item.deps.filter((dep) => dep.kind === "prop").map((dep) => dep.name));
}

function addLine(lines: Item[], line: Item): void {
  const same = (other: Item) =>
    other.kind === line.kind && other.at === line.at && other.expression === line.expression;
  if (!lines.some(same)) lines.push(line);
}

function isTreeKind(kind: Item["kind"]): kind is TreeKind {
  return kind !== "slot" && kind !== "helper" && kind !== "element";
}

/** A mount as the census finds it, before it is named, with the prop it is named by among its
 *  host's. */
interface Placed extends Omit<Mount, "name" | "shown"> {
  readonly naming: string | undefined;
}

/** A mount's name: its host and the element it is, with the prop it is named by (a composer's
 *  send name, the answer field's handle), which no two mounts of one host share; a mount drawn
 *  under none is named by its place among its host's. */
function mountNames(mounts: readonly Placed[]): Mount[] {
  const taken = new Map<string, number>();
  return mounts.map(({ naming, ...mount }) => {
    const { element, host } = mount;
    const send = naming === undefined ? undefined : attribute(element, naming);
    const base = `<${text(tagOf(element) as ts.Node)}${send === undefined ? "" : ` ${text(send)}`}>`;
    const count = (taken.get(`${host.name} ${base}`) ?? 0) + 1;
    taken.set(`${host.name} ${base}`, count);
    const shown = count === 1 ? base : `${base} #${count}`;
    return { ...mount, name: `${host.name} ${shown}`, shown };
  });
}

/** The lines of the props `props` a mount's element is passed, and of its spreads: where its
 *  draft lives, as its host draws it. A value the host computes from nothing of its own never
 *  moves the address, so it is no line. */
function passedLines(element: ts.Node, host: Comp, props: readonly string[]): Item[] {
  const lines: Item[] = [];
  for (const prop of props) {
    const passed = attribute(element, prop);
    if (passed?.initializer === undefined) continue;
    const deps = dependsOn(passed.initializer, host);
    if (deps.length === 0) continue;
    lines.push({
      at: where(passed),
      deps,
      expression: text(passed),
      inside: false,
      kind: prop === "key" ? "key" : "address prop",
      unit: host,
    });
  }
  for (const spread of spreads(element)) {
    lines.push({
      at: where(spread),
      deps: dependsOn(spread.expression, host),
      expression: text(spread),
      inside: false,
      kind: "address prop",
      unit: host,
    });
  }
  return lines;
}

/** The lines of what `host` hands the hook `hook` where the hook reads where a field's draft
 *  lives (`props`): each such property of the object `host` calls it with, each spread into that
 *  object, or the whole argument where it is no object literal. */
function handedLines(host: Comp, hook: string, props: ReadonlySet<string>): Item[] {
  const line = (node: ts.Node, shown: string): Item => ({
    at: where(node),
    deps: dependsOn(node, host),
    expression: `${hook}(${shown})`,
    inside: false,
    kind: "address prop",
    unit: host,
  });
  const lines: Item[] = [];
  for (const call of callsOf(hook, host)) {
    const argument = call.arguments[0];
    if (argument === undefined) continue;
    if (!ts.isObjectLiteralExpression(argument)) {
      lines.push(line(argument, text(argument)));
      continue;
    }
    for (const property of argument.properties) {
      const name = ts.isSpreadAssignment(property) ? undefined : property.name?.getText();
      if (name === undefined || props.has(name)) {
        lines.push(line(property, `{ ${text(property)} }`));
      }
    }
  }
  return lines;
}

/** The lines of what is handed the hook `hook` where it reads where a field's draft lives
 *  (`props`), for a mount drawn in `host`: what `host` hands it, or, where `host` calls it
 *  nowhere, what each component hands it that draws `host` with the hook's answer as its
 *  `formProp`. */
function formLines(
  program: Program,
  host: Comp,
  hook: string,
  props: ReadonlySet<string>,
  formProp: string | undefined
): Item[] {
  const own = handedLines(host, hook, props);
  if (own.length > 0 || formProp === undefined) return own;
  const callers = new Map<string, Comp>();
  for (const { element } of program.usesById.get(host.id) ?? []) {
    if (attribute(element, formProp) === undefined) continue;
    for (const caller of drawnIn(program, element)) callers.set(caller.id, caller);
  }
  return [...callers.values()].flatMap((caller) => handedLines(caller, hook, props));
}

/** Whether a node sits outside every function and class: the app's root, as `main.tsx`
 *  renders it. */
function atTopLevel(node: ts.Node): boolean {
  for (let current = node.parent; !ts.isSourceFile(current); current = current.parent) {
    if (ts.isFunctionLike(current) || ts.isClassLike(current)) return false;
  }
  return true;
}

/** Every element of the program under `handle`, a plain element's attribute that marks a field. */
function handled(program: Program, handle: string): ts.Node[] {
  const found: ts.Node[] = [];
  for (const mod of program.mods.values()) {
    const visit = (node: ts.Node) => {
      if (
        (ts.isJsxSelfClosingElement(node) || ts.isJsxElement(node)) &&
        attribute(node, handle) !== undefined
      ) {
        found.push(node);
      }
      ts.forEachChild(node, visit);
    };
    visit(mod.sf);
  }
  return found;
}

/** Reads every mount of a draft field (`DRAFT_FIELDS`) and the tree above it, and what it cannot
 *  read. */
export function readTrees(program: Program): Trees {
  /** The places a component is drawn, by its element and the component drawing it. */
  const edges = new Map<ts.Node, Map<string, Edge>>();
  const wanted = new Map<string, Set<string>>();
  const hostsOf = new Map<string, Set<string>>();
  const unread = new Set<string>();
  /** Records what a walk cannot read, each line as being above `above`. */
  const unreadAbove = (walked: Walked, above: string) => {
    for (const line of walked.unread) unread.add(`${line}, above ${above}`);
  };

  /** Climbs from `startId`, whose `props` decide where a mount below it lives or whether it is
   *  drawn, through every component that draws it, to the routes and the app's root, or to
   *  `upTo` and not past it. */
  const climb = (
    startId: string,
    props: ReadonlySet<string>,
    host: string,
    upTo: string | undefined
  ) => {
    const queue: { id: string; props: Set<string> }[] = [{ id: startId, props: new Set(props) }];
    for (let step = queue.shift(); step !== undefined; step = queue.shift()) {
      if (step.id === upTo) continue;
      const labels = hostsOf.get(step.id) ?? new Set<string>();
      const newHost = !labels.has(host);
      labels.add(host);
      hostsOf.set(step.id, labels);
      const known = wanted.get(step.id) ?? new Set<string>();
      const fresh = [...step.props].filter((prop) => !known.has(prop)).sort();
      for (const prop of fresh) known.add(prop);
      wanted.set(step.id, known);
      if (fresh.length === 0 && !newHost) continue;
      const child = program.comps.get(step.id) as Comp;
      for (const use of program.usesById.get(step.id) ?? []) {
        const parents: (Comp | undefined)[] = drawnIn(program, use.element);
        if (parents.length === 0) {
          if (!atTopLevel(use.element)) {
            unread.add(
              `${where(use.element)}: <${child.name}>, drawn by no component the census reads, above ${host}`
            );
          }
          parents.push(undefined);
        }
        const places = edges.get(use.element) ?? new Map<string, Edge>();
        edges.set(use.element, places);
        for (const parent of parents) {
          const reached = places.get(parent?.id ?? "");
          const edge: Edge = reached ?? {
            child,
            element: use.element,
            hosts: new Set(),
            lines: [],
            parent,
            routed: false,
            walked: parent === undefined ? undefined : walkUp(program, use.element, parent),
          };
          if (reached === undefined) places.set(parent?.id ?? "", edge);
          edge.hosts.add(host);
          const { walked } = edge;
          if (parent === undefined || walked === undefined) continue;
          const next = new Set<string>();
          // The place's own lines, read when a climb first reaches it: its key, its spreads, and
          // what its walk up passes.
          if (reached === undefined) {
            const items: Item[] = [];
            const key = attribute(use.element, "key");
            if (key?.initializer !== undefined) {
              items.push({
                at: where(key),
                deps: dependsOn(key.initializer, parent),
                expression: `<${child.name} ${text(key)}>`,
                inside: false,
                kind: "key",
                unit: parent,
              });
            }
            for (const spread of spreads(use.element)) {
              items.push({
                at: where(spread),
                deps: dependsOn(spread.expression, parent),
                expression: `<${child.name} ${text(spread)}>`,
                inside: false,
                kind: "address prop",
                unit: parent,
              });
            }
            unreadAbove(walked, `<${child.name}> in ${parent.name}`);
            for (const item of [...items, ...walked.items]) {
              if (item.route) edge.routed = true;
              for (const prop of propsRead(item)) next.add(prop);
              addLine(edge.lines, item);
            }
          }
          for (const prop of fresh) {
            const passed = attribute(use.element, prop);
            if (passed === undefined) continue;
            const line: Item = {
              at: where(passed),
              deps: passed.initializer === undefined ? [] : dependsOn(passed.initializer, parent),
              expression: text(passed),
              inside: false,
              kind: "address prop",
              unit: parent,
            };
            for (const name of propsRead(line)) next.add(name);
            addLine(edge.lines, line);
          }
          if (!edge.routed) queue.push({ id: parent.id, props: next });
        }
      }
    }
  };

  // Each element drawing a field's component, or under a field's handle, is a mount of each
  // component it is drawn in; one drawn in none the census reads, a field component read as a
  // value rather than drawn, or a field whose host keeps its draft nowhere the census reads, is a
  // mount it cannot place.
  const placedMounts: Placed[] = [];
  for (const field of DRAFT_FIELDS) {
    if ("component" in field) {
      const comp = program.comps.get(field.component);
      if (comp === undefined) throw new Error(`${field.component} not found`);
      const props = [...readAddressProps(comp, field.homes), "key"].sort();
      for (const { element } of program.usesById.get(comp.id) ?? []) {
        const hosts = drawnIn(program, element);
        if (hosts.length === 0) {
          unread.add(`${where(element)}: <${comp.name}>, drawn by no component the census reads`);
        }
        for (const host of hosts) {
          const address = passedLines(element, host, props);
          placedMounts.push({
            address,
            element,
            host,
            naming: field.sendName,
            upTo: field.upTo,
            walked: walkUp(program, element, host),
          });
        }
      }
      for (const ref of moduleRefs(program, comp.id)) {
        unread.add(`${where(ref)}: ${comp.name}, read as a value, not drawn as an element`);
      }
      continue;
    }
    const decl = declOf(program, field.hook);
    const fn = decl === undefined ? undefined : componentFunction(decl);
    if (decl === undefined || fn === undefined) throw new Error(`${field.hook} not found`);
    const hook = field.hook.slice(field.hook.lastIndexOf("#") + 1);
    const props = readAddressProps({ fn, mod: modOf(program, decl) }, field.homes);
    for (const element of handled(program, field.handle)) {
      const shown = `<${text(tagOf(element) as ts.Node)} ${field.handle}>`;
      const hosts = drawnIn(program, element);
      if (hosts.length === 0) {
        unread.add(`${where(element)}: ${shown}, drawn by no component the census reads`);
      }
      for (const host of hosts) {
        const address = [
          ...passedLines(element, host, ["key"]),
          ...formLines(program, host, hook, props, field.formProp),
        ];
        if (address.every((line) => line.kind === "key")) {
          unread.add(
            `${where(element)}: ${shown} in ${host.name}, which hands ${hook} nowhere the census can see`
          );
        }
        placedMounts.push({
          address,
          element,
          host,
          naming: field.handle,
          upTo: field.upTo,
          walked: walkUp(program, element, host),
        });
      }
    }
  }
  const mounts = mountNames(placedMounts);
  const items: TreeItem[] = [];
  for (const mount of mounts) {
    unreadAbove(mount.walked, mount.name);
    const lines: Item[] = [...mount.address];
    const carried = new Set<string>();
    for (const line of lines) {
      for (const name of propsRead(line)) carried.add(name);
    }
    for (const item of mount.walked.items) {
      for (const name of propsRead(item)) carried.add(name);
      addLine(lines, item);
    }
    for (const line of lines) {
      if (!isTreeKind(line.kind)) continue;
      items.push({
        at: line.at,
        expression: line.expression,
        host: line.unit.name,
        kind: line.kind,
        mount: mount.name,
        mounts: [mount.name],
      });
    }
    climb(mount.host.id, carried, mount.name, mount.upTo);
  }
  // Read once every mount has climbed, since a later mount's climb adds itself to a place an
  // earlier one already reached.
  const allEdges = [...edges.values()].flatMap((places) => [...places.values()]);
  for (const edge of allEdges) {
    for (const line of edge.lines) {
      if (!isTreeKind(line.kind)) continue;
      items.push({
        at: line.at,
        expression: line.expression,
        host: line.unit.name,
        kind: line.kind,
        mount: edge.child.name,
        mounts: [...edge.hosts],
      });
    }
  }
  return { edges: allEdges, items, mounts, program, unread: [...unread] };
}
