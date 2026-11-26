import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";

/**
 * The hosts of the shared pick, and the rules each opts into (dispatch://INFRA-2160's table "The
 * hosts of the shared pick, each with the rules it opts into and its test rows").
 *
 * `MultiSelect` takes two rules a host opts into, each as one prop: D5's `menu`, the rules of a
 * list that stands in a menu's place (R4 and R5: the snooze, the priority and the status, which a
 * key opens and whose every pick is a write, `ListMenu`), and D2's `keysOnTrigger`, the keys of a
 * `<select>` on the closed trigger (R6: the assignee, the repository and an ask's hand-on). Since
 * one prop carries each rule set whole, a host takes every rule of it or none. What a host does
 * with its rules is held by the rows each host names below and by `MultiSelect.test.tsx`; this
 * file holds who takes which. It reads every file of `web/src` other than the tests (a
 * `.test.ts` or `.test.tsx`, and the helpers under `__tests__/`), finds each `<MultiSelect>` the
 * file draws - parsed, so a comment or a string that names the element counts nothing, an
 * element the formatter breaks across lines counts once, and an import under another name is
 * followed - and fails:
 *
 * - for a file that draws one and that the table does not name;
 * - for a host the table names that draws a number of them other than the table's, none
 *   included: a status pick moved into a shared file of its own is named here by that file;
 * - for an element of a host that takes a rule and does not pass its prop by name, so a file
 *   that draws an R4 pick draws no other `MultiSelect`;
 * - for an element of any other file that passes it.
 *
 * A prop passed in a spread (`{...filterPanel("labels")}`) is not read: a rule is passed by its
 * name, where a reader of the host sees it.
 */

const WEB_SRC = join(import.meta.dir, "..");

/** An opt-in rule of the shared pick, by its prop. */
type Rule = "keysOnTrigger" | "menu";

const RULES: readonly Rule[] = ["menu", "keysOnTrigger"];

/** What each rule is, as a failure names it. */
const RULE_NAMES: Readonly<Record<Rule, string>> = {
  keysOnTrigger: "D2's closed-trigger keys (`keysOnTrigger`)",
  menu: "D5's rules (`menu`)",
};

/** One host of the shared pick: a file that draws it. */
interface Host {
  /** The file, under `web/src`. */
  readonly file: string;
  /** Where the reader meets its lists. */
  readonly surface: string;
  /** How many `<MultiSelect>` elements the file draws. */
  readonly elements: number;
  /** The rules every element of the file passes, and no element of a file not given them. */
  readonly rules: readonly Rule[];
  /** What D5's digits are, for a host that takes D5's rules. */
  readonly digits?: string;
  /** The rows that hold what the host does with its rules. */
  readonly rows: string;
}

/** The R4 hosts' rows: the browser checks their lists are read by, and the unit rows of the table
 *  "Where the focus goes back, by opener". */
const MENU_ROWS =
  'k-menu-contrast, and the rows of "Where the focus goes back, by opener" in `inbox/SnoozeControl.test.tsx`, `issue/PriorityControl.test.tsx` and `issue/IssuePage.test.tsx`';

/** INFRA-3135's lists, which take neither rule. */
const THEIRS = "INFRA-3135's own rows, `e2e/keyboard-agents-picker.e2e.ts`";

/** Every host, as the table "The hosts of the shared pick" names it. */
const HOSTS: readonly Host[] = [
  {
    digits: "`1`-`4`, and `0` on a snoozed item",
    elements: 1,
    file: "features/inbox/SnoozeControl.tsx",
    rows: `k-snooze-menu (the reading pane, the short-row list, the reading view, the drawer, the bar), k-next-item (bulk), k-phone, ${MENU_ROWS}`,
    rules: ["menu"],
    surface:
      "`SnoozePick`: the pane head's Snooze, a card's Snooze, the drawer's, and the selection bar's Snooze selected asks, which `inbox/Inbox.tsx` draws as `<SnoozePick>`",
  },
  {
    digits: "`0`-`3`",
    elements: 1,
    file: "features/issue/PriorityControl.tsx",
    rows: `k-priority-menu (each surface), k-phone, ${MENU_ROWS}`,
    rules: ["menu"],
    surface: "the priority: the Inbox, today's issue header, the Board's cards, the issues table",
  },
  {
    digits: "none",
    elements: 1,
    file: "features/issue/IssueHeader.tsx",
    rows: `k-status-menu, ${MENU_ROWS}`,
    rules: ["menu"],
    surface: "the status of today's issue page",
  },
  {
    digits: "none",
    elements: 1,
    file: "features/issue-page/IssuePropertiesRow.tsx",
    rows: `k-status-menu, ${MENU_ROWS}`,
    rules: ["menu"],
    surface: "the status of the new issue page's properties row",
  },
  {
    elements: 1,
    file: "features/issue/AssigneeControl.tsx",
    rows: "k-choice-writes-on-enter, k-menu-contrast",
    rules: ["keysOnTrigger"],
    surface: "the header's assignee",
  },
  {
    elements: 1,
    file: "features/issue/RepoControl.tsx",
    rows: "k-choice-writes-on-enter, k-menu-contrast",
    rules: ["keysOnTrigger"],
    surface: "the header's repository",
  },
  {
    elements: 1,
    file: "features/inbox/AskHandOn.tsx",
    rows: "k-choice-writes-on-enter, k-menu-contrast",
    rules: ["keysOnTrigger"],
    surface: "an ask's hand-on",
  },
  {
    elements: 2,
    file: "features/inbox/Inbox.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the Inbox's filter fields and its Assignee filter",
  },
  {
    elements: 1,
    file: "components/ColumnFilter.tsx",
    rows: THEIRS,
    rules: [],
    surface: "a table column's filter",
  },
  { elements: 1, file: "components/Pager.tsx", rows: THEIRS, rules: [], surface: "Rows per page" },
  {
    elements: 1,
    file: "features/agents/AgentMessageComposer.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the Agents composer's Issue list",
  },
  {
    elements: 3,
    file: "features/agents/AgentsPage.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the Agents page's filters",
  },
  {
    elements: 1,
    file: "features/architecture/ComponentPicker.tsx",
    rows: THEIRS,
    rules: [],
    surface: "an issue's components",
  },
  {
    elements: 1,
    file: "features/artifacts/ArtifactDetails.tsx",
    rows: THEIRS,
    rules: [],
    surface: "a document's details",
  },
  {
    elements: 1,
    file: "features/artifacts/ArtifactHeader.tsx",
    rows: THEIRS,
    rules: [],
    surface: "a document's Version list",
  },
  {
    elements: 1,
    file: "features/conversation/DeliveryModePick.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the Delivery mode of the live agent view and of the Agents page's broadcast",
  },
  {
    elements: 3,
    file: "features/delivery/DeliveryPage.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the Delivery page's filters",
  },
  {
    elements: 1,
    file: "features/delivery/WindowControl.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the Delivery page's window",
  },
  {
    elements: 1,
    file: "features/issue/CreateIssueDialog.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the new issue dialog",
  },
  {
    elements: 1,
    file: "features/issue/IssueLabels.tsx",
    rows: THEIRS,
    rules: [],
    surface: "an issue's labels",
  },
  {
    elements: 1,
    file: "features/issue/SpecToolbar.tsx",
    rows: THEIRS,
    rules: [],
    surface: "a spec's toolbar",
  },
  {
    elements: 1,
    file: "features/priorities/IssuePriorityLine.tsx",
    rows: THEIRS,
    rules: [],
    surface: "a priority's issue line",
  },
  {
    elements: 1,
    file: "features/project/IssueBoard.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the Board's lists",
  },
  {
    elements: 1,
    file: "features/project/issues-table-page.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the issues table's page",
  },
  {
    elements: 1,
    file: "features/project/issues-table-rows.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the issues table's rows",
  },
  {
    elements: 2,
    file: "features/search/SearchPalette.tsx",
    rows: THEIRS,
    rules: [],
    surface: "the search bar's Project and Status lists",
  },
  {
    elements: 1,
    file: "features/settings/ProjectSelect.tsx",
    rows: THEIRS,
    rules: [],
    surface: "Settings' project",
  },
  {
    elements: 1,
    file: "features/settings/ProjectsSection.tsx",
    rows: THEIRS,
    rules: [],
    surface: "Settings' projects",
  },
];

/** One `<MultiSelect>` a file draws: the line it starts on, and the props it passes by name. */
interface Drawn {
  readonly line: number;
  readonly props: ReadonlySet<string>;
}

/** Whether `name` is a file of `web/src` the inventory reads: source, not a test. */
function isSource(name: string): boolean {
  return (
    (name.endsWith(".ts") || name.endsWith(".tsx")) &&
    !name.endsWith(".d.ts") &&
    !name.endsWith(".test.ts") &&
    !name.endsWith(".test.tsx")
  );
}

/** Every source file under `dir`, by its path under `web/src`, the test helpers' directory left
 *  out. */
function sourceFiles(dir: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== "__tests__" && entry.name !== "node_modules") {
        files.push(...sourceFiles(path));
      }
    } else if (isSource(entry.name)) {
      files.push(path.slice(WEB_SRC.length + 1));
    }
  }
  return files;
}

/** The names a file binds the shared pick to: each import of `MultiSelect` from its module. */
function localNames(source: ts.SourceFile): Set<string> {
  const names = new Set<string>();
  for (const statement of source.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier)) {
      continue;
    }
    if (!/(?:^|\/)MultiSelect(?:\.tsx?)?$/.test(statement.moduleSpecifier.text)) continue;
    const bindings = statement.importClause?.namedBindings;
    if (bindings === undefined || !ts.isNamedImports(bindings)) continue;
    for (const element of bindings.elements) {
      if ((element.propertyName ?? element.name).text === "MultiSelect") {
        names.add(element.name.text);
      }
    }
  }
  return names;
}

/** Each `<MultiSelect>` element of the file `path` holding `text`, in the order it draws them. */
function drawnIn(path: string, text: string): Drawn[] {
  const source = ts.createSourceFile(
    path,
    text,
    ts.ScriptTarget.Latest,
    true,
    path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS
  );
  const names = localNames(source);
  const drawn: Drawn[] = [];
  if (names.size === 0) return drawn;
  const visit = (node: ts.Node) => {
    if (
      (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) &&
      names.has(node.tagName.getText(source))
    ) {
      const props = new Set<string>();
      for (const attribute of node.attributes.properties) {
        if (ts.isJsxAttribute(attribute)) props.add(attribute.name.getText(source));
      }
      drawn.push({
        line: source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1,
        props,
      });
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return drawn;
}

/** Every source file of `web/src`, each with the `<MultiSelect>` elements it draws. */
const FILES = sourceFiles(WEB_SRC).map((file) => {
  const text = readFileSync(join(WEB_SRC, file), "utf8");
  return [file, text.includes("MultiSelect") ? drawnIn(file, text) : []] as const;
});

const DRAWN = new Map(FILES);

/** What is wrong with `host` as its file draws it, each said at the line it is found at. */
function problemsOf(host: Host, drawn: readonly Drawn[]): string[] {
  const problems: string[] = [];
  if (drawn.length !== host.elements) {
    problems.push(
      `${host.file} draws ${drawn.length} <MultiSelect>, where the table names ${host.elements}: ${host.surface}`
    );
  }
  for (const element of drawn) {
    for (const rule of RULES) {
      const takes = host.rules.includes(rule);
      if (takes && !element.props.has(rule)) {
        const digits =
          rule === "menu" && host.digits !== undefined ? `, digits ${host.digits}` : "";
        problems.push(
          `${host.file}:${element.line} draws a <MultiSelect> that passes no \`${rule}\`, where every list of the file takes ${RULE_NAMES[rule]}${digits}, held by ${host.rows}`
        );
      } else if (!takes && element.props.has(rule)) {
        problems.push(
          `${host.file}:${element.line} passes \`${rule}\`, where the table gives no list of the file ${RULE_NAMES[rule]}`
        );
      }
    }
  }
  return problems;
}

describe("the hosts of the shared pick, each with the rules it opts into", () => {
  // A guard on the walk itself: a walk that found nothing would let every row below pass while
  // reading no host at all.
  test("the walk reads every source file of web/src", () => {
    expect(FILES.length).toBeGreaterThan(500);
    expect(FILES.filter(([, drawn]) => drawn.length > 0).length).toBeGreaterThan(20);
  });

  // Each form below is one a host writes, and the reader has to read through it: an element the
  // formatter breaks across lines, the element's name in a comment or a string, a prop passed
  // bare or with a value, one passed in a spread, and the pick imported under a name of its own.
  test("the reader counts each element once, wherever its lines break, and reads its props by name", () => {
    const source = [
      'import { MultiSelect } from "../../components/MultiSelect";',
      "",
      "// A <MultiSelect> named in a comment draws nothing.",
      'const words = "<MultiSelect />";',
      "export const Probe = () => (",
      "  <>",
      "    <MultiSelect",
      "      keysOnTrigger",
      '      menu={{ keys: { "1": "a" }, opensOn: "first" }}',
      '      mode="single"',
      "    />",
      "    <MultiSelect {...spread} label={words} />",
      "  </>",
      ");",
    ].join("\n");
    expect(
      drawnIn("features/inbox/Probe.tsx", source).map(({ line, props }) => [line, [...props]])
    ).toEqual([
      [7, ["keysOnTrigger", "menu", "mode"]],
      [12, ["label"]],
    ]);
    const renamed = [
      'import { MultiSelect as Pick } from "./MultiSelect";',
      "export const Probe = () => <Pick menu={menu}>x</Pick>;",
    ].join("\n");
    expect(drawnIn("components/Probe.tsx", renamed).map(({ props }) => [...props])).toEqual([
      ["menu"],
    ]);
  });

  test("the table names each host once", () => {
    const files = HOSTS.map(({ file }) => file);
    expect(files.filter((file, index) => files.indexOf(file) !== index)).toEqual([]);
  });

  test("every file of web/src that draws a <MultiSelect> is a host the table names", () => {
    const named = new Set(HOSTS.map(({ file }) => file));
    expect(
      FILES.filter(([file, drawn]) => drawn.length > 0 && !named.has(file)).map(
        ([file, drawn]) =>
          `${file} draws ${drawn.length} <MultiSelect>, and the table names no host there: name it, with the rules it opts into`
      )
    ).toEqual([]);
  });

  // One row of the table each: the failure names each line of the host that breaks it.
  test.each(
    HOSTS.map((host) => {
      const rules =
        host.rules.length === 0
          ? "with neither rule"
          : `each with ${host.rules.map((rule) => RULE_NAMES[rule]).join(" and ")}`;
      return [`${host.file} draws ${host.elements} <MultiSelect>, ${rules}`, host] as const;
    })
  )("%s", (_name, host) => {
    expect(problemsOf(host, DRAWN.get(host.file) ?? [])).toEqual([]);
  });
});
