/**
 * The composer census (INFRA-1503), its trees: every `MentionComposer` mount, the props that name
 * where its draft lives, and every way its address changes or the mount ends, followed up the
 * component tree to the route elements of `app.tsx`. Each mount's own lines come first: the
 * address props passed to it, and what in its host draws, hides or keys it. Then each place a
 * component above a mount is drawn, once: the props it is handed that decide where a mount below
 * lives or whether it is drawn, and what at that place draws, hides or keys it.
 */
import * as ts from "typescript";

import {
  attribute,
  bindingNamed,
  type Comp,
  calleeName,
  type Dep,
  dependsOn,
  drawnIn,
  helperId,
  helperName,
  isModuleHelper,
  isThis,
  lookup,
  modOf,
  moduleRefs,
  type Program,
  readIdentifiers,
  referencesOf,
  resolveLocal,
  spreads,
  tagComp,
  tagOf,
  text,
  where,
} from "./program";

/** The calls of `MentionComposer` that read where its draft lives. */
const DRAFT_HOME_CALLS = new Set([
  "useComposerSend",
  "mountDraft",
  "useKeptDrafts",
  "useDraftHomes",
]);

export const CONDITIONAL_OPERATORS = new Set([
  ts.SyntaxKind.AmpersandAmpersandToken,
  ts.SyntaxKind.BarBarToken,
  ts.SyntaxKind.QuestionQuestionToken,
]);

/** The kinds of a tree's item: what moves an address or ends or hides a mount. */
export type TreeKind = "address prop" | "conditional" | "key" | "route" | "early return";

/** One thing a walk up from a mount passes. A slot, a helper or an element is only the way the
 *  walk went; the other kinds are the tree's items. */
export interface Item {
  readonly kind: TreeKind | "slot" | "helper" | "element";
  readonly at: string;
  readonly expression: string;
  readonly deps: readonly Dep[];
  /** The component whose code holds the item. */
  readonly unit: Comp;
  /** Whether the walk reached it inside a slot's receiver, whose props its callers decide. */
  readonly inside: boolean;
  /** The element the walk ends at, where it reached a route. */
  readonly route?: boolean;
  /** The expression the item turns on, which the forms read for the layout. */
  readonly node?: ts.Node;
}

/** A `MentionComposer` mount, named by its host and its send name. A mount drawn in a helper of
 *  its module is a mount of each component that draws the helper. */
export interface Mount {
  readonly element: ts.Node;
  readonly host: Comp;
  /** The mount as its host draws it, by its send name: `<MentionComposer mutationKey={…}>`. */
  readonly shown: string;
  /** The host and `shown`, which name the mount among all of them. */
  readonly name: string;
}

/** One place a component is drawn by another, above at least one mount. */
export interface Edge {
  readonly child: Comp;
  readonly parent: Comp | undefined;
  readonly element: ts.Node;
  readonly lines: Item[];
  /** The mounts, by name, the place stands above. */
  readonly hosts: Set<string>;
  routed: boolean;
  structural: boolean;
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
  readonly composer: Comp;
  readonly mounts: readonly Mount[];
  readonly edges: readonly Edge[];
  readonly items: readonly TreeItem[];
  /** What the census met and cannot read, each with where it is: a `MentionComposer` it places
   *  in no component, and a component element, a helper or a slot between a mount and the routes
   *  whose drawing it cannot follow. Each is a mount or an item the census may be missing. */
  readonly unread: readonly string[];
}

/** How a walk up runs. The forms (`forms.ts`) walk with `receivers` set: each element passed is
 *  an item then, and each component a slot is followed into is recorded there. The trees walk
 *  with `unread` set, where the walk records what it cannot read; a forms walk retraces one of
 *  theirs and leaves it out. */
export interface Walk {
  readonly program: Program;
  readonly receivers?: Set<string>;
  readonly unread?: string[];
}

export function containsReturn(node: ts.Node): boolean {
  let found = false;
  const visit = (current: ts.Node) => {
    if (found || (current !== node && ts.isFunctionLike(current))) return;
    if (ts.isReturnStatement(current)) {
      found = true;
      return;
    }
    ts.forEachChild(current, visit);
  };
  visit(node);
  return found;
}

/** The props `MentionComposer` reads where its draft lives, through the draft-home calls. */
function readAddressProps(composer: Comp): Set<string> {
  const props = new Set<string>();
  /** The props an argument reads, directly or through a local computed from them; a hook's or
   *  a callback's result is the composer's own state, not where the host put it. */
  const walk = (node: ts.Node, seen: Set<ts.Node>) => {
    for (const ident of readIdentifiers(node, true)) {
      const found = lookup(ident, composer);
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
      DRAFT_HOME_CALLS.has(node.expression.text)
    ) {
      for (const argument of node.arguments) walk(argument, new Set());
    }
    ts.forEachChild(node, visit);
  };
  if (composer.fn.body !== undefined) visit(composer.fn.body);
  // React's own: a new key is a new mount.
  props.add("key");
  return props;
}

export function walkUp(
  walk: Walk,
  start: ts.Node,
  comp: Comp,
  items: Item[],
  inside: boolean,
  seen: Set<ts.Node>,
  depth = 0,
  initialPath: readonly string[] = []
): void {
  if (seen.has(start)) return;
  seen.add(start);
  if (depth > 6) {
    unreadAt(walk, start, "sits deeper than the census reads");
    return;
  }
  const push = (item: Omit<Item, "inside" | "unit">) => items.push({ ...item, inside, unit: comp });
  // The properties of an object literal the JSX sits in, outermost first, so a slot handed over
  // as `anchoredThread={{ children: <X /> }}` is followed as `anchoredThread.children`.
  let path = [...initialPath];
  let child: ts.Node = start;
  let current: ts.Node | undefined = start.parent;
  while (current !== undefined && child !== comp.fn) {
    if (ts.isPropertyAssignment(current) && child === current.initializer) {
      path.unshift(current.name.getText());
    }
    if (ts.isJsxElement(current) && child !== current.openingElement) {
      // The forms read each element the mount sits in for what the layout changes about it: a
      // class, a role, a dialog's ref.
      if (walk.receivers !== undefined) {
        push({
          at: where(current.openingElement),
          deps: [],
          expression: `<${text(current.openingElement.tagName)}>`,
          kind: "element",
          node: current.openingElement,
        });
      }
      const key = attribute(current, "key");
      if (key?.initializer !== undefined) {
        push({
          at: where(key),
          deps: dependsOn(key.initializer, comp),
          expression: `<${text(current.openingElement.tagName)} ${text(key)}>`,
          kind: "key",
          node: key.initializer,
        });
      }
      const hidden = attribute(current, "hidden");
      if (hidden !== undefined) {
        push({
          at: where(hidden),
          deps: hidden.initializer === undefined ? [] : dependsOn(hidden.initializer, comp),
          expression: `<${text(current.openingElement.tagName)} ${text(hidden)}>`,
          kind: "conditional",
          node: hidden.initializer,
        });
      }
      const tag = current.openingElement.tagName;
      if (!isPlainTag(tag)) {
        const why = followElement(walk, comp, current, "children", [], items, seen, depth + 1);
        if (why !== undefined) unreadAt(walk, current.openingElement, `<${text(tag)}> ${why}`);
      }
    } else if (ts.isConditionalExpression(current) && child !== current.condition) {
      push({
        at: where(current.condition),
        deps: dependsOn(current.condition, comp),
        expression: `${child === current.whenTrue ? "" : "!"}(${text(current.condition)}) ? …`,
        kind: "conditional",
        node: current.condition,
      });
    } else if (
      ts.isBinaryExpression(current) &&
      CONDITIONAL_OPERATORS.has(current.operatorToken.kind) &&
      child === current.right
    ) {
      push({
        at: where(current.left),
        deps: dependsOn(current.left, comp),
        expression: `${text(current.left)} ${current.operatorToken.getText()} …`,
        kind: "conditional",
        node: current.left,
      });
    } else if (ts.isIfStatement(current) && child !== current.expression) {
      const arm = child === current.thenStatement ? "if" : "else of if";
      push({
        at: where(current.expression),
        deps: dependsOn(current.expression, comp),
        expression: `${arm} (${text(current.expression)})`,
        kind: "conditional",
        node: current.expression,
      });
    } else if (ts.isCaseClause(current)) {
      const switched = current.parent.parent;
      push({
        at: where(current.expression),
        deps: dependsOn(switched.expression, comp),
        expression: `switch (${text(switched.expression)}) case ${text(current.expression)}`,
        kind: "conditional",
        node: switched.expression,
      });
    } else if (ts.isReturnStatement(current)) {
      earlyReturns(current, comp, push);
    } else if (ts.isJsxAttribute(current)) {
      const element = current.parent.parent;
      const tag = text(tagOf(element) as ts.Node);
      const name = current.name.getText();
      if (name === "element" && tag === "Route") {
        const routePath = attribute(element, "path");
        push({
          at: where(element),
          deps: [],
          expression: `<Route ${routePath === undefined ? "(no path)" : text(routePath)}>`,
          kind: "route",
          route: true,
        });
        return;
      }
      push({
        at: where(current),
        deps: [],
        expression: `passed as <${tag} ${name}={…}>`,
        kind: "slot",
      });
      // An attribute of a plain element (an event handler, a value) draws nothing.
      if (isPlainTag(tagOf(element) as ts.JsxTagNameExpression)) return;
      const why = followElement(walk, comp, element, name, path, items, seen, depth + 1);
      if (why !== undefined) unreadAt(walk, current, `<${tag} ${name}={…}> ${why}`);
      path = [];
      const key = attribute(element, "key");
      if (key?.initializer !== undefined) {
        push({
          at: where(key),
          deps: dependsOn(key.initializer, comp),
          expression: `<${tag} ${text(key)}>`,
          kind: "key",
          node: key.initializer,
        });
      }
      child = ts.isJsxOpeningElement(element) ? element.parent : element;
      current = child.parent;
      continue;
    } else if (isModuleHelper(current) && current !== comp.fn) {
      // A helper of its module, outside every component: follow each place a component it is
      // drawn in reads it, `comp` here, directly or through another helper of a module.
      const name = helperName(current);
      const refs = moduleRefs(walk.program, helperId(walk.program, current)).filter(
        (ref) => placed(ref, []) !== undefined && drawnIn(walk.program, ref).includes(comp)
      );
      push({ at: where(current), deps: [], expression: name, kind: "helper" });
      if (refs.length === 0) {
        unreadAt(
          walk,
          current,
          `${name}, a helper of its module, is drawn in ${comp.name} nowhere the census can see`
        );
      }
      for (const ref of refs) walkUp(walk, ref, comp, items, inside, seen, depth + 1);
      return;
    } else if (ts.isVariableDeclaration(current) && ts.isIdentifier(current.name)) {
      // A local holding the JSX, or a helper returning it: follow each place it is used.
      const refs = referencesOf(current.name.text, current, comp).filter(
        (ref) => placed(ref, []) !== undefined
      );
      push({ at: where(current), deps: [], expression: current.name.text, kind: "helper" });
      if (refs.length === 0) {
        unreadAt(
          walk,
          current,
          `${current.name.text}, a local of ${comp.name}, is drawn nowhere the census can see`
        );
      }
      for (const ref of refs) walkUp(walk, ref, comp, items, inside, seen, depth + 1);
      return;
    } else if (ts.isFunctionDeclaration(current) && current !== comp.fn) {
      const name = current.name?.text ?? "(anonymous)";
      const refs = referencesOf(name, current, comp).filter((ref) => placed(ref, []) !== undefined);
      push({ at: where(current), deps: [], expression: name, kind: "helper" });
      if (refs.length === 0) {
        unreadAt(
          walk,
          current,
          `${name}, a helper of ${comp.name}, is drawn nowhere the census can see`
        );
      }
      for (const ref of refs) walkUp(walk, ref, comp, items, inside, seen, depth + 1);
      return;
    } else if (
      (ts.isArrowFunction(current) || ts.isFunctionExpression(current)) &&
      current !== comp.fn &&
      ts.isCallExpression(current.parent) &&
      current.parent.arguments.includes(current as ts.Expression)
    ) {
      // Drawn once per item of a list: an item that leaves the list ends its mount.
      const call = current.parent;
      push({
        at: where(call),
        deps: dependsOn(call.expression, comp),
        expression: `drawn by ${text(call.expression)}(…)`,
        kind: "conditional",
        node: call.expression,
      });
    } else if (
      ts.isCallExpression(current) &&
      ts.isIdentifier(current.expression) &&
      current.arguments.includes(child as ts.Expression)
    ) {
      // Handed to a helper of the component's own (`page(list, row)`): follow its parameter to
      // where the helper draws it, and from there to where the helper is called.
      const callee = lookup(current.expression, comp);
      const init = callee.kind === "local" ? callee.decl.initializer : undefined;
      const fn =
        init !== undefined && (ts.isArrowFunction(init) || ts.isFunctionExpression(init))
          ? init
          : callee.kind === "function"
            ? (callee.node as ts.FunctionDeclaration)
            : undefined;
      const index = current.arguments.indexOf(child as ts.Expression);
      const param = fn?.parameters[index];
      if (fn !== undefined && param !== undefined && ts.isIdentifier(param.name)) {
        const uses: ts.Identifier[] = [];
        const visit = (node: ts.Node) => {
          if (ts.isIdentifier(node) && node !== param.name) {
            const found = lookup(node, comp);
            if (found.kind === "param" && found.fn === fn && found.index === index) {
              if (placed(node, []) !== undefined) uses.push(node);
            }
          }
          ts.forEachChild(node, visit);
        };
        if (fn.body !== undefined) visit(fn.body);
        push({ at: where(current), deps: [], expression: param.name.text, kind: "helper" });
        if (uses.length === 0) {
          unreadAt(
            walk,
            current,
            `${current.expression.text}, a helper of ${comp.name}, draws its ${param.name.text} nowhere the census can see`
          );
        }
        for (const use of uses) walkUp(walk, use, comp, items, inside, seen, depth + 1);
        return;
      }
    }
    child = current;
    current = current.parent;
  }
}

/** The `if (…) return` statements before `statement` in its blocks, each of which ends the mount
 *  while its condition holds. */
function earlyReturns(
  statement: ts.Node,
  comp: Comp,
  push: (item: Omit<Item, "inside" | "unit">) => void
): void {
  let child: ts.Node = statement;
  let current: ts.Node | undefined = statement.parent;
  while (current !== undefined && child !== comp.fn) {
    if (ts.isBlock(current)) {
      for (const sibling of current.statements) {
        if (sibling === child) break;
        if (ts.isIfStatement(sibling) && containsReturn(sibling.thenStatement)) {
          push({
            at: where(sibling.expression),
            deps: dependsOn(sibling.expression, comp),
            expression: `if (${text(sibling.expression)}) return …`,
            kind: "early return",
            node: sibling.expression,
          });
        }
      }
    }
    if (ts.isFunctionLike(current)) break;
    child = current;
    current = current.parent;
  }
}

/** Whether a use of a slot prop places the slot itself: drawn as a child, passed on whole to
 *  another element, portalled, or returned. Answers the parts of `subpath` still to reach, or
 *  undefined where the use only reads something out of the prop (a field, a test). */
function placed(use: ts.Node, subpath: readonly string[]): string[] | undefined {
  let node: ts.Node = use;
  const rest = [...subpath];
  for (;;) {
    const parent: ts.Node = node.parent;
    if (
      ts.isCallExpression(parent) &&
      (parent.expression === node ||
        (ts.isPropertyAccessExpression(parent.expression) &&
          /^(map|flatMap)$/.test(parent.expression.name.text) &&
          parent.arguments.includes(node as ts.Expression)))
    ) {
      // A helper called, or handed to `map`, draws what it returns where the call's value goes.
      node = parent;
      continue;
    }
    if (ts.isPropertyAccessExpression(parent) && parent.expression === node) {
      if (rest.length === 0 || parent.name.text !== rest[0]) return undefined;
      rest.shift();
    } else if (
      ts.isConditionalExpression(parent)
        ? parent.condition === node
        : ts.isBinaryExpression(parent)
          ? !CONDITIONAL_OPERATORS.has(parent.operatorToken.kind) || parent.right !== node
          : !(
              ts.isParenthesizedExpression(parent) ||
              ts.isNonNullExpression(parent) ||
              ts.isAsExpression(parent) ||
              ts.isArrayLiteralExpression(parent) ||
              ts.isSpreadElement(parent)
            )
    ) {
      if (ts.isJsxExpression(parent)) {
        return rest.length === 0 || ts.isJsxAttribute(parent.parent) ? rest : undefined;
      }
      if (ts.isJsxSpreadAttribute(parent)) return rest;
      if (ts.isVariableDeclaration(parent) && parent.initializer === node) return rest;
      if (
        ts.isCallExpression(parent) &&
        ts.isIdentifier(parent.expression) &&
        parent.arguments.includes(node as ts.Expression) &&
        rest.length === 0
      ) {
        // Handed to a helper; the walk up follows the helper's parameter, where the helper is
        // the component's own or its module's.
        return rest;
      }
      if (ts.isReturnStatement(parent) || ts.isArrowFunction(parent)) {
        return rest.length === 0 ? rest : undefined;
      }
      if (
        ts.isCallExpression(parent) &&
        parent.arguments.includes(node as ts.Expression) &&
        /createPortal$/.test(parent.expression.getText()) &&
        rest.length === 0
      ) {
        node = parent;
        continue;
      }
      return undefined;
    }
    node = parent;
  }
}

/** Records, on a trees walk, what between a mount and the routes the walk cannot read. */
function unreadAt(walk: Walk, node: ts.Node, what: string): void {
  walk.unread?.push(`${where(node)}: ${what}`);
}

/** A tag of the platform's own (`div`), which draws what it holds as it is. */
function isPlainTag(tag: ts.JsxTagNameExpression): boolean {
  return ts.isJsxNamespacedName(tag) || (ts.isIdentifier(tag) && /^[a-z]/.test(tag.text));
}

/** The declaration an id (`<module>#<name>`) names. */
function declOf(program: Program, id: string | undefined): ts.Node | undefined {
  if (id === undefined) return undefined;
  const at = id.lastIndexOf("#");
  return program.mods.get(id.slice(0, at))?.decls.get(id.slice(at + 1));
}

/** Library elements that draw what they wrap as it is, by `<module>#<export>`, each with why the
 *  census reads it as a plain element. Any other library element between a mount and the routes
 *  is unread. */
const LIBRARY_WRAPPERS: Readonly<Record<string, string>> = {
  "react#Fragment": "draws its children and nothing else",
  "react#Suspense":
    "draws its children, or its fallback until they first load; a tree it has drawn that suspends again is hidden and kept, not unmounted",
};

/** The binding element of a component's props that `name` is read out of. */
function propBinding(comp: Comp, name: string): ts.BindingElement | undefined {
  const patterns: ts.BindingName[] = [];
  if (comp.cls === undefined) {
    const first = comp.fn.parameters[0];
    if (first !== undefined) patterns.push(first.name);
  } else {
    const visit = (node: ts.Node) => {
      if (ts.isVariableDeclaration(node) && node.initializer && isThis(node.initializer, "props")) {
        patterns.push(node.name);
      }
      ts.forEachChild(node, visit);
    };
    if (comp.fn.body !== undefined) visit(comp.fn.body);
  }
  for (const pattern of patterns) {
    const hit = bindingNamed(pattern, name);
    if (hit !== undefined && hit !== true) return hit;
  }
  return undefined;
}

/** A tag a component takes as a prop (`as: Tag = "div"`), which draws a plain element where its
 *  default and every value the component's users pass are strings. */
function plainTagProp(program: Program, comp: Comp, tag: ts.Identifier): boolean {
  const found = lookup(tag, comp);
  if (found.kind !== "prop") return false;
  const fallback = propBinding(comp, tag.text)?.initializer;
  if (fallback !== undefined && !ts.isStringLiteralLike(fallback)) return false;
  return (program.usesById.get(comp.id) ?? []).every(({ element }) => {
    if (spreads(element).length > 0) return false;
    const passed = attribute(element, found.prop);
    if (passed === undefined) return fallback !== undefined;
    const value = passed.initializer;
    return (
      value !== undefined &&
      (ts.isStringLiteral(value) ||
        (ts.isJsxExpression(value) &&
          value.expression !== undefined &&
          ts.isStringLiteralLike(value.expression)))
    );
  });
}

/** Whether an element the census reads no component for draws what it wraps as it is: a
 *  context's provider, a library element known to, or a plain tag its component takes as a
 *  string. */
function drawsAsItIs(program: Program, comp: Comp, element: ts.Node): boolean {
  const mod = modOf(program, element);
  const tag = tagOf(element) as ts.JsxTagNameExpression;
  const context =
    ts.isPropertyAccessExpression(tag) && tag.name.text === "Provider" ? tag.expression : tag;
  if (ts.isIdentifier(context)) {
    const decl = declOf(program, resolveLocal(program, mod, context.text));
    const init =
      decl !== undefined && ts.isVariableDeclaration(decl) ? decl.initializer : undefined;
    if (init !== undefined && calleeName(init) === "createContext") return true;
  }
  if (!ts.isIdentifier(tag)) return false;
  const binding = mod.imports.get(tag.text);
  if (
    binding !== undefined &&
    binding.from === undefined &&
    LIBRARY_WRAPPERS[`${binding.spec}#${binding.name}`] !== undefined
  ) {
    return true;
  }
  return plainTagProp(program, comp, tag);
}

/** Follows a slot handed to the component an element draws, its `prop` (`children`, for what
 *  the element wraps), into that component's body. Answers why the census cannot, where it
 *  cannot: the element names no component it reads, or the component draws the slot nowhere it
 *  can see. */
function followElement(
  walk: Walk,
  comp: Comp,
  element: ts.Node,
  prop: string,
  subpath: readonly string[],
  items: Item[],
  seen: Set<ts.Node>,
  depth: number
): string | undefined {
  const { program } = walk;
  const id = tagComp(program, modOf(program, element), element);
  if (id !== undefined && program.comps.has(id)) {
    return followSlot(walk, id, prop, subpath, items, seen, depth);
  }
  if (prop === "children" && drawsAsItIs(program, comp, element)) return undefined;
  return id === undefined || declOf(program, id) === undefined
    ? "is no component of the code the census reads"
    : "is no function or class component the census reads";
}

/** Where `receiver` draws what it was handed as `prop` (at `subpath` inside it, for an object
 *  handed over), and what decides whether it does: a class component's through `this.props`.
 *  Answers why the census cannot follow it, where it cannot. */
function followSlot(
  walk: Walk,
  receiverId: string,
  prop: string,
  subpath: readonly string[],
  items: Item[],
  seen: Set<ts.Node>,
  depth: number
): string | undefined {
  const receiver = walk.program.comps.get(receiverId);
  if (receiver?.fn.body === undefined) return "is no component with a body the census reads";
  if (depth > 6) return "sits deeper than the census reads";
  let drawn = false;
  const visit = (node: ts.Node) => {
    let start: ts.Node | undefined;
    if (ts.isIdentifier(node)) {
      const parent = node.parent;
      const isName =
        (ts.isPropertyAccessExpression(parent) && parent.name === node) ||
        (ts.isBindingElement(parent) && (parent.name === node || parent.propertyName === node));
      if (!isName) {
        const found = lookup(node, receiver);
        if (found.kind === "prop" && found.prop === prop) start = node;
        if (
          found.kind === "props-object" &&
          ts.isPropertyAccessExpression(parent) &&
          parent.name.text === prop
        ) {
          start = parent;
        }
      }
    } else if (
      receiver.cls !== undefined &&
      ts.isPropertyAccessExpression(node) &&
      isThis(node.expression, "props") &&
      node.name.text === prop
    ) {
      start = node;
    }
    const rest = start === undefined ? undefined : placed(start, subpath);
    if (start !== undefined && rest !== undefined) {
      drawn = true;
      walk.receivers?.add(receiverId);
      walkUp(walk, start, receiver, items, true, seen, depth, rest);
    }
    ts.forEachChild(node, visit);
  };
  visit(receiver.fn.body);
  return drawn ? undefined : `draws its ${prop} nowhere the census can see`;
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

/** A mount's name: its host and the send name it is drawn under, which no two mounts of one host
 *  share; a mount drawn under none is named by its place among its host's. */
function mountNames(mounts: readonly Pick<Mount, "element" | "host">[]): Mount[] {
  const taken = new Map<string, number>();
  return mounts.map((mount) => {
    const send = attribute(mount.element, "mutationKey");
    const base = `<MentionComposer${send === undefined ? "" : ` ${text(send)}`}>`;
    const count = (taken.get(`${mount.host.name} ${base}`) ?? 0) + 1;
    taken.set(`${mount.host.name} ${base}`, count);
    const shown = count === 1 ? base : `${base} #${count}`;
    return { ...mount, name: `${mount.host.name} ${shown}`, shown };
  });
}

/** Whether a node sits outside every function and class: the app's root, as `main.tsx`
 *  renders it. */
function atTopLevel(node: ts.Node): boolean {
  for (let current = node.parent; !ts.isSourceFile(current); current = current.parent) {
    if (ts.isFunctionLike(current) || ts.isClassLike(current)) return false;
  }
  return true;
}

/** Reads every mount of `MentionComposer` and the tree above it, and what it cannot read. */
export function readTrees(program: Program): Trees {
  const composer = [...program.comps.values()].find(
    (comp) => comp.id === "features/conversation/MentionComposer.tsx#MentionComposer"
  );
  if (composer === undefined) throw new Error("MentionComposer not found");
  const addressProps = [...readAddressProps(composer)].sort();
  /** The places a component is drawn, by its element and the component drawing it. */
  const edges = new Map<ts.Node, Map<string, Edge>>();
  const wanted = new Map<string, Set<string>>();
  const hostsOf = new Map<string, Set<string>>();
  const unread = new Set<string>();
  /** Walks up from `start` in `comp`, each line it cannot read recorded as being above `above`. */
  const walkFrom = (start: ts.Node, comp: Comp, items: Item[], above: string) => {
    const walk = { program, unread: [] as string[] };
    walkUp(walk, start, comp, items, false, new Set());
    for (const line of walk.unread) unread.add(`${line}, above ${above}`);
  };

  /** Climbs from `startId`, whose `props` decide where a mount below it lives or whether it is
   *  drawn, through every component that draws it, to the routes and the app's root. */
  const climb = (startId: string, props: ReadonlySet<string>, host: string) => {
    const queue: { id: string; props: Set<string> }[] = [{ id: startId, props: new Set(props) }];
    for (let step = queue.shift(); step !== undefined; step = queue.shift()) {
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
          let edge = places.get(parent?.id ?? "");
          if (edge === undefined) {
            edge = {
              child,
              element: use.element,
              hosts: new Set(),
              lines: [],
              parent,
              routed: false,
              structural: false,
            };
            places.set(parent?.id ?? "", edge);
          }
          edge.hosts.add(host);
          if (parent === undefined) continue;
          const next = new Set<string>();
          if (!edge.structural) {
            edge.structural = true;
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
            walkFrom(use.element, parent, items, `<${child.name}> in ${parent.name}`);
            for (const item of items) {
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

  // Each use of `MentionComposer` is a mount of each component it is drawn in; one drawn in none
  // the census reads, or read as a value rather than drawn, is a mount it cannot place.
  const placedMounts: Pick<Mount, "element" | "host">[] = [];
  for (const { element } of program.usesById.get(composer.id) ?? []) {
    const hosts = drawnIn(program, element);
    if (hosts.length === 0) {
      unread.add(`${where(element)}: <MentionComposer>, drawn by no component the census reads`);
    }
    for (const host of hosts) placedMounts.push({ element, host });
  }
  for (const ref of moduleRefs(program, composer.id)) {
    unread.add(`${where(ref)}: MentionComposer, read as a value, not drawn as an element`);
  }
  const mounts = mountNames(placedMounts);
  const items: TreeItem[] = [];
  for (const mount of mounts) {
    const { element, host } = mount;
    const lines: Item[] = [];
    const carried = new Set<string>();
    for (const prop of addressProps) {
      const passed = attribute(element, prop);
      if (passed?.initializer === undefined) continue;
      const line: Item = {
        at: where(passed),
        deps: dependsOn(passed.initializer, host),
        expression: text(passed),
        inside: false,
        kind: prop === "key" ? "key" : "address prop",
        unit: host,
      };
      for (const name of propsRead(line)) carried.add(name);
      // A value the host computes from nothing of its own never moves the address.
      if (line.deps.length > 0) lines.push(line);
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
    const walked: Item[] = [];
    walkFrom(element, host, walked, mount.name);
    for (const item of walked) {
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
    climb(host.id, carried, mount.name);
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
  return { composer, edges: allEdges, items, mounts, program, unread: [...unread] };
}
