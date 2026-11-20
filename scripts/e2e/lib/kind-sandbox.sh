# shellcheck shell=bash
# A throwaway kind cluster running Agent Sandbox's controller under gVisor, for a test of Legion's
# Sandbox runtime on an amd64 Linux host: the rig of the kind job of ci.yaml, which
# packages/daemon/internal/kindtest drives from a Go test. Sourced by the caller, never run. The
# caller defines `note MESSAGE` (one indented observation line) and `fail MESSAGE` (ends the run,
# naming its check) before it calls a function here, and calls kind_sandbox_teardown when it ends,
# so the cluster goes on any exit.
#
# Every pin of the rig is here: kind's release, which kind_sandbox_install_kind builds with
# `go install` and kind_sandbox_create holds the binary to; the node image, the local registry,
# busybox, the NFS server and PostgreSQL by digest, each a row of .github/ci-images.yaml, which
# .github/scripts/check-ci-images.sh holds this file to; Agent Sandbox's v1.0.3 manifest, vendored
# beside its CRD in packages/daemon/internal/runtime/sandbox/testdata (the manifest's own header
# says how it differs from the release); and gVisor's dated release by sha512.
#
# kind_sandbox_install_kind DIR builds the pinned kind into DIR and makes it kind_bin.
# kind_sandbox_use NAME WORK names the cluster NAME and its files in WORK for the functions below,
# with no cluster made: a caller that runs each function in a shell of its own (internal/kindtest)
# calls it first in each.
# kind_sandbox_create NAME WORK [API_PORT] makes the cluster NAME, on the pinned node image, with
# its kubeconfig in a file of WORK (kind_kubeconfig) and the context kind-NAME (kind_context). Its
# one node (kind_node) is labelled legion.dev/pool=legion, the pool the runtime places every pod
# on, and the cluster has the legion namespace and kind's standard storage class. With API_PORT,
# the API server listens on 127.0.0.1:API_PORT, and its service-account issuer is
# https://127.0.0.1:API_PORT, whose discovery document and keys anyone may read, so a process of
# the host verifies a pod's service-account token as a broker does. kind_k runs kubectl against
# that file and context alone, so the caller's kubeconfig and its current context are never read.
# kind_sandbox_start_registry PORT starts a registry container on the kind network
# (kind_registry), published on the host at 127.0.0.1:PORT, from which the node pulls
# localhost:PORT/<image>: an image pushed there has a registry digest, which
# runtime.kubernetes.image requires.
# kind_sandbox_install_agent_sandbox applies Agent Sandbox's CRD and manifest and waits for its
# controller.
# kind_sandbox_install_gvisor WORK installs gVisor's dated release into the node's containerd as
# the runsc handler with a gvisor RuntimeClass, and holds a runtimeClassName: gvisor pod to gVisor's
# kernel.
# kind_sandbox_load_image IMAGE pulls IMAGE, a reference by digest, into the node's containerd under
# that digest, so a pod's start never waits on a registry.
# kind_sandbox_route_github ADDRESS PORT sends the cluster's pods to ADDRESS:PORT for github.com
# and api.github.com: a selectorless Service on 443 whose one endpoint is ADDRESS:PORT, and a
# CoreDNS hosts rule naming its cluster IP (kind_github_address). The rule reaches pods alone: the
# node, and containerd's pulls, resolve through Docker's DNS.
# kind_sandbox_start_nfs_server SEED starts an NFS server in the cluster (NFS-Ganesha, as the
# Kubernetes SIG Storage's nfs-provisioner image ships it), exporting /team, which holds seed.txt
# with SEED, and /scratch, and sets kind_nfs_server to its Service's cluster IP once the node
# itself mounts /team and reads the seed.
# kind_sandbox_start_postgres PASSWORD starts a PostgreSQL server in the cluster (pgvector's image,
# the services: Postgres of CI's other jobs), whose superuser postgres has PASSWORD, on a Service
# the cluster's pods reach at its cluster IP and this host reaches at the node's address on the
# kind network and the Service's node port, once it accepts connections.
# kind_sandbox_teardown STATUS EVIDENCE writes, when STATUS is not 0, what the cluster held (its
# nodes and pods, the legion namespace's Sandboxes and volume claims, its pods' descriptions and
# container logs and the nfs and postgres namespaces', every event, and the Agent Sandbox
# controller's log) to EVIDENCE, then deletes the cluster and the registry. It does nothing before
# kind_sandbox_use or kind_sandbox_create has named a cluster, and deletes nothing once the
# cluster is gone.
#
# LEGION_E2E_KIND names the kind binary when kind_sandbox_install_kind has not run in this shell.

kind_bin=${LEGION_E2E_KIND:-kind}
kind_cluster=
kind_context=
kind_kubeconfig=
kind_node=
kind_registry=
kind_github_address=
kind_nfs_server=

kind_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)

# The kind release, and the node image its release notes name the default of (Kubernetes
# v1.37.0, kindest/node at this digest).
kind_version=v0.33.0
kind_node_image=ghcr.io/cybermonklabs/legion/ci@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
# The local registry (registry 2.8.3), the busybox of the gVisor check pod (1.37), the NFS server
# (nfs-provisioner v4.0.8), and PostgreSQL with pgvector.
kind_registry_image=ghcr.io/cybermonklabs/legion/ci@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
kind_busybox_image=ghcr.io/cybermonklabs/legion/ci@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
kind_nfs_image=ghcr.io/cybermonklabs/legion/ci@sha256:c825f3d5e28bde099bd7a3daace28772d412c9157ad47fa752a9ad0baafc118d
kind_postgres_image=ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11

kind_agent_sandbox_crd=$kind_root/packages/daemon/internal/runtime/sandbox/testdata/sandboxes.agents.x-k8s.io.yaml
kind_agent_sandbox_manifest=$kind_root/packages/daemon/internal/runtime/sandbox/testdata/agent-sandbox-v1.0.3.yaml
kind_gvisor_url=https://storage.googleapis.com/gvisor/releases/release/20261012.0/x86_64/gvisor.tar.zstd
kind_gvisor_sha512=ce2e1aeb853c104e854dea6644333a1dba0fbc9dd1646af97e1d013fdf810da6d4176dd96f2139d7b055e5382117c0d7c4567ebd64ed9bef47fdd15f80fd67ab

kind_k() { kubectl --kubeconfig "$kind_kubeconfig" --context "$kind_context" "$@"; }

kind_sandbox_install_kind() {
  # `go install <module>@<version>` builds the release from its own go.mod, outside any workspace,
  # and reads no file of this repository.
  GOBIN=$1 GOWORK=off GOFLAGS= go install "sigs.k8s.io/kind@$kind_version"
  kind_bin=$1/kind
  note "$("$kind_bin" version)"
}

kind_sandbox_use() {
  kind_cluster=$1 kind_context=kind-$1 kind_kubeconfig=$2/kubeconfig kind_node=$1-control-plane kind_registry=$1-registry
}

kind_sandbox_create() {
  kind_sandbox_use "$1" "$2"
  local port=${3:-} config=$2/kind-config.yaml version
  version=$("$kind_bin" version 2>&1) || fail "no kind runs as $kind_bin: $version; build the pinned one with kind_sandbox_install_kind"
  [[ $version == "kind $kind_version "* ]] ||
    fail "the rig is pinned to kind $kind_version, and $kind_bin is $version; build the pinned one with kind_sandbox_install_kind"
  {
    printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\n'
    [ -z "$port" ] || printf 'networking:\n  apiServerAddress: 127.0.0.1\n  apiServerPort: %s\n' "$port"
    printf 'nodes:\n- role: control-plane\n  image: %s\n' "$kind_node_image"
    # Kubernetes 1.37 takes kubeadm's v1beta4 configuration alone, whose extraArgs is a list of
    # name and value; a patch naming that apiVersion is applied as written, and kubeadm lets an
    # extra argument override its own default (https://kubernetes.default.svc.cluster.local).
    [ -z "$port" ] || cat <<EOF
  kubeadmConfigPatches:
  - |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    apiServer:
      extraArgs:
      - name: service-account-issuer
        value: https://127.0.0.1:$port
      - name: service-account-jwks-uri
        value: https://127.0.0.1:$port/openid/v1/jwks
EOF
  } >"$config"
  "$kind_bin" create cluster --name "$kind_cluster" --config "$config" --kubeconfig "$kind_kubeconfig" --wait 180s
  kind_k label node "$kind_node" legion.dev/pool=legion
  kind_k create namespace legion
  kind_k get storageclass standard -o name
  if [ -n "$port" ]; then
    kind_k create clusterrolebinding service-account-issuer-discovery-anonymous \
      --clusterrole=system:service-account-issuer-discovery --group=system:unauthenticated
    note "the API server's service-account issuer is https://127.0.0.1:$port; anyone may read its discovery document and keys"
  fi
  note "$("$kind_bin" version), node $(kind_k get node "$kind_node" -o jsonpath='{.status.nodeInfo.kubeletVersion}, allocatable {.status.allocatable.cpu} cpu, {.status.allocatable.memory} memory, {.status.allocatable.ephemeral-storage} ephemeral storage')"
}

kind_sandbox_start_registry() {
  local port=$1 hosts=/etc/containerd/certs.d/localhost:$1
  docker run -d --name "$kind_registry" --network kind -p "127.0.0.1:${port}:5000" "$kind_registry_image" >/dev/null
  # kind's node images read /etc/containerd/certs.d (config_path) since kind v0.27.0, so this
  # mirror needs no restart of containerd.
  docker exec "$kind_node" mkdir -p "$hosts"
  printf '[host."http://%s:5000"]\n' "$kind_registry" | docker exec -i "$kind_node" cp /dev/stdin "$hosts/hosts.toml"
  kind_k apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: local-registry-hosting, namespace: kube-public}
data:
  localRegistryHosting.v1: |
    host: "localhost:$port"
EOF
  local _
  for _ in $(seq 1 30); do
    curl -fsS "http://127.0.0.1:${port}/v2/" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -fsS "http://127.0.0.1:${port}/v2/" >/dev/null || fail "the registry $kind_registry does not answer on 127.0.0.1:$port"
  note "registry $kind_registry on the kind network, localhost:$port to the host and the node"
}

kind_sandbox_install_agent_sandbox() {
  kind_k apply -f "$kind_agent_sandbox_crd"
  # `kubectl wait --for=condition=` refuses a CRD whose status holds no conditions yet, which a
  # wait issued right after the create can meet (CI run 44538419665: ".status.conditions accessor
  # error: <nil>"), so this reads the condition until it is True, for at most 60 seconds.
  local established="" deadline=$((SECONDS + 60))
  until [ "$established" = True ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "the Sandbox CRD is not Established after 60 s"
    sleep 1
    established=$(kind_k get crd/sandboxes.agents.x-k8s.io \
      -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null || true)
  done
  kind_k apply -f "$kind_agent_sandbox_manifest"
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
  kind_k run gvisor-check --image="$kind_busybox_image" --restart=Never \
    --overrides='{"spec":{"runtimeClassName":"gvisor"}}' -- dmesg
  kind_k wait --for=jsonpath='{.status.phase}'=Succeeded pod/gvisor-check --timeout=180s
  kind_k logs gvisor-check | grep -q 'Starting gVisor' || fail "a gvisor pod's kernel is not gVisor's: $(kind_k logs gvisor-check)"
  note "a runtimeClassName: gvisor pod runs under $(docker exec "$kind_node" /usr/local/bin/runsc --version | head -1)"
  kind_k delete pod gvisor-check --wait=false
}

kind_sandbox_load_image() {
  [[ $1 == *@sha256:* ]] || fail "the image to load must be named by digest (<image>@sha256:<64 hex>), got $1"
  docker exec "$kind_node" crictl pull "$1" >/dev/null
  note "the node's containerd holds $1: $(docker exec "$kind_node" crictl inspecti -o go-template --template '{{.status.size}}' "$1") bytes"
}

kind_sandbox_route_github() {
  local address=$1 port=$2 corefile hosts
  kind_k apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: fake-github}
---
apiVersion: v1
kind: Service
metadata: {name: github, namespace: fake-github}
spec:
  ports: [{name: https, port: 443, protocol: TCP}]
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: github
  namespace: fake-github
  labels: {kubernetes.io/service-name: github}
addressType: IPv4
ports: [{name: https, port: $port, protocol: TCP}]
endpoints: [{addresses: ["$address"]}]
EOF
  kind_github_address=$(kind_k -n fake-github get service github -o jsonpath='{.spec.clusterIP}')
  [ -n "$kind_github_address" ] || fail "the fake GitHub's Service has no cluster IP"
  corefile=$(kind_k -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
  [[ $corefile == *".:53 {"* ]] || fail "CoreDNS's Corefile has no .:53 server block to route github.com in: $corefile"
  [[ $corefile != *"hosts {"* ]] || fail "CoreDNS's Corefile holds a hosts block already, and the rig routes github.com once: $corefile"
  hosts=".:53 {
    hosts {
       $kind_github_address github.com api.github.com
       fallthrough
    }"
  corefile=${corefile/".:53 {"/"$hosts"}
  kind_k -n kube-system create configmap coredns --from-literal=Corefile="$corefile" --dry-run=client -o yaml |
    kind_k replace -f -
  # A restart rather than CoreDNS's reload, which waits on the ConfigMap volume's sync and then
  # 30 s and its jitter; the restart also drops the cache's answers for the two names.
  kind_k -n kube-system rollout restart deployment/coredns
  kind_k -n kube-system rollout status deployment/coredns --timeout=180s
  note "github.com and api.github.com resolve, for the cluster's pods, to $kind_github_address:443, whose endpoint is $address:$port"
}

kind_sandbox_start_nfs_server() {
  local seed=$1 mounted
  kind_k create namespace nfs
  kind_k apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: ganesha, namespace: nfs}
data:
  ganesha.conf: |
    NFS_Core_Param { Protocols = 4; }
    NFSV4 { Grace_Period = 0; Lease_Lifetime = 10; Minor_Versions = 0; }
    EXPORT_DEFAULTS { SecType = sys; Squash = no_root_squash; }
    EXPORT { Export_Id = 1; Path = /export/team; Pseudo = /team; Access_Type = RW; FSAL { Name = VFS; } }
    EXPORT { Export_Id = 2; Path = /export/scratch; Pseudo = /scratch; Access_Type = RW; FSAL { Name = VFS; } }
---
apiVersion: v1
kind: Pod
metadata: {name: ganesha, namespace: nfs, labels: {app: ganesha}}
spec:
  containers:
  - name: ganesha
    image: $kind_nfs_image
    command:
    - sh
    - -c
    - >-
      mkdir -p /export/team /export/scratch /run/dbus /usr/local/var/run/ganesha &&
      printf '%s\n' "\$SEED" >/export/team/seed.txt && chmod 0777 /export/team /export/scratch &&
      rpcbind -w && rpc.statd --port 662 && dbus-daemon --system --nopidfile &&
      exec ganesha.nfsd -F -L /dev/stdout -f /etc/ganesha/ganesha.conf
    env: [{name: SEED, value: "$seed"}]
    securityContext: {capabilities: {add: [DAC_READ_SEARCH, SYS_RESOURCE]}}
    readinessProbe: {tcpSocket: {port: 2049}, periodSeconds: 2}
    resources: {requests: {cpu: 100m, memory: 64Mi}, limits: {memory: 512Mi}}
    volumeMounts: [{name: export, mountPath: /export}, {name: conf, mountPath: /etc/ganesha}]
  volumes:
  - {name: export, emptyDir: {}}
  - {name: conf, configMap: {name: ganesha}}
---
apiVersion: v1
kind: Service
metadata: {name: ganesha, namespace: nfs}
spec: {selector: {app: ganesha}, ports: [{name: nfs, port: 2049}]}
EOF
  kind_k -n nfs wait --for=condition=Ready pod/ganesha --timeout=180s
  kind_nfs_server=$(kind_k -n nfs get service ganesha -o jsonpath='{.spec.clusterIP}')
  [ -n "$kind_nfs_server" ] || fail "the NFS server's Service has no cluster IP"
  mounted=$(docker exec "$kind_node" sh -c "mkdir -p /tmp/share-check && mount -t nfs $kind_nfs_server:/team /tmp/share-check && cat /tmp/share-check/seed.txt; umount /tmp/share-check" 2>&1) ||
    fail "the kind node cannot mount $kind_nfs_server:/team: $mounted"
  [ "$mounted" = "$seed" ] || fail "the node read $mounted from $kind_nfs_server:/team, not the seed"
  note "NFS-Ganesha at $kind_nfs_server exports /team (seed.txt: $seed) and /scratch; the node mounts /team and reads the seed"
}

kind_sandbox_start_postgres() {
  local password=$1
  kind_k create namespace postgres
  # The data directory is the pod's own: one level above it is mounted, which is where both the
  # image's PostgreSQL 17 (/var/lib/postgresql/data) and 18 (/var/lib/postgresql/18) keep theirs.
  kind_k apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata: {name: postgres, namespace: postgres, labels: {app: postgres}}
spec:
  containers:
  - name: postgres
    image: $kind_postgres_image
    env: [{name: POSTGRES_PASSWORD, value: "$password"}]
    readinessProbe: {exec: {command: [pg_isready, -q, -h, 127.0.0.1, -U, postgres]}, periodSeconds: 2}
    resources: {requests: {cpu: 100m, memory: 256Mi}, limits: {memory: 1Gi}}
    volumeMounts: [{name: data, mountPath: /var/lib/postgresql}]
  volumes:
  - {name: data, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata: {name: postgres, namespace: postgres}
spec: {type: NodePort, selector: {app: postgres}, ports: [{name: postgres, port: 5432}]}
EOF
  kind_k -n postgres wait --for=condition=Ready pod/postgres --timeout=180s
  note "PostgreSQL at $(kind_k -n postgres get service postgres -o jsonpath='{.spec.clusterIP}:5432') in the cluster, node port $(kind_k -n postgres get service postgres -o jsonpath='{.spec.ports[0].nodePort}'), accepting connections"
}

kind_sandbox_teardown() {
  local status=$1 evidence=$2 namespace pod
  [ -n "$kind_cluster" ] || return 0
  if "$kind_bin" get clusters 2>/dev/null | grep -qx "$kind_cluster"; then
    if [ "$status" -ne 0 ]; then
      mkdir -p "$evidence"
      {
        kind_k get nodes -o wide
        kind_k get pods -A -o wide
        kind_k -n legion get sandboxes,persistentvolumeclaims -o yaml
        for namespace in legion nfs postgres; do
          kind_k -n "$namespace" describe pods
          for pod in $(kind_k -n "$namespace" get pods -o name 2>/dev/null); do
            kind_k -n "$namespace" logs "$pod" --all-containers --prefix --tail=200
          done
        done
        kind_k get events -A --sort-by=.lastTimestamp
      } >"$evidence/cluster-at-failure.txt" 2>&1 || true
      kind_k -n agent-sandbox-system logs deployment/agent-sandbox-controller --all-containers --tail=2000 \
        >"$evidence/agent-sandbox-controller.log" 2>&1 || true
      echo "the cluster's pods, Sandboxes, volume claims, events and logs at the failure: $evidence/cluster-at-failure.txt and $evidence/agent-sandbox-controller.log" >&2
    fi
    "$kind_bin" delete cluster --name "$kind_cluster" --kubeconfig "$kind_kubeconfig" >/dev/null 2>&1 ||
      echo "could not delete kind cluster $kind_cluster; delete it with: $kind_bin delete cluster --name $kind_cluster" >&2
  fi
  docker rm -f "$kind_registry" >/dev/null 2>&1 || true
}
