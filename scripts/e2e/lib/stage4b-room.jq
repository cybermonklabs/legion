# Stage 4b's capacity reads of the nodes the run's pods may use (stage4b-sandbox-tree.sh's preflight
# and issue-independence): the reservation an issue pod carries, the room those nodes have for the
# run's pods now, and whether one of them can hold two. A node the run's pods may use is one the
# scheduling they carry once admitted lets them onto (admitted_scheduling, run_node): the run's
# runtime.kubernetes.scheduling, its node_selector (every node when it sets none) and tolerations,
# with what the pods' RuntimeClass adds at admission, and none where admission refuses the pods. No
# pod asks for or keeps off another pod's node (LEGION-632), so room is counted in pods of the run's
# largest reservation, never in nodes per tree. Loaded with `jq -L scripts/e2e/lib` and
# `include "stage4b-room";`; quantity comes from stage4b-pods.jq.
include "stage4b-pods";

# issue_pod_reservation($expected) is an issue pod's reservation, cores and bytes: its one
# container's cpu and memory (EXPECTED maps each pod the daemon builds to its {cpu, memory}). The
# init containers carry the same reservation and add nothing: a pod's effective request is the
# larger of its largest init container and its containers' sum (the Kubernetes pod-resources rule).
def issue_pod_reservation($expected):
  def required_quantity($field):
    $expected.issue[$field] as $value
    | if $value == null then error("run_resources.issue.\($field) is missing") else ($value | quantity) end;
  {cpu: required_quantity("cpu"), memory: required_quantity("memory")};

# pod_request is a pod's effective request, cores and bytes, as the scheduler counts it against a
# node: the larger of its containers' summed requests and its largest init container's, a container
# that requests nothing counting as 0.
def pod_request:
  [.spec.containers[]? | .resources.requests // {}] as $c
  | [.spec.initContainers[]? | .resources.requests // {}] as $i
  | def total($list; $key): [$list[] | .[$key] // 0 | quantity] | add // 0;
    def largest($list; $key): [$list[] | .[$key] // 0 | quantity] | max // 0;
    {cpu: ([total($c; "cpu"), largest($i; "cpu")] | max),
     memory: ([total($c; "memory"), largest($i; "memory")] | max)};

# placeable_node keeps a node the scheduler can place a pod on: Ready, schedulable, not tainted
# karpenter.sh/disrupted and not being deleted.
def placeable_node:
  select((.spec.unschedulable // false) | not)
  | select(.metadata.deletionTimestamp == null)
  | select(any(.spec.taints[]?; .key == "karpenter.sh/disrupted") | not)
  | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"));

# fits($free; $pod) is how many pods of reservation POD fit in FREE (both {cpu, memory}), never
# below 0.
def fits($free; $pod):
  [($free.cpu / $pod.cpu), ($free.memory / $pod.memory)] | min | floor | if . < 0 then 0 else . end;

# tolerated($tolerations) holds when one of TOLERATIONS tolerates the node taint on input: of the
# taint's key, or of no key under `Exists`, which tolerates every taint; under operator `Exists`, or
# under `Equal` (Kubernetes' default) with the taint's value; and of no effect or the taint's.
def tolerated($tolerations):
  . as $taint
  | any($tolerations[];
      (.key == $taint.key or ((.key // "") == "" and .operator == "Exists"))
      and (.operator == "Exists" or (.value // "") == ($taint.value // ""))
      and ((.effect // "") == "" or .effect == $taint.effect));

# admitted_scheduling($selector; $tolerations; $runtime_class) is the placement a pod of the run
# carries once admitted, as Kubernetes' RuntimeClass admission (setScheduling) sets it from the
# run's SELECTOR (scheduling.node_selector) and TOLERATIONS (scheduling.tolerations) and the
# RuntimeClass RUNTIME_CLASS's scheduling. The selector is the union of the run's and the
# RuntimeClass's scheduling.nodeSelector, a label both name at the same value carried once. The
# tolerations are the run's followed by the RuntimeClass's scheduling.tolerations: Kubernetes'
# MergeTolerations also drops each toleration that another tolerates a superset of, so its list
# may be shorter, but it tolerates the same taints. Where the RuntimeClass's nodeSelector names a
# label the run's selector names at another value, admission refuses the pod, and the result is
# {conflicts}, one entry per such label in the wording of Kubernetes' refusal, which names the
# first it meets: `runtimeClass.scheduling.nodeSelector[KEY] = VALUE; pod.spec.nodeSelector[KEY] =
# VALUE`. A RuntimeClass with no scheduling adds nothing.
def admitted_scheduling($selector; $tolerations; $runtime_class):
  ($runtime_class.scheduling.nodeSelector // {}) as $class_selector
  | [$class_selector | to_entries[] | select($selector[.key] != null and $selector[.key] != .value)
      | "runtimeClass.scheduling.nodeSelector[\(.key)] = \(.value); pod.spec.nodeSelector[\(.key)] = \($selector[.key])"] as $conflicts
  | if $conflicts == [] then
      {selector: ($selector + $class_selector),
       tolerations: ($tolerations + ($runtime_class.scheduling.tolerations // []))}
    else {conflicts: $conflicts} end;

# run_node($scheduling) keeps a placeable node a pod of SCHEDULING (admitted_scheduling) may use:
# none where admission refuses the pod (conflicts); otherwise one that carries every label of the
# selector at the same value, and whose NoSchedule and NoExecute taints are each tolerated by one
# of the tolerations. Under an empty selector and no tolerations that is every placeable node with
# no such taint.
def run_node($scheduling):
  select($scheduling.conflicts == null)
  | placeable_node
  | select(.metadata.labels as $labels | all($scheduling.selector | to_entries[]; $labels[.key] == .value))
  | select(all(.spec.taints[]? | select(.effect == "NoSchedule" or .effect == "NoExecute"); tolerated($scheduling.tolerations)));

# run_room($scheduling; $nodes; $pods; $pod) is the room for pods of reservation POD on the nodes
# the run's pods may use now, from the SCHEDULING they carry once admitted (admitted_scheduling,
# run_node), every node of the cluster NODES (a node list) and every pod of the cluster PODS (a pod
# list across namespaces): `pod`, the reservation; `scheduling`, the admitted selector and
# tolerations it read the nodes by, or the conflicts for which admission refuses the run's pods;
# and `free_nodes`, each node run_node kept, with what its allocatable has free beside every
# non-terminal pod placed on it and how many such pods fit there. It bounds nothing: no one
# NodePool's limits bound the run's pods and the cluster may add nodes, so a read that keeps no
# node, as of a cluster whose matching nodes are at zero, says nothing of whether the run's pods
# can be placed. Only the conflicts do, since no node the cluster has or adds takes a pod that
# admission refuses (stage4b-sandbox-tree.sh preflight_room).
def run_room($scheduling; $nodes; $pods; $pod):
  ([$pods.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed") | select(.spec.nodeName != null)]) as $placed
  | [$nodes.items[] | run_node($scheduling)
      | .metadata.name as $name
      | ([$placed[] | select(.spec.nodeName == $name) | pod_request]) as $requests
      | {node: $name,
         free: {cpu: ((.status.allocatable.cpu | quantity) - ([$requests[].cpu] | add // 0)),
                memory: ((.status.allocatable.memory | quantity) - ([$requests[].memory] | add // 0))}}
      | .fits = fits(.free; $pod)] as $free_nodes
  | {pod: $pod, scheduling: $scheduling, free_nodes: $free_nodes};

# two_pods_per_node($scheduling; $nodes; $pod) is whether two pods of reservation POD can share one
# of the nodes of NODES the run's pods may use under the SCHEDULING they carry once admitted
# (admitted_scheduling, run_node): `fit` is true when one of those nodes' allocatable holds two such
# pods (fits), and null otherwise, since a node the cluster adds may hold two. It is never false: no
# bound on the nodes the cluster may add is known, so the read never says two of the run's pods
# cannot share a node. `reason` says which case, with the numbers.
def two_pods_per_node($scheduling; $nodes; $pod):
  def gib: . / 1073741824 * 100 | floor / 100;
  {cpu: ($pod.cpu * 2), memory: ($pod.memory * 2)} as $two
  | [$nodes.items[] | run_node($scheduling)
      | {node: .metadata.name, cpu: (.status.allocatable.cpu | quantity), memory: (.status.allocatable.memory | quantity)}] as $usable
  | ([$usable[] | select(fits(.; $pod) >= 2)] | first) as $holder
  | if $holder != null then
      {fit: true, reason: "node \($holder.node)'s allocatable, \($holder.cpu) CPU and \($holder.memory | gib) GiB, holds two of the run's pods (\($two.cpu) CPU, \($two.memory | gib) GiB), so the pods may share a node"}
    else
      {fit: null, reason: "no node of the \($usable | length) the run's pods may use holds two of them (\($two.cpu) CPU, \($two.memory | gib) GiB) in its allocatable, and a node the cluster adds may, so whether two of the run's pods can share a node cannot be read"}
    end;
