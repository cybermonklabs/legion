import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// assert_gh_files, taken from stage4b-sandbox-tree.sh by name, judges a container's gh bundle as
// `ls -A` lists its root and the repository owner's directory inside the pod (pod_exec, stubbed
// here to print ROOT for the root and OWNER for the owner's directory). The bundle is a
// whole-volume Secret projection the kubelet rewrites in place when the Secret changes, so beside
// the owners' directories its root always holds the kubelet's atomic-writer entries, and beside
// those the one file every bundle carries, `renames.txt`: the configured renames `legion gh` and
// `legion git-credential` route a request for a moved repository by (ghconfig.PairsFile). That
// file is required by its name, and no other file at the root is allowed in its place.
//
// One rule judges the owner's own directory, and the rows below drive both containers that hold
// one: a role of an issue pod and the daemon-launched controller. That directory holds config.yml
// and hosts.yml and nothing else, because a bundle holds one directory per owner account the App
// is installed on and no directory for a repository (INFRA-2543 R2), so a `<owner>/<repo>`
// directory, which a daemon before that change wrote, is a failure like any other entry. WHO, the
// helper's third argument, is the container each failure names.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));

// The home the bash below runs under, so it reads nothing of the caller's.
const home = temporaryDirectory("stage4b-gh-files-home");

// The two containers the live proof passes as WHO, each as its own call site names it.
const role = "the implementer's namespaces of pod-a";
const controller = "the daemon-launched controller's container pod-a/controller";

function judge(root: string, owner = "config.yml\nhosts.yml\n", container = role) {
  const script = [
    "set -Eeuo pipefail",
    'fail() { echo "FAIL: $*"; exit 1; }',
    "gh_owners_dir=/var/run/legion/gh",
    "gh_config_dir=/var/run/legion/gh/sjawhar",
    `pod_exec() {
  case "$*" in
    *"$gh_config_dir"*) printf "%s" "$OWNER" ;;
    *) printf "%s" "$ROOT" ;;
  esac
}`,
    fn("assert_gh_files"),
    `assert_gh_files pod-a controller ${JSON.stringify(container)}`,
    "echo OK",
  ].join("\n");
  const result = Bun.spawnSync(["bash", "-c", script], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      ROOT: root,
      OWNER: owner,
    },
  });
  return { exitCode: result.exitCode, out: result.stdout.toString() };
}

describe("assert_gh_files", () => {
  test("passes a refreshed projection: the owners' directories and the pairs file beside the kubelet's atomic-writer entries", () => {
    expect(judge("..2026_10_11_13_01_02.123456789\n..data\nacme\nrenames.txt\nsjawhar\n")).toEqual({
      exitCode: 0,
      out: "OK\n",
    });
  });

  test("fails a bundle without the repository owner's directory, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nacme\nrenames.txt\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the implementer's namespaces of pod-a's /var/run/legion/gh lacks sjawhar; it holds: ..data acme renames.txt \n"
    );
  });

  // Every bundle this daemon writes holds the pairs file, empty where the project configures no
  // rename that resolves, so a root without it is a pod of a daemon before it existed, whose
  // wrappers route a moved repository by its owner alone.
  test("fails a bundle without the pairs file, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nsjawhar\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the implementer's namespaces of pod-a's /var/run/legion/gh holds no renames.txt: the bundle's configured renames, which legion gh and legion git-credential route a moved repository by; it holds: ..data sjawhar \n"
    );
  });

  test("fails an owner's directory that lacks hosts.yml, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nrenames.txt\nsjawhar\n", "config.yml\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the implementer's namespaces of pod-a's /var/run/legion/gh/sjawhar lacks hosts.yml; it holds: config.yml \n"
    );
  });

  // The pairs file is allowed by its name and nothing is allowed in its place: a second
  // lower-case file at the root is a stranger still.
  test("fails a root entry that is neither an owner's directory, the pairs file, nor the kubelet's, naming it", () => {
    const { exitCode, out } = judge("..data\nrenames.txt\nsjawhar\nhosts.yml\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the implementer's namespaces of pod-a's /var/run/legion/gh holds an entry that is neither an owner's directory, the bundle's renames.txt, nor the kubelet's: hosts.yml \n"
    );
  });

  test("fails an owner's directory holding a file beside the two gh files, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nrenames.txt\nsjawhar\n", "config.yml\nhosts.yml\ntoken\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the implementer's namespaces of pod-a's /var/run/legion/gh/sjawhar holds token beside its two gh files, which are all an owner's directory holds: a bundle holds one directory per owner account the App is installed on and no directory for a repository; it holds: config.yml hosts.yml token \n"
    );
  });

  // A daemon before INFRA-2543 wrote a `<owner>/<repo>` directory under the owner's own, holding
  // those same two gh files, and the helper passed a bundle carrying one. A bundle holds no such
  // directory now, so the listing that once passed is the one this row fails.
  test("fails an owner's directory holding a repository's directory, naming it and the listing", () => {
    const { exitCode, out } = judge("..data\nrenames.txt\nsjawhar\n", "config.yml\nhosts.yml\nlegion\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the implementer's namespaces of pod-a's /var/run/legion/gh/sjawhar holds legion beside its two gh files, which are all an owner's directory holds: a bundle holds one directory per owner account the App is installed on and no directory for a repository; it holds: config.yml hosts.yml legion \n"
    );
  });

  test("fails a bundle without the kubelet's ..data, which a subPath mount would leave and never refresh", () => {
    const { exitCode, out } = judge("renames.txt\nsjawhar\n");
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the implementer's namespaces of pod-a's /var/run/legion/gh holds no ..data: it is not the whole-volume projection the kubelet refreshes in place; it holds: renames.txt sjawhar \n"
    );
  });

  test("passes the daemon-launched controller's directory holding its two gh files alone", () => {
    expect(
      judge("..data\nacme\nrenames.txt\nsjawhar\n", "config.yml\nhosts.yml\n", controller)
    ).toEqual({
      exitCode: 0,
      out: "OK\n",
    });
  });

  test("fails the daemon-launched controller's directory holding a repository's directory, naming it", () => {
    const { exitCode, out } = judge(
      "..data\nrenames.txt\nsjawhar\n",
      "config.yml\nhosts.yml\nlegion\n",
      controller
    );
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: the daemon-launched controller's container pod-a/controller's /var/run/legion/gh/sjawhar holds legion beside its two gh files, which are all an owner's directory holds: a bundle holds one directory per owner account the App is installed on and no directory for a repository; it holds: config.yml hosts.yml legion \n"
    );
  });
});
