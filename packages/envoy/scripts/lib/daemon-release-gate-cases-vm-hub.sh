# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-vm-hub.sh
#
# The --listeners leg's VM-hub scenarios. daemon-release-gate.test.sh sources this file after its own
# --listeners scenarios, with the harness's fakes and the listener pins it names; it is not run on
# its own.
#
# Envoy's listenerImage is read only by the VM hub's listener, gcp:NatsHub, and the hub's removal
# deletes that key with it, so an absent key cannot tell a gone hub from a stack file that lost its
# pin. The leg decides the hub from the deployment's program, positively: it fetches the deployment
# repository at its default-branch commit as one archive and reads every Go file in it that is not a
# test. A declared hub whose pin is gone is UNKNOWN, and only a program that declares none, every
# file of it read, drops the VM pin.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file
# shellcheck disable=SC2034 # gate_under_test is the harness's, which its runners read

echo "case: --listeners, envoy/ declares the VM hub and its pin is read"
new_case listeners-hub-declared
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$identical_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "names the use that declares the hub, at the commit it read" \
  "$(has_line "VM HUB: envoy/main.go:3 at ${deployment_head} uses NewNatsHub")"
check "reads the VM pin and compares it" "$(has_line "PASS: envoy runs ${ahead_rev} (ahead of ${commit})")"
check "reads the head of the deployment's default branch" \
  "$(grep -qxF "gh api repos/${deployment}/commits/HEAD --jq .sha" "$calls" && echo true || echo false)"
check "fetches the deployment repository once, at that commit" \
  "$(is "$(grep -c "tarball/${deployment_head}" "$calls")" 1)"
check "prints no NO VM HUB line" "$(lacks "NO VM HUB" "$out")"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, the VM hub declared and its listenerImage absent"
new_case listeners-hub-no-pin
listener_stacks "" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the key the declared hub needs" \
  "$(has_line "UNKNOWN: envoy: envoy/Pulumi.prod.yaml sets no ${envoy_project}:listenerImage")"
check "prints no NO VM HUB line" "$(lacks "NO VM HUB" "$out")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

# Control: a gate whose VM leg reads an absent listenerImage as no VM hub, the reading this leg
# exists to refuse, opens on the fixture above; so that fixture fails such a gate, by name.
mutant vm_hub hub-from-pin <<'EOF'
vm_hub() {
  hub_head=$(gh api "repos/${deployment}/commits/HEAD" --jq .sha)
  project_of envoy || return 1
  stack_file envoy/Pulumi.prod.yaml || return 1
  if [ -n "$(config_value "${project}:listenerImage")" ]; then
    hub=yes
    hub_use=envoy/Pulumi.prod.yaml:1
  else
    hub=no
  fi
}
EOF
gate_under_test="$mutant_gate"
run_gate -- --listeners "$commit"
gate_under_test="$script"
check "control: a VM leg that reads the absent pin as no hub opens on the declared-and-absent fixture" \
  "$([ "$(last_line_is "GATE OPEN")" = true ] && grep -q '^NO VM HUB: ' "$out" && echo true || echo false)"

echo "case: --listeners, no Go file of the deployment declares the VM hub"
new_case listeners-no-hub
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
index "$envoy_pin" "$legion" "$behind_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$identical_rev"
compare "$behind_rev" behind 4
compare "$ahead_rev" ahead 0
compare "$identical_rev" identical 0
run_gate -- --listeners "$commit"
check "exits 0" "$(is "$status" 0)"
check "says the program declares no VM hub, at the commit it read" \
  "$(has_line "NO VM HUB: envoy/ at ${deployment_head} declares no gcp:NatsHub")"
check "a func's own name, a method's, a comment, a string, a _test.go, testdata/ and another program declare nothing" \
  "$(grep -q '^VM HUB:' "$out" && echo false || echo true)"
check "reads no VM pin: not envoy's stack files, not its image" \
  "$([ "$(lacks "contents/envoy/" "$calls")" = true ] && lacks "$envoy_pin" "$calls" || echo false)"
check "opens on the other two pins" \
  "$(grep -qxF "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})" "$out" && grep -qxF "PASS: dispatch runs ${identical_rev} (identical to ${commit})" "$out" && echo true || echo false)"
check "the verdict is GATE OPEN" "$(last_line_is "GATE OPEN")"

echo "case: --listeners, the head of the deployment repository cannot be read"
new_case listeners-hub-no-head
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
refuse "repos/${deployment}/commits/HEAD" "gh: Bad credentials (HTTP 401)"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming GitHub's answer" \
  "$(has_line "UNKNOWN: envoy: cannot read the head of ${deployment}: gh: Bad credentials (HTTP 401)")"
check "fetches no archive" "$(lacks "/tarball/" "$calls")"
check "neither reads the VM pin nor says the hub is gone" \
  "$([ "$(lacks "$envoy_pin" "$calls")" = true ] && lacks "NO VM HUB" "$out" || echo false)"
check "still reads the other two" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, a head of the deployment repository that is no commit id"
new_case listeners-hub-head-not-commit
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate FAKE_HEAD=main -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the answer" \
  "$(has_line "UNKNOWN: envoy: GitHub answered main for the head of ${deployment}, which is no commit id")"
check "fetches no archive" "$(lacks "/tarball/" "$calls")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, the deployment repository cannot be fetched"
new_case listeners-hub-unfetched
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
refuse "repos/${deployment}/tarball/${deployment_head}" "gh: Resource not accessible by integration (HTTP 403)"
index "$envoy_pin" "$legion" "$ahead_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming GitHub's answer" \
  "$(has_line "UNKNOWN: envoy: cannot fetch ${deployment} at ${deployment_head}: gh: Resource not accessible by integration (HTTP 403)")"
check "neither reads the VM pin nor says the hub is gone" \
  "$([ "$(lacks "$envoy_pin" "$calls")" = true ] && lacks "NO VM HUB" "$out" || echo false)"
check "still reads the other two" "$(has_line "PASS: envoy-cluster runs ${ahead_rev} (ahead of ${commit})")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --listeners, an archive of the repository that cannot be read"
new_case listeners-hub-corrupt
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
: >"${case_root}/corrupt-archive"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the archive" \
  "$(grep -qF "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: the archive cannot be read: " "$out" && echo true || echo false)"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

echo "case: --listeners, envoy/ holds no Go file"
new_case listeners-hub-no-go
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
find "${case_root}/files/envoy" -name '*.go' -delete
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, having nothing to decide by" \
  "$(has_line "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/ holds no Go file to read the VM hub from")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"

# Every form of a use declares the hub, wherever the repository's code holds it: a space or a
# comment between the name and its call, a reference called later, a helper package of envoy/, the
# module envoy/go.mod replaces with a local directory, a package under testdata/ or an _ directory,
# which go build compiles once something imports it, and another program of the repository, which
# can only lean the gate toward reading the VM pin. Each fixture's VM pin is behind, so a gate that
# missed the use would print NO VM HUB and open.
for shape in space comment reference helper replaced testdata underscore other-program; do
  echo "case: --listeners, the VM hub declared by a use of shape ${shape}"
  new_case "listeners-use-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  use='package hub\n\nfunc Run() { envoygcp.NewNatsHub(ctx, "hub") }\n'
  case "$shape" in
    space) printf 'package main\n\n\thub, err := envoygcp.NewNatsHub (ctx, "hub")\n' >"${case_root}/files/envoy/main.go" ;;
    comment) printf 'package main\n\n\thub, err := envoygcp.NewNatsHub /* the VM hub */ (ctx, "hub")\n' >"${case_root}/files/envoy/main.go" ;;
    reference) printf 'package main\n\nvar newHub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/main.go" ;;
    helper) mkdir -p "${case_root}/files/envoy/infra" && printf '%b' "$use" >"${case_root}/files/envoy/infra/hub.go" ;;
    replaced) printf '%b' "$use" >"${case_root}/files/shared/hub.go" ;;
    testdata) mkdir -p "${case_root}/files/envoy/testdata/hub" && printf '%b' "$use" >"${case_root}/files/envoy/testdata/hub/hub.go" ;;
    underscore) mkdir -p "${case_root}/files/envoy/_infra" && printf '%b' "$use" >"${case_root}/files/envoy/_infra/hub.go" ;;
    other-program) printf '%b' "$use" >"${case_root}/files/envoy-cluster/hub.go" ;;
  esac
  index "$envoy_pin" "$legion" "$behind_rev"
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$behind_rev" behind 4
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits 1" "$(is "$status" 1)"
  check "names a use" "$(grep -q "^VM HUB: .* at ${deployment_head} uses NewNatsHub$" "$out" && echo true || echo false)"
  check "reads the VM pin and shuts on it" "$(has_line "GATE SHUT: envoy runs ${behind_rev} (behind ${commit} by 4)")"
  check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
done

# A file that names NewNatsHub and cannot be read as Go could hide a use, so it is UNKNOWN; a file
# that does not name it cannot use it, whatever else is wrong with it.
for shape in comment utf-8 unrelated; do
  echo "case: --listeners, a Go file the gate cannot read as Go (${shape})"
  new_case "listeners-hub-not-go-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  case "$shape" in
    comment)
      printf 'package main\n\n/* envoygcp.NewNatsHub(ctx, "hub") and a comment with no end\n' >"${case_root}/files/envoy/main.go"
      want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/main.go names NewNatsHub and has a /* comment with no end"
      ;;
    utf-8)
      printf 'package main\n\n// \xff\nvar hub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/main.go"
      want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/main.go names NewNatsHub and is no UTF-8 text"
      ;;
    unrelated)
      printf 'package main\n\nfunc main( { /* \xff and no end\n' >"${case_root}/files/envoy/broken.go"
      want="NO VM HUB: envoy/ at ${deployment_head} declares no gcp:NatsHub"
      ;;
  esac
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits $([ "$shape" = unrelated ] && echo 0 || echo 3)" "$(is "$status" "$([ "$shape" = unrelated ] && echo 0 || echo 3)")"
  check "says ${want%%:*}" "$(has_line "$want")"
done

# A local replace or use that leaves the repository names code the gate cannot read, in any of the
# forms go.mod and go.work take: quoted, indented, in a block, absolute, in a go.work.
for shape in relative quoted indented absolute block work missing; do
  echo "case: --listeners, the program names a directory outside the repository (${shape})"
  new_case "listeners-go-mod-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  mod="${case_root}/files/envoy/go.mod"
  outside="../../far"
  case "$shape" in
    relative) printf 'module example.org/deployment/envoy\n\nreplace example.org/far => ../../far\n' >"$mod" ;;
    quoted) printf 'module example.org/deployment/envoy\n\nreplace example.org/far => "../../far" // a comment\n' >"$mod" ;;
    indented) printf 'module example.org/deployment/envoy\n\n\treplace example.org/far v1.0.0 => ../../far\n' >"$mod" ;;
    absolute) outside="/srv/envoy helpers" && printf 'module example.org/deployment/envoy\n\nreplace example.org/far => "/srv/envoy helpers"\n' >"$mod" ;;
    block) printf 'module example.org/deployment/envoy\n\nreplace(\n\texample.org/near => ./near\n\texample.org/far => %s\n)\n' "\`../../far\`" >"$mod" ;;
    work) printf 'go 1.26\n\nuse (\n\t./envoy\n\t../far\n)\n' >"${case_root}/files/go.work" && outside="../far" ;;
    missing) rm "$mod" ;;
  esac
  case "$shape" in
    work) want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: go.work names ../far, a directory outside this repository" ;;
    missing) want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/ holds no go.mod" ;;
    *) want="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: envoy/go.mod names ${outside}, a directory outside this repository" ;;
  esac
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming why" "$(has_line "$want")"
  check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
done

# A Go file that is a link compiles from its target, whatever the target's own name, so the gate
# reads the target; each fixture's VM pin is behind, so a gate that left the link unread would
# print NO VM HUB and open.
for shape in file chain; do
  echo "case: --listeners, the VM hub declared through a Go file that is a link (${shape})"
  new_case "listeners-link-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  printf 'package main\n\nvar hub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/hub-source.txt"
  if [ "$shape" = file ]; then
    ln -s hub-source.txt "${case_root}/files/envoy/zz_live.go"
  else
    ln -s ../envoy/hub-source.txt "${case_root}/files/shared/hub-link.txt"
    ln -s ../shared/hub-link.txt "${case_root}/files/envoy/zz_live.go"
  fi
  index "$envoy_pin" "$legion" "$behind_rev"
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$behind_rev" behind 4
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits 1" "$(is "$status" 1)"
  check "names the link as the use" "$(has_line "VM HUB: envoy/zz_live.go:3 at ${deployment_head} uses NewNatsHub")"
  check "reads the VM pin and shuts on it" "$(has_line "GATE SHUT: envoy runs ${behind_rev} (behind ${commit} by 4)")"
done

# What the gate cannot read from the archive is UNKNOWN: a link out of the repository, which can
# carry a local module or a workspace; a Go file that links to nothing in it; and the members git
# never writes, which a repository's archive cannot hold.
for shape in outside absolute dangling hardlink directory duplicate; do
  echo "case: --listeners, an archive member the gate cannot read (${shape})"
  new_case "listeners-member-${shape}"
  listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
  envoy_program none
  dir="${case_root}/files/envoy"
  prefix="UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}:"
  case "$shape" in
    outside) ln -s ../../far "$dir/linked" && want="${prefix} envoy/linked is a link to ../../far, outside this repository" ;;
    absolute) ln -s /srv/work/go.work "$dir/go.work" && want="${prefix} envoy/go.work is a link to /srv/work/go.work, outside this repository" ;;
    dangling) ln -s missing.txt "$dir/zz_live.go" && want="${prefix} envoy/zz_live.go is a link to missing.txt, which is no file of this repository" ;;
    hardlink) ln "$dir/main.go" "$dir/zz_copy.go" && want="${prefix} the archive holds envoy/zz_copy.go as a hard link, which git never writes" ;;
    directory) mkdir "$dir/live.go" && want="${prefix} the archive holds envoy/live.go as no file and no link" ;;
    duplicate)
      python3 - "${case_root}/files" "${case_root}/archive.tgz" <<'PY'
import io
import sys
import tarfile

root, out = sys.argv[1:]
with tarfile.open(out, "w:gz") as archive:
    archive.add(root, arcname="example-org-deployment-top")
    data = b"package main\n\nvar hub = envoygcp.NewNatsHub\n"
    info = tarfile.TarInfo("example-org-deployment-top/envoy/main.go")
    info.size = len(data)
    archive.addfile(info, io.BytesIO(data))
PY
      want="${prefix} the archive holds envoy/main.go twice, which a repository cannot"
      ;;
  esac
  index "$envoy_pin" "$legion" "$behind_rev"
  index "$cluster_pin" "$legion" "$ahead_rev"
  index "$dispatch_listener_pin" "$legion" "$ahead_rev"
  compare "$behind_rev" behind 4
  compare "$ahead_rev" ahead 0
  run_gate -- --listeners "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN naming the member" "$(has_line "$want")"
  check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
done

# GitHub's archive holds no submodule's files, only an empty directory at its path, so a repository
# that declares submodules (.gitmodules) holds code the gate cannot read, a use of NewNatsHub among
# what it could hold. Its program declaring no hub elsewhere, the gate still says UNKNOWN.
echo "case: --listeners, a deployment repository that declares submodules"
new_case listeners-submodules
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
printf '[submodule "vendor/sdk"]\n\tpath = vendor/sdk\n\turl = https://github.com/example-org/sdk\n' >"${case_root}/files/.gitmodules"
mkdir -p "${case_root}/files/vendor/sdk"
index "$envoy_pin" "$legion" "$behind_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$behind_rev" behind 4
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the submodules" \
  "$(has_line "UNKNOWN: envoy: cannot read the Go files of ${deployment} at ${deployment_head}: the repository holds .gitmodules, and submodule code is not in the archive")"
check "never says the hub is gone" "$(lacks "NO VM HUB" "$out")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"
