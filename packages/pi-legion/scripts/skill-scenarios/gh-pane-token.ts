// Whether the Legion gh wrapper of a run's label runs a call naming only owners outside the
// world's project on the pane's own token rather than refusing it, as main's wrapper since #4035
// (cac1693d) does for a call that writes nothing (gh.go's ghTargetOf, `fallback`; gh_selectors.go's
// dir; R4 of dispatch://INFRA-3399), read by that wrapper's own read grammar (gh_reads.go's
// ghWritesNothing and ghGraphQLQueryRead), which daemon-pane.ts's `labelGhSelectors` carries in
// the run's selectors (gh-selectors.ts's `paneToken`) where the label's source holds that shape.
// The gh stand-in lets such a call go on to the world's GitHub (gh-standin.ts), and health reads
// it the same way (health.ts's rule 7). It is a module of its own, apart from gh-selectors.ts, so
// a boot's reading of the selectors (boot-refusal.ts) takes in no reader of a call's verb.
import { FIELD_FLAGS, FILE_FIELD_FLAGS, methodsOf } from "./gh-call";
import { type GhSelectors, ghCallOf, type ReadGrammar } from "./gh-selectors";

/** Whether the label's wrapper runs the call `argv` under `env` with the pane's own GH_CONFIG_DIR
 * where it would refuse it for its owner (gh-selectors.ts's `ownerRefusal`): `selectors` carry the
 * read grammar of a wrapper of that shape (`paneToken`), the pane names a GH_CONFIG_DIR, the call
 * writes nothing (`writesNothing`, or a GraphQL query `graphqlQueryRead` admits), it names one
 * owner or more and every one is outside the project of the world's repository `repo`, which
 * holds a gh directory for its own owner alone (gh_selectors.go's dir: every item an
 * OwnerNotServedError), and the wrapper read no working-directory remote for it, which in a world
 * names the world's repository (`ghCallOf`). The call then goes on to GitHub on the pane's token,
 * and gets whatever the world's GitHub answers. A wrapper of the older shape refuses every such
 * call. */
export function runsOnPaneToken(
  argv: readonly string[],
  selectors: GhSelectors,
  repo: string,
  env: Readonly<Record<string, string | undefined>>
): boolean {
  const grammar = selectors.paneToken;
  if (grammar === undefined || (env.GH_CONFIG_DIR ?? "") === "") return false;
  const read = ghCallOf(argv, selectors, env);
  if (read === undefined || read.remotes || read.target.owners.length === 0) return false;
  const world = (repo.split("/")[0] ?? "").toLowerCase();
  if (read.target.owners.some(([owner]) => owner === world)) return false;
  return writesNothing(argv, read.path, grammar) || graphqlQueryRead(argv, grammar);
}

/** gh_reads.go's ghWritesNothing: `argv`, as typed, begins with the path its words resolved to,
 * spelled as that path (no alias, no option before it), and the words after it are in the read
 * grammar of that leaf of ghReadLeaves, or of `gh api` (`readGrammar`). */
function writesNothing(
  argv: readonly string[],
  leaf: readonly string[],
  grammar: ReadGrammar
): boolean {
  if (argv.length < leaf.length || leaf.some((word, i) => argv[i] !== word)) return false;
  const rest = argv.slice(leaf.length);
  if (leaf.join(" ") === "api") return readGrammar(rest, grammar.api, grammar);
  const options = grammar.leaves[leaf.join(" ")];
  return options !== undefined && readGrammar(rest, options);
}

/** gh_reads.go's ghReadGrammar: every word that begins with `-` is one of `options`' spellings
 * whole, a value's in the next word, which is not empty and does not begin with `-`; every other
 * word is an argument. For `gh api` (`api`, its grammar) there is exactly one argument, a relative
 * REST path (`restEndpoint`), every method it names exactly GET (gh-call.ts's `methodsOf`) and a
 * header no method override. */
function readGrammar(
  args: readonly string[],
  options: Readonly<Record<string, boolean>>,
  api?: ReadGrammar
): boolean {
  let argumentsSeen = 0;
  for (let i = 0; i < args.length; i++) {
    const word = args[i] ?? "";
    if (!word.startsWith("-")) {
      argumentsSeen++;
      if (api !== undefined && (argumentsSeen > 1 || word === "" || !restEndpoint(word)))
        return false;
      continue;
    }
    if (!Object.hasOwn(options, word)) return false;
    if (options[word] === true) {
      const value = args[++i];
      if (value === undefined || value === "" || value.startsWith("-")) return false;
      if (
        api !== undefined &&
        HEADER_FLAGS.includes(word) &&
        api.overrides.includes(headerName(value))
      )
        return false;
    }
  }
  if (api === undefined) return true;
  return argumentsSeen === 1 && methodsOf(args).every((method) => method === "GET");
}

/** The spellings of gh api's request header option. */
const HEADER_FLAGS = ["-H", "--header"];

/** gh_reads.go's ghHeaderName: the name a header option's value sets, trimmed and lowercased. */
const headerName = (value: string) => (value.split(":")[0] ?? "").trim().toLowerCase();

/** gh_reads.go's ghAPIRESTEndpoint: no whole URL, and a path that, read to its query or fragment
 * and its escapes decoded, has no segment `graphql` in any case, `.` or `..`. */
function restEndpoint(endpoint: string): boolean {
  if (endpoint.includes("://")) return false;
  const end = endpoint.search(/[?#]/);
  let decoded: string;
  try {
    decoded = decodeURIComponent(end < 0 ? endpoint : endpoint.slice(0, end));
  } catch {
    return false;
  }
  return decoded
    .split("/")
    .every((segment) => segment !== "." && segment !== ".." && segment.toLowerCase() !== "graphql");
}

/** gh_reads.go's ghGraphQLQueryRead: `api graphql`, then only options of the grammar's GraphQL
 * query, each value in the next word; exactly one raw field keyed `query` and no other key holding
 * the word, no typed field (gh-call.ts's FILE_FIELD_FLAGS) whose value begins with `@`, no header
 * that overrides the method or sets Content-Length, and a query whose text holds neither
 * `mutation` nor `subscription` in any case. */
function graphqlQueryRead(argv: readonly string[], grammar: ReadGrammar): boolean {
  if (argv[0] !== "api" || argv[1] !== "graphql") return false;
  let query = "";
  let queries = 0;
  for (let i = 2; i < argv.length; i++) {
    const option = argv[i] ?? "";
    if (!Object.hasOwn(grammar.graphql, option)) return false;
    if (grammar.graphql[option] !== true) continue;
    const value = argv[++i];
    if (value === undefined) return false;
    if (FIELD_FLAGS.includes(option)) {
      const equals = value.indexOf("=");
      const typed = FILE_FIELD_FLAGS.includes(option);
      const key = equals < 0 ? value : value.slice(0, equals);
      const text = equals < 0 ? "" : value.slice(equals + 1);
      if (equals < 0 || (typed && text.startsWith("@"))) return false;
      if (key === "query" && !typed) {
        query = text;
        queries++;
      } else if (key.includes("query")) return false;
    } else if (HEADER_FLAGS.includes(option)) {
      const name = headerName(value);
      if (grammar.overrides.includes(name) || name === "content-length") return false;
    }
  }
  const lower = query.toLowerCase();
  return queries === 1 && !lower.includes("mutation") && !lower.includes("subscription");
}
