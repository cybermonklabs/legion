# shellcheck shell=bash
# A throwaway kind cluster running Agent Sandbox's controller under gVisor, for a live proof of
# Legion's Sandbox runtime on an amd64 Linux host. Sourced by the entry script, never run. The entry
# script defines `note MESSAGE` (one indented observation line) and `fail MESSAGE` (ends the run,
# naming its check) before it calls a function here, and calls kind_sandbox_teardown from its EXIT
# trap, so the cluster goes on any exit.
#
# kind_sandbox_install_kind WORK installs kind v0.33.0, pinned by sha256, into WORK and makes it
# kind_bin, with that release's default node image, kindest/node:v1.37.0, as the copy
# .github/ci-images.yaml makes of it by digest (kind_node_image): a caller that runs on a
# GitHub-hosted runner calls it, so the cluster's node and its kind are one pair whatever kind the
# runner carries, and the node image comes from this repository's own package, not Docker Hub. A
# caller that does not call it creates the cluster with the kind on PATH (or LEGION_E2E_KIND) and
# that kind's own default node image.
# kind_sandbox_create NAME WORK makes the cluster NAME with its kubeconfig in a file of WORK
# (kind_kubeconfig) and the context kind-NAME (kind_context). Its one node (kind_node) is labelled
# legion.dev/pool=legion, the pool the runtime places every pod on, and the cluster has the legion
# namespace and kind's standard storage class. kind_k runs kubectl against that file and context
# alone, so the caller's kubeconfig and its current context are never read.
# kind_sandbox_install_agent_sandbox WORK applies Agent Sandbox's release manifest at the version
# internal/runtime/sandbox speaks, pinned by sha256, and waits for its controller.
# kind_sandbox_install_gvisor WORK installs gVisor's dated release, pinned by sha512, into the
# node's containerd as the runsc handler with a gvisor RuntimeClass, and holds a runtimeClassName:
# gvisor pod of kind_busybox, busybox 1.37 as .github/ci-images.yaml copies it by digest, to
# gVisor's kernel.
# kind_sandbox_load_image IMAGE pulls IMAGE, a reference by digest, into the node's containerd under
# that digest, so a pod's start never waits on a registry.
# kind_sandbox_internal_kubeconfig FILE writes the kubeconfig a container on the kind network reads,
# whose server is the node's address on that network.
# kind_sandbox_teardown STATUS EVIDENCE writes, when STATUS is not 0, what the cluster held (pods,
# Sandboxes, volume claims, events and the legion namespace's container logs) to
# EVIDENCE/cluster-at-failure.txt, then deletes the cluster. It does nothing before
# kind_sandbox_create has named a cluster, or once the cluster is gone.
#
# LEGION_E2E_KIND names the kind binary when it is not on PATH.

kind_bin=${LEGION_E2E_KIND:-kind}
kind_cluster=
kind_context=
kind_kubeconfig=
kind_node=
kind_node_image=

kind_busybox=ghcr.io/cybermonklabs/legion/ci@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
kind_release_url=https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-amd64
kind_release_sha256=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d

kind_agent_sandbox_url=https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.3/sandbox.yaml
kind_agent_sandbox_sha256=725fafdabe6aac202a89dc57f1cfe0e2e92f3164c8c2bd343fffca52f7039d96
kind_gvisor_url=https://storage.googleapis.com/gvisor/releases/release/20261012.0/x86_64/gvisor.tar.zstd
kind_gvisor_sha512=ce2e1aeb853c104e854dea6644333a1dba0fbc9dd1646af97e1d013fdf810da6d4176dd96f2139d7b055e5382117c0d7c4567ebd64ed9bef47fdd15f80fd67ab

kind_k() { kubectl --kubeconfig "$kind_kubeconfig" --context "$kind_context" "$@"; }

kind_sandbox_install_kind() {
  curl -fsSLo "$1/kind" "$kind_release_url"
  echo "$kind_release_sha256  $1/kind" | sha256sum -c -
  chmod +x "$1/kind"
  kind_bin=$1/kind
  kind_node_image=ghcr.io/cybermonklabs/legion/ci@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
}

kind_sandbox_create() {
  kind_cluster=$1 kind_context=kind-$1 kind_kubeconfig=$2/kubeconfig kind_node=$1-control-plane
  "$kind_bin" create cluster --name "$kind_cluster" --kubeconfig "$kind_kubeconfig" --wait 180s ${kind_node_image:+--image "$kind_node_image"}
  kind_k label node "$kind_node" legion.dev/pool=legion
  kind_k create namespace legion
  kind_k get storageclass standard -o name
  note "kind $("$kind_bin" version), node $(kind_k get node "$kind_node" -o jsonpath='{.status.nodeInfo.kubeletVersion} {.status.allocatable.cpu} cpu {.status.allocatable.memory} memory')"
}

kind_sandbox_install_agent_sandbox() {
  curl -fsSLo "$1/sandbox.yaml" "$kind_agent_sandbox_url"
  echo "$kind_agent_sandbox_sha256  $1/sandbox.yaml" | sha256sum -c -
  kind_k apply -f "$1/sandbox.yaml"
  kind_k -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=300s
}

kind_sandbox_install_gvisor() {
  curl -fsSLo "$1/gvisor.tar.zstd" "$kind_gvisor_url"
  echo "$kind_gvisor_sha512  $1/gvisor.tar.zstd" | sha512sum -c -
  mkdir -p "$1/gvisor"
  tar --zstd -xf "$1/gvisor.tar.zstd" -C "$1/gvisor"
  local part
  for part in runsc containerd-shim-runsc-v1 gvisor-bin; do
    docker cp "$1/gvisor/$part" "$kind_node:/usr/local/bin/$part"
  done
  docker exec "$kind_node" sh -c 'chown -R root:root /usr/local/bin/gvisor-bin &&
    printf "\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]\n  runtime_type = \"io.containerd.runsc.v1\"\n" >>/etc/containerd/config.toml &&
    systemctl restart containerd'
  kind_k wait --for=condition=Ready "node/$kind_node" --timeout=120s
  kind_k apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
  kind_k run gvisor-check --image="$kind_busybox" --restart=Never \
    --overrides='{"spec":{"runtimeClassName":"gvisor"}}' -- dmesg
  kind_k wait --for=jsonpath='{.status.phase}'=Succeeded pod/gvisor-check --timeout=180s
  # The whole log is read before it is matched: under the caller's pipefail, a `grep -q` reading
  # kubectl's output exits at its first match, and a kubectl still writing dies of SIGPIPE, which
  # fails the check on a log that holds the line.
  local kernel
  kernel=$(kind_k logs gvisor-check) || fail "cannot read the gvisor-check pod's log"
  [[ $kernel == *'Starting gVisor'* ]] || fail "a gvisor pod's kernel is not gVisor's: $kernel"
  note "a runtimeClassName: gvisor pod runs under $(docker exec "$kind_node" /usr/local/bin/runsc --version | head -1)"
  kind_k delete pod gvisor-check --wait=false
}

kind_sandbox_load_image() {
  [[ $1 == *@sha256:* ]] || fail "the image to load must be named by digest (<image>@sha256:<64 hex>), got $1"
  docker exec "$kind_node" crictl pull "$1" >/dev/null
  note "the node's containerd holds $1: $(docker exec "$kind_node" crictl inspecti -o go-template --template '{{.status.size}}' "$1") bytes"
}

kind_sandbox_internal_kubeconfig() {
  "$kind_bin" get kubeconfig --internal --name "$kind_cluster" >"$1"
}

kind_sandbox_teardown() {
  local status=$1 evidence=$2
  [ -n "$kind_cluster" ] || return 0
  # kind's whole list is read before it is matched: kind writes one line per cluster, so a
  # `grep -qx` that matched before the last line leaves kind to die of SIGPIPE under pipefail, and
  # a live cluster would be taken for one already gone and left behind.
  local clusters
  clusters=$("$kind_bin" get clusters 2>/dev/null) || return 0
  grep -qx "$kind_cluster" <<<"$clusters" || return 0
  if [ "$status" -ne 0 ]; then
    {
      kind_k get pods -A -o wide
      kind_k -n legion get sandboxes,persistentvolumeclaims -o yaml
      kind_k -n legion describe pods
      kind_k -n legion get events --sort-by=.lastTimestamp
      kind_k -n legion logs --all-containers --prefix --tail=200 -l legion.dev/project 2>&1
    } >"$evidence/cluster-at-failure.txt" 2>&1 || true
    echo "the cluster's pods, Sandboxes, volume claims, events and container logs at the failure: $evidence/cluster-at-failure.txt" >&2
  fi
  "$kind_bin" delete cluster --name "$kind_cluster" --kubeconfig "$kind_kubeconfig" >/dev/null 2>&1 ||
    echo "could not delete kind cluster $kind_cluster; delete it with: $kind_bin delete cluster --name $kind_cluster" >&2
}
