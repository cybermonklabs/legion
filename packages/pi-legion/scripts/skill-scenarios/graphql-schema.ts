// GitHub's GraphQL schema as far as the world's graph answers it: for each type of an object the
// world's graph holds (WORLD_TYPES), every field GitHub's schema gives that type and the type the
// field returns, read off GitHub's own introspection and never written by hand
// (graphql-schema.json). GitHub validates a query against its schema before it runs any of it,
// and refuses a query that selects a field its type has not got: measured on 2026-11-17 from a
// Legion issue pod, `gh api graphql -f
// query='query{repository(owner:"<owner>",name:"<repo>"){pullRequest(number:<n>){number bogus}}}'`
// printed `{"errors":[{"path":["query","repository","pullRequest","bogus"],"extensions":{"code":
// "undefinedField","typeName":"PullRequest","fieldName":"bogus"},"locations":[{"line":1,
// "column":87}],"message":"Field 'bogus' doesn't exist on type 'PullRequest'"}]}`, no data, and
// exited 1, where the world answered the field null beside the rest of the query
// (gh-routes.ts's `undefinedFields` reads a query against this).
//
//   bun graphql-schema.ts <out json>
//
// reads each of WORLD_TYPES from GitHub with the pod's own gh (`gh api graphql` introspection, two
// types a call, the most GitHub lets one query introspect), then formats the file as the
// repository's biome does. The file is imported, not read at run time: the gh stand-in runs as a
// bundle under the run's own directory (rig.sh's standins), which holds none of this directory's
// files, and the bundle carries what it imports.
import { writeFileSync } from "node:fs";
import schema from "./graphql-schema.json";

/** The types of the objects the world's graph holds (rig.sh's fixture_write and the gh stand-in's
 * `graphAnswer`): the two roots, the repository and its owner, the pull request with its review
 * threads, reviews, conversation, files and commits, each connection and edge GitHub pages them
 * in, the accounts that author them, and the payloads of the two thread mutations. A field whose
 * type is none of these is read no further, so nothing below it is refused. */
export const WORLD_TYPES = [
  "Query",
  "Mutation",
  "Repository",
  "RepositoryOwner",
  "Organization",
  "Ref",
  "PullRequest",
  "PullRequestReviewThreadConnection",
  "PullRequestReviewThreadEdge",
  "PullRequestReviewThread",
  "PullRequestReviewCommentConnection",
  "PullRequestReviewCommentEdge",
  "PullRequestReviewComment",
  "PullRequestReviewConnection",
  "PullRequestReviewEdge",
  "PullRequestReview",
  "IssueCommentConnection",
  "IssueCommentEdge",
  "IssueComment",
  "PageInfo",
  "Commit",
  "User",
  "Bot",
  "Actor",
  "PullRequestChangedFileConnection",
  "PullRequestChangedFileEdge",
  "PullRequestChangedFile",
  "PullRequestTimelineItemsConnection",
  "PullRequestTimelineItemsEdge",
  "ResolveReviewThreadPayload",
  "AddPullRequestReviewThreadReplyPayload",
  "MergeQueueEntry",
  "PullRequestCommitConnection",
  "PullRequestCommitEdge",
  "PullRequestCommit",
];

/** Each type's fields, each by the named type it returns (a list's or a non-null's own type). */
export type SchemaTypes = Readonly<Record<string, Readonly<Record<string, string>>>>;
/** The schema this directory keeps (graphql-schema.json). */
export const SCHEMA_TYPES: SchemaTypes = schema.types;

/** An introspection `__Type` as `derive` reads one. */
interface IntrospectedType {
  readonly name: string;
  readonly kind: string;
  readonly fields: readonly { name: string; type: Wrapped }[] | null;
}
interface Wrapped {
  readonly name: string | null;
  readonly ofType?: Wrapped | null;
}
/** The type `type` names under any list and non-null that wrap it. */
function named(type: Wrapped): string {
  return type.name ?? (type.ofType ? named(type.ofType) : "");
}

/** The schema of `introspected`, GitHub's `__Type` of each of WORLD_TYPES. A type of WORLD_TYPES
 * GitHub did not answer, or one with no fields, fails the derivation, naming it. */
function derive(introspected: readonly IntrospectedType[]): SchemaTypes {
  return Object.fromEntries(
    WORLD_TYPES.map((name) => {
      const fields = introspected.find((type) => type.name === name)?.fields ?? [];
      if (fields.length === 0) throw new Error(`GitHub's schema answered no fields of ${name}`);
      return [name, Object.fromEntries(fields.map((field) => [field.name, named(field.type)]))];
    })
  );
}

if (import.meta.main) {
  const [out, ...rest] = Bun.argv.slice(2);
  if (out === undefined || rest.length > 0) {
    console.error("usage: bun graphql-schema.ts <out json>");
    process.exit(2);
  }
  const read_at = new Date().toISOString().replace(/\.\d{3}Z$/, "Z");
  const introspected: IntrospectedType[] = [];
  for (let at = 0; at < WORLD_TYPES.length; at += 2) {
    // One `__type` read for each of two types, the most GitHub lets one query introspect.
    const reads = WORLD_TYPES.slice(at, at + 2).map(
      (name) =>
        `t${name}:__type(name:"${name}"){name kind fields(includeDeprecated:true){name type{name ofType{name ofType{name ofType{name}}}}}}`
    );
    const asked = Bun.spawnSync(["gh", "api", "graphql", "-f", `query=query{${reads.join(" ")}}`]);
    if (asked.exitCode !== 0) {
      console.error(asked.stderr.toString());
      process.exit(1);
    }
    const answer = JSON.parse(asked.stdout.toString()) as {
      data: Record<string, IntrospectedType>;
    };
    introspected.push(...Object.values(answer.data));
  }
  const schema = {
    made_by:
      "graphql-schema.ts (bun graphql-schema.ts <out json>), from GitHub's GraphQL introspection of each of its WORLD_TYPES, read from a Legion issue pod with the role App token its gh gives",
    read_at,
    types: derive(introspected),
  };
  writeFileSync(out, `${JSON.stringify(schema, null, 2)}\n`);
  const formatted = Bun.spawnSync(["bunx", "biome", "format", "--write", out], {
    stdout: "inherit",
    stderr: "inherit",
  });
  process.exit(formatted.exitCode);
}
