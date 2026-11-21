import { describe, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { callWrites, GH_COMMANDS, graphqlOperations, graphqlWrites, pinnedTag } from "./gh-writes";

// The rows run the rig's programs as real processes, which a loaded pod slows past bun's 5 s
// default, so each gets a minute, as rig.test.ts's do.
setDefaultTimeout(60_000);

/** gh's own help, recorded from the gh the worker image pins (the Legion wrapper's testdata). */
const GH_HELP = path.join(
  import.meta.dir,
  "..",
  "..",
  "..",
  "daemon",
  "cmd",
  "legion",
  "testdata",
  "gh-help"
);

describe("gh-writes: gh's commands at the pinned tag", () => {
  // Catches: a bump of the worker image's gh with gh-writes.json left at the old tag, so rule 20
  // reads a call by the commands of a gh no pane runs.
  test("the extract is gh's at the tag the worker image pins", () => {
    expect(GH_COMMANDS.tag).toBe(pinnedTag());
  });

  // gh's own `gh reference`, recorded from that gh's binary, lists every command gh does not hide,
  // and two words of the recording's own: `co`, an alias gh's fresh configuration sets, and
  // `reference`, a help topic, both added at run time. Catches: a derivation that leaves a group
  // out, as a walk of `pkg/cmd/<group>/<command>` directories left out the codespace, label and
  // extension groups, `pr lock` and `pr unlock` and every command of the root's own (gh 2.98.0:
  // 165 commands read there, 207 run).
  test("every command gh's own reference lists is one the extract holds, and no more", () => {
    expect(readFileSync(path.join(GH_HELP, "VERSION"), "utf8")).toStartWith(
      `gh version ${GH_COMMANDS.tag.slice(1)} `
    );
    const listed = new Set<string>();
    for (const line of readFileSync(path.join(GH_HELP, "reference.txt"), "utf8").split("\n")) {
      const heading = /^#{2,} gh (.*)$/.exec(line)?.[1];
      if (heading === undefined) continue;
      const words: string[] = [];
      for (const word of heading.split(" ")) {
        if (!/^[a-z][a-z0-9-]*$/.test(word)) break;
        words.push(word);
      }
      listed.add(words.join(" "));
    }
    listed.delete("co");
    listed.delete("reference");
    const shown = Object.entries(GH_COMMANDS.commands)
      .filter(([, command]) => command.hidden !== true)
      .map(([name]) => name);
    expect(shown.sort()).toEqual([...listed].sort());
  });

  // The architect's ruling of 2026-11-18T15:08Z on the auth commands. Catches: a reading of the
  // OAuth flow as no write, and one of a command that changes only the pane's own configuration
  // as one.
  test("auth login and auth refresh write, and auth logout, switch and setup-git do not", () => {
    const auth = ["login", "refresh", "logout", "switch", "setup-git", "token", "status"].map(
      (command) => [command, GH_COMMANDS.commands[`auth ${command}`]?.writes]
    );
    expect(auth).toEqual([
      ["login", true],
      ["refresh", true],
      ["logout", false],
      ["switch", false],
      ["setup-git", false],
      ["token", false],
      ["status", false],
    ]);
  });

  // A POST to a service of GitHub's the world does not model is a write the world cannot fake (the
  // implementer's ruling of 2026-11-18, by the architect's 15:08Z one on `auth login`): gh's
  // telemetry client posts to https://cafe.github.com (pkg/cmd/send-telemetry/send_telemetry.go:21
  // at v2.98.0). Catches: a derivation that reads the telemetry client as writing nothing.
  test("send-telemetry writes, its POST going to a service of GitHub's", () => {
    expect(GH_COMMANDS.commands["send-telemetry"]).toEqual({
      file: "pkg/cmd/send-telemetry/send_telemetry.go",
      hidden: true,
      runs: true,
      writes: true,
      write:
        "internal/barista/observability/telemetry.twirp.go:700 an HTTP POST, reached by newCmdSendTelemetry → runSendTelemetry → telemetryAPIProtobufClient.RecordEvents → telemetryAPIProtobufClient.callRecordEvents → doProtobufRequest → newRequest",
    });
  });
});

describe("gh-writes: a call's command, as cobra finds it", () => {
  // Catches: a reading of the first two words alone, which read `gh pr -R o/r merge` and
  // `gh cs delete` (a group by its alias) as no write and `gh pr ls` (an alias of a read) as
  // one; a group that runs on its own read by its subcommands; and a word after `--`, which
  // cobra reads as no command, read as one.
  test("each word names a command by its name or an alias, past the options before it", () => {
    const rows: [argv: string[], writes: boolean][] = [
      [["pr", "merge", "1"], true],
      [["pr", "ls"], false],
      [["pr", "checkout", "1"], false],
      [["pr", "-R", "o/r", "merge", "1"], true],
      [["pr", "--repo=o/r", "merge"], true],
      [["pr", "--draft", "merge"], true],
      [["cs", "delete"], true],
      [["skills", "publish"], true],
      [["codespace", "ports"], true],
      [["agent-task", "list"], false],
      [["agent", "create", "a task"], true],
      [["issue", "frobnicate"], false],
      [["pr"], false],
      [["co", "1"], false],
      [["--", "pr", "merge"], false],
      [["pr", "--", "merge"], false],
    ];
    expect(rows.map(([argv]) => [argv, callWrites({ argv })])).toEqual(rows);
  });

  // The architect's ruling of 2026-11-18: `gh api` is no write by its name. A call of it writes as
  // the request it names does: by the method gh sends, GET unless a field or `--input` makes it
  // a POST or `-X`/`--method` names one, and for GraphQL by the operation its document runs.
  // Catches: `gh api` read as a write by name, which read every GET as one; and a method or a
  // body left unread, which read a POST of a comment as no write.
  test("gh api writes by the request it sends, never by its name", () => {
    const mutation = 'query=mutation { addStar(input: {starrableId: "x"}) { clientMutationId } }';
    const rows: [argv: string[], writes: boolean][] = [
      [["api", "repos/o/r/pulls/1"], false],
      [["api", "-f", "body=x", "repos/o/r/issues/1/comments"], true],
      [["api", "-X", "PUT", "repos/o/r/pulls/1/merge"], true],
      [["api", "--method=delete", "repos/o/r/git/refs/heads/x"], true],
      [["api", "-X", "GET", "-f", "q=is:open", "search/issues"], false],
      [["api", "--input", "body.json", "repos/o/r/issues"], true],
      [["api", "-X", "HEAD", "repos/o/r"], false],
      [["api", "graphql", "-f", "query={ viewer { login } }"], false],
      [["api", "graphql", "-f", mutation], true],
    ];
    expect(GH_COMMANDS.commands.api).toMatchObject({ byRequest: true, writes: false });
    expect(rows.map(([argv]) => [argv, callWrites({ argv })])).toEqual(rows);
  });
});

describe("gh-writes: a GraphQL document's own operation", () => {
  const two =
    'query Q { viewer { login } } mutation M { addStar(input: {starrableId: "x"}) { clientMutationId } }';

  // Catches: a `mutation` read anywhere in the text — in a comment, a string or a block string,
  // or as a name inside an argument or a selection — and a fragment read as an operation; and a
  // string's `}` read as a bracket, which ends the operation early.
  test("the operations are read by the document's own grammar", () => {
    const rows: [text: string, operations: { type: string; name?: string }[]][] = [
      [
        two,
        [
          { type: "query", name: "Q" },
          { type: "mutation", name: "M" },
        ],
      ],
      ["{ viewer { login } }", [{ type: "query" }]],
      ["# mutation M { x }\nquery { viewer { login } }", [{ type: "query" }]],
      ['query { search(query: "x } mutation {") { issueCount } }', [{ type: "query" }]],
      ['"""a } mutation { block"""\nquery { viewer { login } }', [{ type: "query" }]],
      ["query { mutation: viewer { login } }", [{ type: "query" }]],
      ["query Q($at: Input = {a: 1}) { viewer { login } }", [{ type: "query", name: "Q" }]],
      [
        'mutation($b: String = "}") { addComment(input: {body: $b}) { x } }',
        [{ type: "mutation" }],
      ],
      ["fragment F on Query { viewer { login } } query { ...F }", [{ type: "query" }]],
    ];
    expect(rows.map(([text]) => [text, graphqlOperations(text)])).toEqual(rows);
  });

  // Catches: a document of several operations read by its first, or by any mutation it holds,
  // rather than by the one GitHub runs (the call's `operationName`, or its only operation).
  test("the operation GitHub runs is the one the call names, or the document's only one", () => {
    const rows: [text: string, operationName: string | undefined, writes: boolean][] = [
      [two, "M", true],
      [two, "Q", false],
      [two, undefined, false],
      [two, "N", false],
      ['mutation { addStar(input: {starrableId: "x"}) { clientMutationId } }', undefined, true],
      ['mutation { addStar(input: {starrableId: "x"}) { clientMutationId } }', "", true],
      ["{ viewer { login } }", undefined, false],
    ];
    expect(rows.map(([text, name]) => [text, name, graphqlWrites(text, name)])).toEqual(rows);
  });
});

/** A source tree shaped as gh's, as far as gh-writes.go reads one: the go.mod naming gh's module
 * and each file of `files`, by its path. */
function ghSource(dir: string, files: Record<string, string>): string {
  writeFileSync(path.join(dir, "go.mod"), "module github.com/cli/cli/v2\n\ngo 1.26\n");
  for (const [file, text] of Object.entries(files)) {
    mkdirSync(path.join(dir, path.dirname(file)), { recursive: true });
    writeFileSync(path.join(dir, file), text);
  }
  return dir;
}

/** gh-writes.go's printout over `source`, run as gh-writes.ts runs it. */
function derive(source: string): { code: number; out: string; err: string } {
  const run = Bun.spawnSync(["go", "run", path.join(import.meta.dir, "gh-writes.go"), source], {
    cwd: import.meta.dir,
    env: { ...process.env, GOWORK: "off" },
  });
  return { code: run.exitCode, out: run.stdout.toString(), err: run.stderr.toString() };
}

const COBRA = '"github.com/spf13/cobra"';
/** Each way gh's source builds a command and sends a write, one command each (gh-writes.go's
 * header says how each is read). */
const FIXTURE: Record<string, string> = {
  "pkg/cmd/root/root.go": `package root

import (
	apiCmd "github.com/cli/cli/v2/pkg/cmd/api"
	authCmd "github.com/cli/cli/v2/pkg/cmd/auth"
	issueCmd "github.com/cli/cli/v2/pkg/cmd/issue"
	orgCmd "github.com/cli/cli/v2/pkg/cmd/org"
	telemetryCmd "github.com/cli/cli/v2/pkg/cmd/send-telemetry"
	${COBRA}
)

func NewCmdRoot(topics []string) *cobra.Command {
	cmd := &cobra.Command{Use: "gh <command> <subcommand> [flags]"}
	cmd.AddCommand(apiCmd.NewCmdApi())
	cmd.AddCommand(authCmd.NewCmdAuth())
	cmd.AddCommand(issueCmd.NewCmdIssue())
	cmd.AddCommand(orgCmd.NewCmdOrg())
	cmd.AddCommand(telemetryCmd.NewCmdSendTelemetry())
	for _, topic := range topics {
		helpTopic := NewCmdHelpTopic(topic)
		cmd.AddCommand(helpTopic)
	}
	return cmd
}

func NewCmdHelpTopic(topic string) *cobra.Command {
	return &cobra.Command{Use: topic}
}
`,
  "pkg/cmd/api/api.go": `package api

import (
	"net/http"

	${COBRA}
)

func NewCmdApi() *cobra.Command {
	return &cobra.Command{Use: "api <endpoint>", RunE: func(c *cobra.Command, a []string) error {
		_, err := http.NewRequest("POST", a[0], nil)
		return err
	}}
}
`,
  "pkg/cmd/auth/auth.go": `package auth

import (
	logoutCmd "github.com/cli/cli/v2/pkg/cmd/auth/logout"
	refreshCmd "github.com/cli/cli/v2/pkg/cmd/auth/refresh"
	${COBRA}
)

func NewCmdAuth() *cobra.Command {
	cmd := &cobra.Command{Use: "auth <command>"}
	cmd.AddCommand(logoutCmd.NewCmdLogout())
	cmd.AddCommand(refreshCmd.NewCmdRefresh())
	return cmd
}
`,
  "pkg/cmd/auth/logout/logout.go": `package logout

import (
	"github.com/cli/cli/v2/internal/config"
	${COBRA}
)

func NewCmdLogout() *cobra.Command {
	return &cobra.Command{Use: "logout", RunE: func(c *cobra.Command, a []string) error {
		return config.New().Delete("github.com")
	}}
}
`,
  "internal/config/config.go": `package config

type Config struct{}

func New() *Config { return &Config{} }

func (c *Config) Delete(host string) error { return nil }
`,
  "pkg/cmd/auth/refresh/refresh.go": `package refresh

import (
	"github.com/cli/cli/v2/internal/authflow"
	${COBRA}
)

func NewCmdRefresh() *cobra.Command {
	return &cobra.Command{Use: "refresh", RunE: func(c *cobra.Command, a []string) error {
		_, err := authflow.AuthFlow("github.com")
		return err
	}}
}
`,
  "internal/authflow/flow.go": `package authflow

import "github.com/cli/oauth"

func AuthFlow(host string) (string, error) {
	flow := &oauth.Flow{Host: host}
	return flow.DetectFlow()
}
`,
  "pkg/cmd/issue/issue.go": `package issue

import (
	"net/http"

	commentCmd "github.com/cli/cli/v2/pkg/cmd/issue/comment"
	createCmd "github.com/cli/cli/v2/pkg/cmd/issue/create"
	deleteCmd "github.com/cli/cli/v2/pkg/cmd/issue/delete"
	developCmd "github.com/cli/cli/v2/pkg/cmd/issue/develop"
	listCmd "github.com/cli/cli/v2/pkg/cmd/issue/list"
	portsCmd "github.com/cli/cli/v2/pkg/cmd/issue/ports"
	"github.com/cli/cli/v2/pkg/cmdutil"
	${COBRA}
)

func NewCmdIssue() *cobra.Command {
	cmd := &cobra.Command{Use: "issue <command>", Aliases: []string{"issues"}}
	cmdutil.AddGroup(cmd, "General commands", listCmd.NewCmdList(), createCmd.NewCmdCreate())
	comment := commentCmd.NewCmdComment()
	cmd.AddCommand(comment, deleteCmd.NewCmdDelete(), developCmd.NewCmdDevelop(), portsCmd.NewCmdPorts())
	cmd.AddCommand(
		&cobra.Command{Use: "close", RunE: func(c *cobra.Command, a []string) error {
			_, err := http.NewRequest(http.MethodPatch, "repos/o/r/issues/1", nil)
			return err
		}},
		func() *cobra.Command {
			return &cobra.Command{Use: "view", RunE: func(c *cobra.Command, a []string) error { return nil }}
		}(),
	)
	return cmd
}
`,
  "pkg/cmdutil/cmdgroup.go": `package cmdutil

import ${COBRA}

func AddGroup(parent *cobra.Command, title string, cmds ...*cobra.Command) {
	for _, c := range cmds {
		parent.AddCommand(c)
	}
}
`,
  "pkg/cmd/issue/list/list.go": `package list

import (
	"github.com/cli/cli/v2/api"
	${COBRA}
)

func NewCmdList() *cobra.Command {
	return &cobra.Command{Use: "list", Aliases: []string{"ls"}, RunE: func(c *cobra.Command, a []string) error {
		_, err := api.CurrentLogin("github.com")
		return err
	}}
}
`,
  "api/login.go": `package api

import (
	"net/http"

	"github.com/cli/cli/v2/internal/ghinstance"
)

func CurrentLogin(host string) (string, error) {
	_, err := http.NewRequest("POST", ghinstance.GraphQLEndpoint(host), nil)
	return "query UserCurrent{viewer{login}}", err
}

func IssueCreate(host string) error {
	query := \`
	mutation IssueCreate($input: CreateIssueInput!) { createIssue(input: $input) { issue { url } } }\`
	_ = query
	return nil
}
`,
  "internal/ghinstance/host.go": `package ghinstance

func GraphQLEndpoint(host string) string { return "https://api." + host + "/graphql" }
`,
  "pkg/cmd/issue/create/create.go": `package create

import (
	"github.com/cli/cli/v2/api"
	${COBRA}
)

func NewCmdCreate() *cobra.Command { return newCmdCreate(nil) }

func newCmdCreate(runF func() error) *cobra.Command {
	return &cobra.Command{Use: "create", Aliases: []string{"new"}, RunE: func(c *cobra.Command, a []string) error {
		return api.IssueCreate("github.com")
	}}
}
`,
  "pkg/cmd/issue/comment/comment.go": `package comment

import ${COBRA}

type commenter interface{ Comment(body string) error }

type mutator interface {
	Mutate(name string, m interface{}, v map[string]interface{}) error
}

type graphqlCommenter struct{ client mutator }

func (g graphqlCommenter) Comment(body string) error {
	return g.client.Mutate("CommentCreate", nil, nil)
}

func NewCmdComment() *cobra.Command {
	var c commenter = graphqlCommenter{}
	return &cobra.Command{Use: "comment", RunE: func(cmd *cobra.Command, a []string) error {
		return c.Comment(a[0])
	}}
}
`,
  "pkg/cmd/issue/delete/delete.go": `package delete

import (
	"net/http"

	${COBRA}
)

type deleter struct{ client *http.Client }

func (d deleter) Delete(number string) error {
	req, _ := http.NewRequest(http.MethodDelete, "repos/o/r/issues/"+number, nil)
	_, err := d.client.Do(req)
	return err
}

func NewCmdDelete() *cobra.Command {
	return &cobra.Command{Use: "delete", RunE: func(c *cobra.Command, a []string) error {
		return deleter{}.Delete(a[0])
	}}
}
`,
  "pkg/cmd/issue/develop/develop.go": `package develop

import (
	"context"

	"github.com/cli/cli/v2/git"
	${COBRA}
)

func NewCmdDevelop() *cobra.Command {
	return &cobra.Command{Use: "develop", RunE: func(c *cobra.Command, a []string) error {
		_, err := (&git.Client{}).Command(context.Background(), "push", "origin", "HEAD")
		return err
	}}
}
`,
  "git/client.go": `package git

import (
	"context"
	"os/exec"
)

type Client struct{}

func (c *Client) Command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "git", args...), nil
}
`,
  "pkg/cmd/issue/ports/ports.go": `package ports

import (
	"net/http"

	${COBRA}
)

func NewCmdPorts() *cobra.Command {
	cmd := &cobra.Command{Use: "ports", RunE: func(c *cobra.Command, a []string) error { return nil }}
	cmd.AddCommand(newForwardCmd())
	cmd.AddCommand(&cobra.Command{Use: "visibility", RunE: func(c *cobra.Command, a []string) error {
		_, err := http.NewRequest("PATCH", "user/codespaces/x/ports", nil)
		return err
	}})
	return cmd
}

func newForwardCmd() *cobra.Command {
	return &cobra.Command{Use: "forward", RunE: func(c *cobra.Command, a []string) error {
		_, err := http.NewRequest("POST", "user/codespaces/x/start", nil)
		return err
	}}
}
`,
  "pkg/cmd/org/org.go": `package org

import (
	"net/http"

	${COBRA}
)

func NewCmdOrg() *cobra.Command {
	cmd := &cobra.Command{Use: "org <command>", PersistentPreRunE: func(c *cobra.Command, a []string) error {
		_, err := http.NewRequest(http.MethodPut, "user/visits", nil)
		return err
	}}
	cmd.AddCommand(&cobra.Command{Use: "list", RunE: func(c *cobra.Command, a []string) error { return nil }})
	return cmd
}
`,
  "pkg/cmd/send-telemetry/send_telemetry.go": `package sendtelemetry

import (
	"github.com/cli/cli/v2/internal/barista/observability"
	${COBRA}
)

func NewCmdSendTelemetry() *cobra.Command {
	return &cobra.Command{Use: "send-telemetry", Hidden: true, RunE: func(c *cobra.Command, a []string) error {
		return observability.Record()
	}}
}
`,
  "internal/barista/observability/telemetry.go": `package observability

import "net/http"

func Record() error {
	_, err := http.NewRequest("POST", "https://telemetry.example", nil)
	return err
}
`,
};

describe("gh-writes.go: the derivation over gh's source", () => {
  const sources = temporaryDirectories("gh-writes-source");

  // One command each way gh's source builds one (a constructor of its own package, one returning
  // another's, a `cobra.Command` literal and a called function literal where it is added, one
  // added through cmdutil's AddGroup or through a variable) and each way it writes (net/http's
  // method constant, a method literal, a `Mutate` call, a mutation's text, a `git push`, the OAuth
  // flow, a group's PersistentPreRunE), and the reads beside them. Catches, each by the row it
  // turns: a method call resolved to no method, or to a method of a package the caller does not
  // reach (auth logout's configuration `Delete` read as issue delete's request); a group that runs
  // on its own read with the commands it adds (issue ports); a POST to the GraphQL endpoint read as
  // a write (issue list); a hidden command's `Hidden` not read (send-telemetry); a hook cobra runs
  // around a command left out of it (org list); `gh api` read as a write by its name, where a call
  // of it writes by its request (api); and each marker left out.
  test("each command is read as gh builds it, and writes where any call it reaches does", () => {
    const { code, out, err } = derive(ghSource(sources(), FIXTURE));
    expect({ code, err }).toEqual({ code: 0, err: "" });
    const commands = JSON.parse(out) as Record<
      string,
      {
        file: string;
        aliases?: string[];
        hidden?: boolean;
        runs: boolean;
        byRequest?: boolean;
        writes: boolean;
        write?: string;
      }
    >;
    expect(
      Object.fromEntries(
        Object.entries(commands).map(([name, { aliases, hidden, runs, writes }]) => [
          name,
          { aliases, hidden, runs, writes },
        ])
      )
    ).toEqual({
      api: { aliases: undefined, hidden: undefined, runs: true, writes: false },
      auth: { aliases: undefined, hidden: undefined, runs: false, writes: false },
      "auth logout": { aliases: undefined, hidden: undefined, runs: true, writes: false },
      "auth refresh": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      issue: { aliases: ["issues"], hidden: undefined, runs: false, writes: false },
      "issue close": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      "issue comment": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      "issue create": { aliases: ["new"], hidden: undefined, runs: true, writes: true },
      "issue delete": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      "issue develop": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      "issue list": { aliases: ["ls"], hidden: undefined, runs: true, writes: false },
      "issue ports": { aliases: undefined, hidden: undefined, runs: true, writes: false },
      "issue ports forward": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      "issue ports visibility": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      "issue view": { aliases: undefined, hidden: undefined, runs: true, writes: false },
      org: { aliases: undefined, hidden: undefined, runs: false, writes: false },
      "org list": { aliases: undefined, hidden: undefined, runs: true, writes: true },
      "send-telemetry": { aliases: undefined, hidden: true, runs: true, writes: true },
    });
    expect(commands["issue delete"]?.write).toBe(
      "pkg/cmd/issue/delete/delete.go:12 an HTTP DELETE, reached by NewCmdDelete → deleter.Delete"
    );
    expect(commands["org list"]?.write).toBe(
      "pkg/cmd/org/org.go:11 an HTTP PUT, reached by NewCmdOrg's PersistentPreRunE"
    );
    expect(commands.api).toEqual({
      file: "pkg/cmd/api/api.go",
      runs: true,
      byRequest: true,
      writes: false,
    });
  });

  // Catches: a command the derivation cannot read left out of the list, which rule 20 would read
  // as no write, rather than the derivation refusing.
  test("a command whose constructor it cannot read is an error, never a command left out", () => {
    const source = ghSource(sources(), {
      ...FIXTURE,
      "pkg/cmd/org/org.go": `package org

import ${COBRA}

func NewCmdOrg(made *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{Use: "org <command>"}
	cmd.AddCommand(made)
	return cmd
}
`,
    });
    const { code, out, err } = derive(source);
    expect({ code, out, err }).toEqual({
      code: 1,
      out: "",
      err: "gh-writes: NewCmdOrg adds a command whose constructor cannot be read (pkg/cmd/org/org.go:7)\nexit status 1\n",
    });
  });
});
