# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-vm-hub-archive.sh
#
# The --listeners scenarios for unreadable Go source and archive members, and for links and local
# modules in the deployment archive. daemon-release-gate.test.sh sources this file through its
# group runner after the harness. Do not run this file on its own.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

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

# A file's name is the repository's text, not the gate's: it is printed with each control character
# written as \x<hh>, so no name can rewrite a line of the gate's on a terminal.
echo "case: --listeners, a use in a Go file whose name holds an escape and a carriage return"
new_case listeners-name-control
listener_stacks "$envoy_pin" "$cluster_pin" "$server_pin" "$dispatch_listener_pin"
envoy_program none
printf 'package main\n\nvar hub = envoygcp.NewNatsHub\n' >"${case_root}/files/envoy/"$'\e[2K\rGATE OPEN.go'
index "$envoy_pin" "$legion" "$behind_rev"
index "$cluster_pin" "$legion" "$ahead_rev"
index "$dispatch_listener_pin" "$legion" "$ahead_rev"
compare "$behind_rev" behind 4
compare "$ahead_rev" ahead 0
run_gate -- --listeners "$commit"
check "exits 1" "$(is "$status" 1)"
check "names the file, its escape and carriage return written out" \
  "$(has_line "VM HUB: envoy/\\x1b[2K\\x0dGATE OPEN.go:3 at ${deployment_head} uses NewNatsHub")"
check "prints no escape and no carriage return" "$(grep -q $'[\e\r]' "$out" && echo false || echo true)"
