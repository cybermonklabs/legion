// The TypeScript half of wholeuse (main.go beside it): every production expression whose static
// type is one of openid-client's error classes (ResponseBodyError, ClientError,
// AuthorizationResponseError, WWWAuthenticateChallengeError, and any other class openid-client or
// oauth4webapi declares under a name ending in Error), read through the TypeScript compiler's own
// type checker, and how each is used: a field compared, a field or the value handed to the box
// (`RemoteText`, `redactRemoteText`, or `BrokerRefusedError`, whose constructor holds both words
// it is given as RemoteText), the value bound to a local whose own uses are listed, or the value
// used whole - formatted, serialized, method-called, passed on, stored, returned or thrown. Such an
// error carries an issuer's or the broker's words in `error`, `error_description`, `error_uri`,
// `cause` (the answer's body) and its message, so a whole use, and a read of one of those that is
// neither compared nor handed to the box, can print them; each is a failure, and it exits 1 naming
// it.
//
// It does not follow an error the checker types `unknown`: a catch clause's binding before it is
// narrowed, which no static type names. Each such value a file that imports openid-client stores
// whole as an error's `cause` is listed as BLIND, which is not a failure, so a reader sees what
// this listing cannot judge.
//
// Usage, from the repository root: bun packages/envoy/internal/redact/testdata/wholeuse/whole-use.ts [tsconfig.json ...]
// With no argument it reads every packages/*/tsconfig.json.
import { readdirSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import ts from "typescript";

/** The fields that hold another party's words. */
const TEXT_FIELDS: Record<string, true> = {
  error: true,
  error_description: true,
  error_uri: true,
  cause: true,
  message: true,
  stack: true,
  response: true,
  body: true,
};
/** The callees a word may be handed to: the box, the rule, and a refusal that boxes at once. */
const BOX_SINKS: Record<string, true> = {
  RemoteText: true,
  redactRemoteText: true,
  BrokerRefusedError: true,
};

interface Use {
  readonly file: string;
  readonly line: number;
  readonly column: number;
  readonly className: string;
  readonly use: string;
  readonly fails: boolean;
  readonly text: string;
}

const root = process.cwd();
const configs =
  process.argv.length > 2
    ? process.argv.slice(2)
    : readdirSync(join(root, "packages"))
        .map((name) => join(root, "packages", name, "tsconfig.json"))
        .filter((path) => ts.sys.fileExists(path));

/** The openid-client error class `type` is an instance of, or one of its union's members is;
 *  undefined if none. The class itself (`ResponseBodyError` in an `instanceof`) is not one. */
function errorClass(type: ts.Type): string | undefined {
  for (const member of type.isUnion() ? type.types : [type]) {
    const symbol = member.getSymbol();
    const file = symbol?.declarations?.[0]?.getSourceFile().fileName ?? "";
    if (
      symbol?.getName().endsWith("Error") &&
      member.getConstructSignatures().length === 0 &&
      (file.includes("/openid-client/") || file.includes("/oauth4webapi/"))
    ) {
      return symbol.getName();
    }
  }
  return undefined;
}

/** `node`'s parent past parentheses, `as`, `!` and `satisfies`. */
function userOf(node: ts.Node): { user: ts.Node; child: ts.Node } {
  let child = node;
  let user = node.parent;
  while (
    ts.isParenthesizedExpression(user) ||
    ts.isAsExpression(user) ||
    ts.isNonNullExpression(user) ||
    ts.isSatisfiesExpression(user)
  ) {
    child = user;
    user = user.parent;
  }
  return { user, child };
}

/** The name a call or `new` calls, as written. */
function calleeName(call: ts.CallExpression | ts.NewExpression): string {
  const callee = call.expression;
  if (ts.isIdentifier(callee)) return callee.text;
  if (ts.isPropertyAccessExpression(callee)) return callee.getText();
  return callee.getText();
}

/** How `user` uses the value `child`: its class, and whether that use fails. */
function wholeUse(user: ts.Node, child: ts.Node): [string, boolean] {
  if (ts.isTemplateSpan(user)) return ["formatted whole (template string)", true];
  if (ts.isBinaryExpression(user)) {
    const op = user.operatorToken.kind;
    if (
      op === ts.SyntaxKind.EqualsEqualsEqualsToken ||
      op === ts.SyntaxKind.ExclamationEqualsEqualsToken ||
      op === ts.SyntaxKind.EqualsEqualsToken ||
      op === ts.SyntaxKind.ExclamationEqualsToken
    ) {
      return ["compared", false];
    }
    if (op === ts.SyntaxKind.InstanceOfKeyword && user.left === child)
      return ["instanceof check", false];
    if (op === ts.SyntaxKind.PlusToken) return ["formatted whole (+)", true];
    if (op === ts.SyntaxKind.QuestionQuestionToken && user.left === child) {
      const next = userOf(user);
      return wholeUse(next.user, next.child);
    }
  }
  if (ts.isCallExpression(user) || ts.isNewExpression(user)) {
    if (user.expression === child) return ["called", true];
    const name = calleeName(user);
    if (BOX_SINKS[name] === true) return [`handed to ${name}`, false];
    if (name === "JSON.stringify") return ["serialized whole by JSON.stringify", true];
    if (/^(console\.|util\.|inspect$|format$|Bun\.inspect|String$)/.test(name))
      return [`formatted whole by ${name}`, true];
    return [`passed whole to ${name}`, true];
  }
  if (ts.isPropertyAssignment(user) || ts.isShorthandPropertyAssignment(user)) {
    return [`stored whole as ${user.name.getText()}`, true];
  }
  if (ts.isVariableDeclaration(user) && user.initializer === child)
    return ["bound to a local (its uses are listed)", false];
  if (ts.isReturnStatement(user)) return ["returned whole", true];
  if (ts.isThrowStatement(user)) return ["thrown whole", true];
  if (ts.isIfStatement(user) || ts.isPrefixUnaryExpression(user))
    return ["tested for truth", false];
  return [`used whole by ${ts.SyntaxKind[user.kind]}`, true];
}

/** Whether `node` is a reference to a value, not a name being declared or a member name. */
function isReference(node: ts.Identifier): boolean {
  const parent = node.parent;
  if (ts.isPropertyAccessExpression(parent) && parent.name === node) return false;
  if (
    (ts.isVariableDeclaration(parent) ||
      ts.isParameter(parent) ||
      ts.isFunctionDeclaration(parent) ||
      ts.isClassDeclaration(parent) ||
      ts.isPropertyAssignment(parent) ||
      ts.isImportSpecifier(parent) ||
      ts.isPropertyDeclaration(parent) ||
      ts.isMethodDeclaration(parent)) &&
    parent.name === node
  ) {
    return false;
  }
  if (ts.isCatchClause(parent) || ts.isTypeReferenceNode(parent) || ts.isImportClause(parent))
    return false;
  return !ts.isPartOfTypeNode(node);
}

const uses: Use[] = [];
const blind: string[] = [];
let files = 0;
for (const config of configs) {
  const read = ts.readConfigFile(config, ts.sys.readFile);
  if (read.error)
    throw new Error(`${config}: ${ts.flattenDiagnosticMessageText(read.error.messageText, "\n")}`);
  const parsed = ts.parseJsonConfigFileContent(read.config, ts.sys, dirname(resolve(config)));
  const program = ts.createProgram(parsed.fileNames, parsed.options);
  const checker = program.getTypeChecker();
  for (const source of program.getSourceFiles()) {
    const name = source.fileName;
    if (
      source.isDeclarationFile ||
      name.includes("/node_modules/") ||
      name.includes("/__tests__/") ||
      name.includes("/test/") ||
      /\.test\.ts$/.test(name) ||
      !parsed.fileNames.includes(name)
    ) {
      continue;
    }
    files++;
    const importsOpenidClient = source.statements.some(
      (s) =>
        ts.isImportDeclaration(s) &&
        ts.isStringLiteral(s.moduleSpecifier) &&
        s.moduleSpecifier.text === "openid-client"
    );
    const visit = (node: ts.Node): void => {
      if (ts.isIdentifier(node) && isReference(node)) {
        const className = errorClass(checker.getTypeAtLocation(node));
        if (className !== undefined) {
          const { user, child } = userOf(node);
          let use: string;
          let fails: boolean;
          if (ts.isPropertyAccessExpression(user) && user.expression === child) {
            const field = user.name.text;
            const next = userOf(user);
            if (ts.isCallExpression(next.user) && next.user.expression === next.child) {
              [use, fails] = [`method-called whole: ${field}()`, true];
            } else if (TEXT_FIELDS[field] !== true) {
              [use, fails] = [`field read: ${field}`, false];
            } else {
              const [how, failing] = wholeUse(next.user, next.child);
              // A field's words bound to a local are a string this listing does not follow.
              const unfollowed = how.startsWith("bound to a local");
              [use, fails] =
                failing || unfollowed
                  ? [
                      `field ${field} used outside a comparison or the box (${unfollowed ? "bound to a local this listing does not follow" : how})`,
                      true,
                    ]
                  : [`field ${field} ${how}`, false];
            }
          } else {
            [use, fails] = wholeUse(user, child);
          }
          const at = source.getLineAndCharacterOfPosition(node.getStart(source));
          uses.push({
            file: relative(root, name),
            line: at.line + 1,
            column: at.character + 1,
            className,
            use,
            fails,
            text: user.getText(source).replace(/\s+/g, " ").slice(0, 120),
          });
        }
      }
      if (
        importsOpenidClient &&
        ts.isPropertyAssignment(node) &&
        node.name.getText(source) === "cause" &&
        (checker.getTypeAtLocation(node.initializer).flags &
          (ts.TypeFlags.Unknown | ts.TypeFlags.Any)) !==
          0
      ) {
        const at = source.getLineAndCharacterOfPosition(node.getStart(source));
        blind.push(
          `BLIND\t${relative(root, name)}:${at.line + 1}:${at.character + 1}\tan untyped value stored whole as cause, which can be an openid-client error\t${node.getText(source)}`
        );
      }
      ts.forEachChild(node, visit);
    };
    visit(source);
  }
}

uses.sort((a, b) =>
  a.file === b.file ? a.line - b.line || a.column - b.column : a.file < b.file ? -1 : 1
);
let failed = 0;
for (const u of uses) {
  if (u.fails) failed++;
  console.log(
    `${u.fails ? "FAIL" : "ok"}\t${u.file}:${u.line}:${u.column}\t${u.className}\t${u.use}\t${u.text}`
  );
}
for (const line of blind) console.log(line);
console.log(
  `SUMMARY\tprojects=${configs.length}\tproduction files=${files}\tuses=${uses.length}\tfailures=${failed}\tblind=${blind.length}`
);
if (files === 0) {
  console.log("UNKNOWN: no production file was read");
  process.exit(2);
}
process.exit(failed > 0 ? 1 : 0);
