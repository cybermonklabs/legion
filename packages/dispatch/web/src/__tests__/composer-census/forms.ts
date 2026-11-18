/**
 * The composer census (INFRA-1503), its forms: where a host draws, hides, moves or re-keys its
 * composer by the layout.
 *
 * The app reads the layout from the viewport's width: `useMediaQuery(Q)` and
 * `window.matchMedia(Q).matches` for a width query Q (the `*_QUERY` constants holding a width,
 * in `features/shell/useDialog.ts`), and Tailwind's responsive classes (`xl:block`), which the
 * stylesheet switches at the same widths. What is computed from a read is the layout's too: a
 * local, a prop a caller passes one in, a hook's argument and what the hook answers, and a state
 * set under a branch on one. The forms list, for each host and each component above a mount up
 * to the routes, every branch, key or early return on the layout that the mount sits under, each
 * element around it whose class, role or dialog the layout decides, and each prop the layout
 * decides on the mount or on a component on the way up; and, in those components and in the
 * hooks they call, every other branch on the layout, which changes what a press or an effect
 * does there.
 */
import * as ts from "typescript";

import {
  attribute,
  attributesOf,
  bindingNamed,
  type Comp,
  calleeName,
  callsOf,
  componentFunction,
  lookup,
  type Program,
  resolveLocal,
  text,
  where,
} from "./program";
import { CONDITIONAL_OPERATORS, containsReturn, type Item, type Trees, walkUp } from "./trees";

/** A form of the layout, with the mounts above which it sits. */
export interface FormItem {
  /** The component or hook whose code holds the form. */
  readonly host: string;
  /** The component drawn at the place the form was found from: `MentionComposer` at a mount,
   *  and `-` for a branch of a component's or a hook's body that wraps no mount. */
  readonly mount: string;
  readonly expression: string;
  readonly at: string;
  readonly mounts: readonly string[];
  /** How the expression reads the layout. */
  readonly why: string;
}

/** A Tailwind class that shows or hides what it is on from one width on. */
const RESPONSIVE_DISPLAY =
  /(?:^|\s)(?:max-)?(?:sm|md|lg|xl|2xl):(?:hidden|block|flex|grid|contents|inline|inline-block|inline-flex|table|invisible|visible|sr-only|not-sr-only)(?=\s|$)/;

/** How a value depends on the layout: wholly, with the way it does; in some of its fields; or
 *  not at all. A whole value is the layout's directly where it is computed from a read (a flag,
 *  a choice between two values), and only as data where the layout decided whether it is there
 *  at all (`cond ? thread : undefined`, a state set under a branch on the layout): such a value
 *  turns a branch of the component it is in, but handed down it is only the value, so a prop
 *  carries the layout to the component below only directly. */
type Shape =
  | { readonly why: string; readonly direct: boolean }
  | { readonly fields: ReadonlyMap<string, Shape> }
  | undefined;

/** A unit read with what its props or parameters carry of the layout. */
interface Env {
  readonly key: string;
  readonly params: ReadonlyMap<string, Shape>;
  readonly unit: Comp;
}

/** What the forms read the program with. */
interface Layout {
  readonly program: Program;
  /** The width queries by name, as the app declares them. */
  readonly queries: ReadonlyMap<string, string>;
  /** Hooks, read as components are: their parameters stand where a component's props do. */
  readonly hooks: ReadonlyMap<string, Comp>;
  readonly unitByFn: ReadonlyMap<ts.Node, Comp>;
  /** The props each component is passed the layout in, by any caller. */
  readonly layoutProps: Map<string, Map<string, Shape>>;
  shapeMemo: Map<ts.Node, Map<string, Shape | "pending">>;
}

/** A node's text cut to `limit` characters, for the words of a reason, which no key holds. */
function brief(node: ts.Node, limit: number): string {
  const shown = text(node);
  return shown.length > limit ? `${shown.slice(0, limit - 1)}…` : shown;
}

const shortWhere = (node: ts.Node) => where(node).replace(/^.*\//, "");
const lineOf = (node: ts.Node) => where(node).replace(/^.*:/, ":");

/** A read of the layout: `useMediaQuery(Q)` or `matchMedia(Q)` for a width query. */
function isLayoutRead(layout: Layout, node: ts.Node): node is ts.CallExpression {
  if (!ts.isCallExpression(node)) return false;
  const callee = node.expression;
  const name = ts.isIdentifier(callee)
    ? callee.text
    : ts.isPropertyAccessExpression(callee)
      ? callee.name.text
      : undefined;
  if (name !== "useMediaQuery" && name !== "matchMedia") return false;
  const query = node.arguments[0];
  if (query === undefined) return false;
  if (ts.isStringLiteralLike(query)) return /width/.test(query.text);
  return ts.isIdentifier(query) && layout.queries.has(query.text);
}

/** The component or hook a node sits in. */
function unitOf(layout: Layout, node: ts.Node): Comp | undefined {
  let current: ts.Node | undefined = node;
  while (current !== undefined) {
    const unit = layout.unitByFn.get(current);
    if (unit !== undefined) return unit;
    current = current.parent;
  }
  return undefined;
}

function whyOf(shape: Shape): string | undefined {
  return shape !== undefined && "why" in shape ? shape.why : undefined;
}

function select(shape: Shape, path: readonly string[]): Shape {
  let current = shape;
  for (const step of path) {
    if (current === undefined || "why" in current) return current;
    current = current.fields.get(step);
  }
  return current;
}

function merge(a: Shape, b: Shape): Shape {
  if (a === undefined) return b;
  if (b === undefined) return a;
  if ("why" in a && "why" in b) return a.direct || !b.direct ? a : b;
  if ("why" in a) return a;
  if ("why" in b) return b;
  const fields = new Map(a.fields);
  for (const [name, field] of b.fields) fields.set(name, merge(fields.get(name), field));
  return { fields };
}

/** The first of `shapes` that is the layout's, a direct one before one that is only data. */
function pick(shapes: readonly Shape[]): Shape {
  let found: Shape;
  for (const shape of shapes) {
    if (shape === undefined || !("why" in shape)) continue;
    if (shape.direct) return shape;
    found ??= shape;
  }
  return found;
}

/** What of a value carries the layout directly. */
function directOnly(shape: Shape): Shape {
  if (shape === undefined) return undefined;
  if ("why" in shape) return shape.direct ? shape : undefined;
  const fields = new Map<string, Shape>();
  for (const [name, field] of shape.fields) {
    const kept = directOnly(field);
    if (kept !== undefined) fields.set(name, kept);
  }
  return fields.size === 0 ? undefined : { fields };
}

function signature(shape: Shape): string {
  if (shape === undefined) return "";
  if ("why" in shape) return `${shape.direct ? "d" : "w"}:${shape.why}`;
  const fields = [...shape.fields].sort(([a], [b]) => a.localeCompare(b));
  return `{${fields.map(([name, field]) => `${name}=${signature(field)}`).join(",")}}`;
}

/** A reason, with the local it reached the value through, kept short. */
function through(why: string, local: string): string {
  return why.split(" <- ").length > 4 ? why : `${why} <- ${local}`;
}

function envFor(layout: Layout, unit: Comp, params?: ReadonlyMap<string, Shape>): Env {
  const given = params ?? layout.layoutProps.get(unit.id) ?? new Map<string, Shape>();
  const key = `${unit.id}|${[...given]
    .map(([name, shape]) => `${name}:${signature(shape)}`)
    .sort()
    .join(";")}`;
  return { key, params: given, unit };
}

function shapeOf(layout: Layout, node: ts.Node | undefined, env: Env): Shape {
  if (node === undefined) return undefined;
  let byEnv = layout.shapeMemo.get(node);
  if (byEnv === undefined) {
    byEnv = new Map();
    layout.shapeMemo.set(node, byEnv);
  }
  if (byEnv.has(env.key)) {
    const known = byEnv.get(env.key);
    return known === "pending" ? undefined : known;
  }
  byEnv.set(env.key, "pending");
  const shape = computeShape(layout, node, env);
  byEnv.set(env.key, shape);
  return shape;
}

function firstWhy(layout: Layout, nodes: readonly (ts.Node | undefined)[], env: Env): Shape {
  return pick(nodes.map((node) => shapeOf(layout, node, env)));
}

/** An arm that leaves the value out: `undefined`, `null`, `void 0`, in parentheses or a cast. */
function isNothing(node: ts.Node): boolean {
  let inner = node;
  while (
    ts.isParenthesizedExpression(inner) ||
    ts.isAsExpression(inner) ||
    ts.isSatisfiesExpression(inner) ||
    ts.isNonNullExpression(inner) ||
    ts.isTypeAssertionExpression(inner) ||
    ts.isPartiallyEmittedExpression(inner)
  ) {
    inner = inner.expression;
  }
  return (
    (ts.isIdentifier(inner) && inner.text === "undefined") ||
    inner.kind === ts.SyntaxKind.NullKeyword ||
    ts.isVoidExpression(inner)
  );
}

function computeShape(layout: Layout, node: ts.Node, env: Env): Shape {
  if (
    ts.isParenthesizedExpression(node) ||
    ts.isAsExpression(node) ||
    ts.isSatisfiesExpression(node) ||
    ts.isNonNullExpression(node) ||
    ts.isAwaitExpression(node) ||
    ts.isTypeAssertionExpression(node) ||
    ts.isSpreadElement(node)
  ) {
    return shapeOf(layout, node.expression, env);
  }
  if (ts.isJsxExpression(node)) return shapeOf(layout, node.expression, env);
  if (isLayoutRead(layout, node)) {
    return { direct: true, why: `${brief(node, 70)} @${shortWhere(node)}` };
  }
  if (ts.isIdentifier(node)) return identShape(layout, node, env);
  if (ts.isPropertyAccessExpression(node)) {
    return select(shapeOf(layout, node.expression, env), [node.name.text]);
  }
  if (ts.isElementAccessExpression(node)) {
    return firstWhy(layout, [node.expression, node.argumentExpression], env);
  }
  if (ts.isCallExpression(node)) return callShape(layout, node, env);
  if (ts.isConditionalExpression(node)) {
    const condition = shapeOf(layout, node.condition, env);
    const why = whyOf(condition);
    if (why !== undefined) {
      const presence = isNothing(node.whenTrue) || isNothing(node.whenFalse);
      const direct = condition !== undefined && "why" in condition && condition.direct;
      return { direct: direct && !presence, why };
    }
    return merge(shapeOf(layout, node.whenTrue, env), shapeOf(layout, node.whenFalse, env));
  }
  if (ts.isBinaryExpression(node)) {
    const kind = node.operatorToken.kind;
    if (kind >= ts.SyntaxKind.FirstAssignment && kind <= ts.SyntaxKind.LastAssignment) {
      return undefined;
    }
    const left = shapeOf(layout, node.left, env);
    const right = shapeOf(layout, node.right, env);
    const either = pick([left, right]);
    if (either !== undefined) return either;
    return CONDITIONAL_OPERATORS.has(kind) ? merge(left, right) : undefined;
  }
  if (ts.isPrefixUnaryExpression(node) || ts.isPostfixUnaryExpression(node)) {
    return firstWhy(layout, [node.operand], env);
  }
  if (ts.isTypeOfExpression(node)) return firstWhy(layout, [node.expression], env);
  if (ts.isObjectLiteralExpression(node)) {
    let fields = new Map<string, Shape>();
    for (const property of node.properties) {
      if (ts.isPropertyAssignment(property)) {
        const shape = shapeOf(layout, property.initializer, env);
        if (shape !== undefined) fields.set(property.name.getText(), shape);
      } else if (ts.isShorthandPropertyAssignment(property)) {
        const shape = shapeOf(layout, property.name, env);
        if (shape !== undefined) fields.set(property.name.text, shape);
      } else if (ts.isSpreadAssignment(property)) {
        const shape = shapeOf(layout, property.expression, env);
        if (whyOf(shape) !== undefined) return shape;
        if (shape !== undefined && "fields" in shape) {
          const merged = merge({ fields }, shape);
          fields = merged !== undefined && "fields" in merged ? new Map(merged.fields) : fields;
        }
      }
    }
    return fields.size === 0 ? undefined : { fields };
  }
  if (ts.isArrayLiteralExpression(node)) return firstWhy(layout, node.elements, env);
  if (ts.isTemplateExpression(node)) {
    return firstWhy(
      layout,
      node.templateSpans.map((span) => span.expression),
      env
    );
  }
  return undefined;
}

/** The path inside a destructured value to one of its bindings. */
function bindingPath(name: ts.BindingName, target: ts.BindingElement): string[] | undefined {
  if (ts.isIdentifier(name)) return undefined;
  for (const [index, element] of name.elements.entries()) {
    if (ts.isOmittedExpression(element)) continue;
    const step = ts.isObjectBindingPattern(name)
      ? (element.propertyName ?? element.name).getText()
      : String(index);
    if (element === target) return [step];
    if (!ts.isIdentifier(element.name)) {
      const inner = bindingPath(element.name, target);
      if (inner !== undefined) return [step, ...inner];
    }
  }
  return undefined;
}

/** Which of a unit's props or parameters binds `ident`: a component's prop by name, its whole
 *  props as "", and a hook's parameter by its index and, destructured, its field. */
function paramKey(layout: Layout, ident: ts.Identifier, unit: Comp): string | undefined {
  const isHook = layout.hooks.has(unit.id);
  const params = unit.fn.parameters;
  for (let index = 0; index < params.length; index++) {
    const hit = bindingNamed(params[index].name, ident.text);
    if (hit === undefined) continue;
    if (hit === true) return isHook ? String(index) : "";
    let top: ts.BindingElement = hit;
    while (ts.isBindingElement(top.parent.parent)) top = top.parent.parent;
    const prop = (top.propertyName ?? top.name).getText();
    return isHook ? `${index}.${prop}` : prop;
  }
  return undefined;
}

function identShape(layout: Layout, ident: ts.Identifier, env: Env): Shape {
  const found = lookup(ident, env.unit);
  if (found.kind === "prop" || found.kind === "props-object") {
    const key = paramKey(layout, ident, env.unit);
    if (key === undefined) return undefined;
    if (key === "") return env.params.size === 0 ? undefined : { fields: env.params };
    return env.params.get(key);
  }
  if (found.kind === "local") {
    const init = found.decl.initializer;
    if (init === undefined) return undefined;
    const hook = calleeName(init);
    if (hook === "useState" || hook === "useReducer") {
      const path =
        found.element === undefined ? undefined : bindingPath(found.decl.name, found.element);
      return path?.[0] === "0" ? stateShape(layout, found.decl, ident.text, env) : undefined;
    }
    if (hook === "useRef" || hook === "useSyncExternalStore") return undefined;
    let shape = shapeOf(layout, init, env);
    if (found.element !== undefined) {
      shape = select(shape, bindingPath(found.decl.name, found.element) ?? []);
    }
    if (shape === undefined || !("why" in shape)) return shape;
    return { direct: shape.direct, why: through(shape.why, ident.text) };
  }
  if (found.kind === "param") {
    // An item of a list the layout decides is the layout's.
    const fn = found.fn;
    const parent = fn.parent;
    if (
      ts.isCallExpression(parent) &&
      ts.isPropertyAccessExpression(parent.expression) &&
      parent.arguments.includes(fn as unknown as ts.Expression)
    ) {
      const why = whyOf(shapeOf(layout, parent.expression.expression, env));
      return why === undefined ? undefined : { direct: false, why: through(why, ident.text) };
    }
  }
  return undefined;
}

/** Where a statement runs only while the layout says so: the branch it sits in, or an early
 *  return before it, on the layout. */
function guardOf(
  layout: Layout,
  node: ts.Node,
  env: Env
): { readonly condition: ts.Expression; readonly why: string } | undefined {
  let child = node;
  let current: ts.Node | undefined = node.parent;
  while (current !== undefined && child !== env.unit.fn) {
    let condition: ts.Expression | undefined;
    if (ts.isIfStatement(current) && child !== current.expression) condition = current.expression;
    else if (ts.isConditionalExpression(current) && child !== current.condition) {
      condition = current.condition;
    } else if (
      ts.isBinaryExpression(current) &&
      CONDITIONAL_OPERATORS.has(current.operatorToken.kind) &&
      child === current.right
    ) {
      condition = current.left;
    } else if (ts.isCaseClause(current)) condition = current.parent.parent.expression;
    if (condition !== undefined) {
      const why = whyOf(shapeOf(layout, condition, env));
      if (why !== undefined) return { condition, why };
    }
    if (ts.isBlock(current)) {
      for (const sibling of current.statements) {
        if (sibling === child) break;
        if (ts.isIfStatement(sibling) && containsReturn(sibling.thenStatement)) {
          const why = whyOf(shapeOf(layout, sibling.expression, env));
          if (why !== undefined) return { condition: sibling.expression, why };
        }
      }
    }
    child = current;
    current = current.parent;
  }
  return undefined;
}

/** A state is the layout's where it starts from the layout or is set to it, and as data where
 *  it is set under a branch on the layout. */
function stateShape(layout: Layout, decl: ts.VariableDeclaration, state: string, env: Env): Shape {
  const init = decl.initializer as ts.CallExpression;
  const start = pick([shapeOf(layout, init.arguments[0], env)]);
  if (start !== undefined && "why" in start) {
    return { direct: start.direct, why: through(start.why, state) };
  }
  if (!ts.isArrayBindingPattern(decl.name)) return undefined;
  const setter = decl.name.elements[1];
  if (setter === undefined || ts.isOmittedExpression(setter) || !ts.isIdentifier(setter.name)) {
    return undefined;
  }
  for (const call of callsOf(setter.name.text, env.unit)) {
    const found = lookup(call.expression as ts.Identifier, env.unit);
    if (found.kind !== "local" || found.decl !== decl) continue;
    const value = pick([shapeOf(layout, call.arguments[0], env)]);
    if (value !== undefined && "why" in value) {
      return {
        direct: value.direct,
        why: `state ${state} @${shortWhere(decl)}, set at ${lineOf(call)} to ${value.why}`,
      };
    }
    const guard = guardOf(layout, call, env);
    if (guard !== undefined) {
      const under = brief(guard.condition, 70);
      return {
        direct: false,
        why: `state ${state} @${shortWhere(decl)}, set at ${lineOf(call)} under ${under}`,
      };
    }
  }
  return undefined;
}

/** What a function hands back, read in `env`. */
function returnedShape(layout: Layout, fn: ts.Node | undefined, env: Env): Shape {
  const body =
    fn !== undefined &&
    (ts.isArrowFunction(fn) || ts.isFunctionExpression(fn) || ts.isFunctionDeclaration(fn))
      ? fn.body
      : undefined;
  if (body === undefined) return undefined;
  if (!ts.isBlock(body)) return shapeOf(layout, body, env);
  let shape: Shape;
  const visit = (node: ts.Node) => {
    if (node !== body && ts.isFunctionLike(node)) return;
    if (ts.isReturnStatement(node) && node.expression !== undefined) {
      shape = merge(shape, shapeOf(layout, node.expression, env));
    }
    ts.forEachChild(node, visit);
  };
  visit(body);
  return shape;
}

/** A hook of the app's own that `call` calls, other than the layout read itself. */
function hookCalled(layout: Layout, call: ts.CallExpression, unit: Comp): Comp | undefined {
  if (!ts.isIdentifier(call.expression) || call.expression.text === "useMediaQuery") {
    return undefined;
  }
  const id = resolveLocal(layout.program, unit.mod, call.expression.text);
  return id === undefined ? undefined : layout.hooks.get(id);
}

/** What a call to `hook` hands its parameters of the layout, directly. */
function hookArguments(
  layout: Layout,
  call: ts.CallExpression,
  hook: Comp,
  env: Env
): Map<string, Shape> {
  const params = new Map<string, Shape>();
  hook.fn.parameters.forEach((param, index) => {
    const shape = directOnly(shapeOf(layout, call.arguments[index], env));
    if (shape === undefined) return;
    if (ts.isIdentifier(param.name)) params.set(String(index), shape);
    else if (ts.isObjectBindingPattern(param.name)) {
      for (const element of param.name.elements) {
        const prop = (element.propertyName ?? element.name).getText();
        const field = select(shape, [prop]);
        if (field !== undefined) params.set(`${index}.${prop}`, field);
      }
    }
  });
  return params;
}

function callShape(layout: Layout, call: ts.CallExpression, env: Env): Shape {
  const name = calleeName(call);
  if (name === "useMemo") return returnedShape(layout, call.arguments[0], env);
  if (name === "useCallback" || name === "useRef" || name === "useEffect") return undefined;
  const hook = hookCalled(layout, call, env.unit);
  if (hook !== undefined) {
    const hookEnv = envFor(layout, hook, hookArguments(layout, call, hook, env));
    return returnedShape(layout, hook.fn, hookEnv);
  }
  // Anything else answers from the layout where its receiver or an argument does.
  const receiver = ts.isPropertyAccessExpression(call.expression)
    ? call.expression.expression
    : undefined;
  return firstWhy(layout, [receiver, ...call.arguments], env);
}

/** Reads the width queries, the hooks, and which props each component is passed the layout in:
 *  every caller's attributes, until a round adds nothing. */
function readLayout(program: Program): Layout {
  const queries = new Map<string, string>();
  const hooks = new Map<string, Comp>();
  const unitByFn = new Map<ts.Node, Comp>(program.compByFn);
  for (const mod of program.mods.values()) {
    for (const [name, decl] of mod.decls) {
      if (/_QUERY$/.test(name) && ts.isVariableDeclaration(decl)) {
        const init = decl.initializer;
        if (init !== undefined && ts.isStringLiteralLike(init) && /width/.test(init.text)) {
          queries.set(name, init.text);
        }
      }
      if (!/^use[A-Z]/.test(name)) continue;
      const fn = componentFunction(decl);
      if (fn === undefined) continue;
      const hook: Comp = { fn, id: `${mod.rel}#${name}`, mod, name };
      hooks.set(hook.id, hook);
      unitByFn.set(fn, hook);
    }
  }
  const layout: Layout = {
    hooks,
    layoutProps: new Map(),
    program,
    queries,
    shapeMemo: new Map(),
    unitByFn,
  };
  for (let round = 0; round < 8; round++) {
    layout.shapeMemo = new Map();
    let changed = false;
    for (const [childId, uses] of program.usesById) {
      if (!program.comps.has(childId)) continue;
      for (const use of uses) {
        const parent = unitOf(layout, use.element);
        if (parent === undefined) continue;
        const env = envFor(layout, parent);
        const passed = new Map<string, Shape>();
        for (const property of attributesOf(use.element)?.properties ?? []) {
          if (ts.isJsxAttribute(property)) {
            const name = property.name.getText();
            const init = property.initializer;
            const value =
              init !== undefined && ts.isJsxExpression(init) ? init.expression : undefined;
            const shape = directOnly(shapeOf(layout, value, env));
            if (shape === undefined || name === "key") continue;
            passed.set(
              name,
              whyOf(shape) === undefined
                ? shape
                : {
                    direct: true,
                    why: `prop ${name}, passed ${brief(property, 70)} @${shortWhere(property)}`,
                  }
            );
          } else {
            const shape = directOnly(shapeOf(layout, property.expression, env));
            if (shape !== undefined && "fields" in shape) {
              for (const [name, field] of shape.fields) passed.set(name, field);
            }
          }
        }
        if (passed.size === 0) continue;
        const known = layout.layoutProps.get(childId) ?? new Map<string, Shape>();
        for (const [name, shape] of passed) {
          const merged = merge(known.get(name), shape);
          if (signature(merged) !== signature(known.get(name))) {
            known.set(name, merged);
            changed = true;
          }
        }
        layout.layoutProps.set(childId, known);
      }
    }
    if (!changed) break;
  }
  layout.shapeMemo = new Map();
  return layout;
}

interface Form {
  readonly at: string;
  readonly expression: string;
  readonly mount: string;
  readonly mounts: Set<string>;
  readonly unit: string;
  readonly why: string;
}

/** The text of the string parts of a class expression. */
function classTexts(node: ts.Node): string[] {
  const found: string[] = [];
  const visit = (current: ts.Node) => {
    if (
      ts.isStringLiteral(current) ||
      ts.isNoSubstitutionTemplateLiteral(current) ||
      ts.isTemplateHead(current) ||
      ts.isTemplateMiddle(current) ||
      ts.isTemplateTail(current)
    ) {
      found.push(current.text);
    }
    ts.forEachChild(current, visit);
  };
  visit(node);
  return found;
}

/** A dialog the element's ref attaches: a `useDialog` handle whose `open` is the layout's. */
function dialogWhy(layout: Layout, node: ts.Node, env: Env): string | undefined {
  let why: string | undefined;
  const visit = (current: ts.Node) => {
    if (why !== undefined) return;
    if (
      ts.isPropertyAccessExpression(current) &&
      current.name.text === "containerRef" &&
      ts.isIdentifier(current.expression)
    ) {
      const found = lookup(current.expression, env.unit);
      const init = found.kind === "local" ? found.decl.initializer : undefined;
      const options =
        init !== undefined && ts.isCallExpression(init) && calleeName(init) === "useDialog"
          ? init.arguments[0]
          : undefined;
      const open =
        options !== undefined && ts.isObjectLiteralExpression(options)
          ? options.properties.find(
              (property): property is ts.PropertyAssignment =>
                ts.isPropertyAssignment(property) && property.name.getText() === "open"
            )
          : undefined;
      const openWhy = whyOf(shapeOf(layout, open?.initializer, env));
      if (open !== undefined && openWhy !== undefined) {
        why = `a dialog (useDialog) open on ${brief(open.initializer, 60)}: ${openWhy}`;
      }
    }
    ts.forEachChild(current, visit);
  };
  visit(node);
  return why;
}

/** What the layout decides about an element around a mount: its class, role or dialog. The
 *  element is shown by its tag and the names of those attributes, since their values (a class
 *  string, a ref's callback) change with every edit of the element's look and not with what
 *  the layout does to the composer in it. */
function elementForm(
  layout: Layout,
  opening: ts.JsxOpeningElement,
  env: Env
): { readonly shown: string; readonly why: string } | undefined {
  const names: string[] = [];
  const whys: string[] = [];
  for (const name of ["className", "role", "aria-modal", "ref"]) {
    const init = attribute(opening, name)?.initializer;
    if (init === undefined) continue;
    const value = ts.isJsxExpression(init) ? init.expression : init;
    if (value === undefined) continue;
    let why = name === "ref" ? dialogWhy(layout, value, env) : whyOf(shapeOf(layout, value, env));
    if (name === "className") {
      const responsive = classTexts(value).find((part) => RESPONSIVE_DISPLAY.test(part));
      if (responsive !== undefined) {
        const shown = `the stylesheet's responsive class "${responsive.trim()}"`;
        why = why === undefined ? shown : `${shown}; ${why}`;
      }
    }
    if (why === undefined) continue;
    names.push(name);
    whys.push(`${name}: ${why}`);
  }
  if (names.length === 0) return undefined;
  return { shown: `<${text(opening.tagName)} ${names.join(" ")}>`, why: whys.join(" | ") };
}

function firstFieldWhy(shape: Shape): string | undefined {
  if (shape === undefined) return undefined;
  if ("why" in shape) return shape.why;
  for (const field of shape.fields.values()) {
    const found = firstFieldWhy(field);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** Whether a branch's arm draws or does something: an element, a call, a function, a name. */
function acts(node: ts.Node): boolean {
  let found = false;
  const visit = (current: ts.Node) => {
    if (found) return;
    if (
      ts.isJsxElement(current) ||
      ts.isJsxSelfClosingElement(current) ||
      ts.isJsxFragment(current) ||
      ts.isCallExpression(current) ||
      ts.isArrowFunction(current) ||
      ts.isFunctionExpression(current) ||
      (ts.isIdentifier(current) && current.text !== "undefined") ||
      ts.isPropertyAccessExpression(current)
    ) {
      found = true;
      return;
    }
    ts.forEachChild(current, visit);
  };
  visit(node);
  return found;
}

/** Reads every form of the layout above the mounts `trees` found. */
export function readForms(trees: Trees): FormItem[] {
  const { program } = trees;
  const layout = readLayout(program);
  /** Each form once, by the node it turns on. */
  const forms = new Map<string, Form>();
  /** The mounts each component or hook stands above. */
  const unitMounts = new Map<string, Set<string>>();
  /** The forms found by walking up from a mount or a component's place, which are those
   *  mounts' alone: a branch scan of the whole component does not add its other mounts. */
  const wrapping = new Set<string>();
  const formKey = (node: ts.Node) => `${node.getSourceFile().fileName}:${node.getStart()}`;

  const addMounts = (id: string, mounts: Iterable<string>) => {
    const known = unitMounts.get(id) ?? new Set<string>();
    for (const mount of mounts) known.add(mount);
    unitMounts.set(id, known);
  };
  const addForm = (
    node: ts.Node,
    mounts: Iterable<string>,
    form: Omit<Form, "mounts">,
    wraps: boolean
  ) => {
    const key = formKey(node);
    if (wraps) wrapping.add(key);
    const known = forms.get(key);
    if (known !== undefined) {
      for (const mount of mounts) known.mounts.add(mount);
      return;
    }
    forms.set(key, { ...form, mounts: new Set(mounts) });
  };

  /** Reads the items a forms walk up gathered for the layout. Answers whether a branch, key or
   *  `hidden` on the layout gates what the walk started from. */
  const readItems = (items: readonly Item[], mounts: readonly string[], mount: string) => {
    let gated = false;
    for (const item of items) {
      const node = item.node;
      if (node === undefined) continue;
      const unit = unitOf(layout, node);
      if (unit === undefined) continue;
      const env = envFor(layout, unit);
      if (item.kind === "element" && ts.isJsxOpeningElement(node)) {
        const found = elementForm(layout, node, env);
        if (found !== undefined) {
          const form = { at: item.at, expression: found.shown, mount, unit: unit.name };
          addForm(node, mounts, { ...form, why: found.why }, true);
        }
        continue;
      }
      const why = whyOf(shapeOf(layout, node, env));
      if (why === undefined) continue;
      gated = true;
      const form = { at: item.at, expression: item.expression, mount, unit: unit.name };
      addForm(node, mounts, { ...form, why }, true);
    }
    return gated;
  };

  /** The layout's props on an element of a component: what a caller hands the next one down.
   *  Under a branch on the layout (`gated`) a value only that branch draws is the branch's, so
   *  only a prop the layout decides directly, and the key, are forms of their own there. */
  const readProps = (
    element: ts.Node,
    parent: Comp,
    mounts: readonly string[],
    on: string,
    gated: boolean
  ) => {
    const env = envFor(layout, parent);
    for (const property of attributesOf(element)?.properties ?? []) {
      const value = ts.isJsxAttribute(property)
        ? property.initializer !== undefined && ts.isJsxExpression(property.initializer)
          ? property.initializer.expression
          : undefined
        : property.expression;
      const name = ts.isJsxAttribute(property) ? property.name.getText() : "(spread)";
      const read = shapeOf(layout, value, env);
      const shape = gated && name !== "key" ? directOnly(read) : read;
      if (shape === undefined) continue;
      const why =
        "why" in shape
          ? shape.why
          : `the fields ${[...shape.fields.keys()].join(", ")}: ${firstFieldWhy(shape) ?? ""}`;
      const form = { at: where(property), expression: text(property), mount: on };
      addForm(property, mounts, { ...form, unit: parent.name, why }, true);
    }
  };

  /** Every branch on the layout in a unit's body, its callbacks, effects and handlers. */
  const readBranches = (unit: Comp, env: Env, mounts: readonly string[]) => {
    const visit = (node: ts.Node) => {
      let condition: ts.Expression | undefined;
      let shown = "";
      if (ts.isIfStatement(node)) {
        condition = node.expression;
        const returns = containsReturn(node.thenStatement) ? " return …" : "";
        shown = `if (${text(node.expression)})${returns}`;
      } else if (ts.isSwitchStatement(node)) {
        condition = node.expression;
        shown = `switch (${text(node.expression)})`;
      } else if (
        ts.isConditionalExpression(node) &&
        (acts(node.whenTrue) || acts(node.whenFalse)) &&
        // An attribute's value is the element's: its props and its class, role or dialog above.
        !(ts.isJsxExpression(node.parent) && ts.isJsxAttribute(node.parent.parent))
      ) {
        condition = node.condition;
        shown = `(${text(node.condition)}) ? …`;
      } else if (
        ts.isBinaryExpression(node) &&
        CONDITIONAL_OPERATORS.has(node.operatorToken.kind) &&
        (ts.isJsxExpression(node.parent) || ts.isExpressionStatement(node.parent)) &&
        acts(node.right)
      ) {
        condition = node.left;
        shown = `${text(node.left)} ${node.operatorToken.getText()} …`;
      }
      // A branch the walk up already found is a form of the mounts it wraps, and only theirs.
      if (condition !== undefined && !wrapping.has(formKey(condition))) {
        const why = whyOf(shapeOf(layout, condition, env));
        if (why !== undefined) {
          const form = { at: where(condition), expression: shown, mount: "-" };
          addForm(condition, mounts, { ...form, unit: unit.name, why }, false);
        }
      }
      ts.forEachChild(node, visit);
    };
    if (unit.fn.body !== undefined) visit(unit.fn.body);
  };

  // The mounts: the layout's props on each, and what wraps it in its host.
  for (const { element, host, name } of trees.mounts) {
    addMounts(host.id, [name]);
    const items: Item[] = [];
    const receivers = new Set<string>();
    walkUp({ program, receivers }, element, host, items, false, new Set());
    for (const receiver of receivers) addMounts(receiver, [name]);
    const gated = readItems(items, [name], "MentionComposer");
    readProps(element, host, [name], "MentionComposer", gated);
  }
  // Each place a component above a mount is drawn: the layout's props and key on it, and what
  // wraps it in the component that draws it.
  for (const edge of trees.edges) {
    if (edge.parent === undefined) continue;
    const hosts = [...edge.hosts];
    addMounts(edge.parent.id, hosts);
    addMounts(edge.child.id, hosts);
    const items: Item[] = [];
    const receivers = new Set<string>();
    walkUp({ program, receivers }, edge.element, edge.parent, items, false, new Set());
    for (const receiver of receivers) addMounts(receiver, hosts);
    const gated = readItems(items, hosts, edge.child.name);
    readProps(edge.element, edge.parent, hosts, edge.child.name, gated);
  }

  // Every other branch on the layout in those components, and in the hooks they call, read with
  // what each call hands the hook.
  const hookWork: { hook: Comp; env: Env; mounts: readonly string[] }[] = [];
  const hooksRead = new Set<string>();
  const queueHooks = (unit: Comp, env: Env, mounts: readonly string[]) => {
    const visit = (node: ts.Node) => {
      if (ts.isCallExpression(node)) {
        const hook = hookCalled(layout, node, unit);
        if (hook !== undefined) {
          const hookEnv = envFor(layout, hook, hookArguments(layout, node, hook, env));
          hookWork.push({ env: hookEnv, hook, mounts });
        }
      }
      ts.forEachChild(node, visit);
    };
    if (unit.fn.body !== undefined) visit(unit.fn.body);
  };
  for (const [id, mountsAbove] of [...unitMounts]) {
    const unit = program.comps.get(id);
    if (unit === undefined) continue;
    const env = envFor(layout, unit);
    readBranches(unit, env, [...mountsAbove]);
    queueHooks(unit, env, [...mountsAbove]);
  }
  for (let work = hookWork.shift(); work !== undefined; work = hookWork.shift()) {
    addMounts(work.hook.id, work.mounts);
    const key = `${work.env.key}|${work.mounts.join(",")}`;
    if (hooksRead.has(key)) continue;
    hooksRead.add(key);
    readBranches(work.hook, work.env, work.mounts);
    queueHooks(work.hook, work.env, work.mounts);
  }

  return [...forms.values()].map((form) => ({
    at: form.at,
    expression: form.expression,
    host: form.unit,
    mount: form.mount,
    mounts: [...form.mounts],
    why: form.why,
  }));
}
