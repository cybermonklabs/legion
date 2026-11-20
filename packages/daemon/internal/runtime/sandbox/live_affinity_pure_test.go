// The reservation rule the Stage 4a live test holds its pods to (guaranteedPod), and the one
// affinity it tolerates (isGKEExtendedDurationAffinity), with their table tests. They read no
// cluster, so they sit apart from the live files the kind job runs whole, and daemon-go runs these
// tests with the package's others.

package sandbox

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// isGKEExtendedDurationAffinity reports whether a carries exactly the NodeAffinity GKE Autopilot
// adds on its own to a pod carrying `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`
// (manifest.go's safeToEvictAnnotation, every issue pod's, never the probe's — probeManifest's
// own doc says why) to protect it from disruption for up to seven days (confirmed live,
// 2026-10-17: a real root issue pod on the production GKE cluster carried exactly this one
// required NodeAffinity term, keyed `cloud.google.com/extended-duration-pods`, nothing else —
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/extended-duration-pods). It is
// GKE's own addition, not a pod affinity or anti-affinity the pod spec asks for, and it still
// admits the pod to any node whose pool carries that label, never a named one — so it does not
// trip the reservation rule's intent (no pod asks for or keeps off a particular node) even though
// it is literally a NodeAffinity. AWS adds none; a pod this doesn't match is still refused.
//
// The `In` operator's own values are not a fixed, tolerable literal: confirmed live, 2026-10-17,
// on this round's own six-role root pod, GKE set them to `["2500m", "X"]` — the first exactly the
// pod's own summed regular-container cpu limit (250+250+500+500+750+250m), the second unexplained
// — where an earlier run's single-container probe pod case (never exercised here; the probe always
// passes extendedDurationAffinity=false) might plausibly carry something else again. GKE computes
// these per pod, not from a fixed set this harness could enumerate, so any non-empty `In` is
// tolerated; only the key and the operator (`In` with values, or `Exists` with none) decide
// whether this is the one shape GKE itself would still admit on its own — a `NotIn` or
// `DoesNotExist` on the same key is a refusal GKE would admit too, but meaning the opposite, and
// is never tolerated here regardless of its own values.
func isGKEExtendedDurationAffinity(a *corev1.Affinity) bool {
	if a == nil || a.PodAffinity != nil || a.PodAntiAffinity != nil || a.NodeAffinity == nil {
		return false
	}
	na := a.NodeAffinity
	if len(na.PreferredDuringSchedulingIgnoredDuringExecution) != 0 || na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	terms := na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 0 || len(terms[0].MatchExpressions) != 1 {
		return false
	}
	expr := terms[0].MatchExpressions[0]
	if expr.Key != "cloud.google.com/extended-duration-pods" {
		return false
	}
	switch expr.Operator {
	case corev1.NodeSelectorOpIn:
		return len(expr.Values) > 0
	case corev1.NodeSelectorOpExists:
		return len(expr.Values) == 0
	default:
		return false
	}
}

// guaranteedPod is the reservation rule the rig's pods are held to, since the rig configures no
// cpu or memory request below a limit (liveIssueOverride): it carries no affinity but GKE's own
// extended-duration one when extendedDurationAffinity allows it (isGKEExtendedDurationAffinity's
// doc has the evidence; the kind rig's checks always pass false), its qosClass is Guaranteed, and
// every container — the init containers included — reserves cpu and memory with the request equal
// to the limit and at least the reservation configured for it (cpu floored at cpuFloor), and
// carries the ephemeral-storage request and limit configured for it, the request under the limit,
// except that ephemeralStorageClamped asks for the limit to equal the request instead of the
// configured limit (the clamp GKE Autopilot's admission makes): want's entry for each regular
// container by name, and init for every init container (nil when the pod has none to hold). A
// container want does not name is a refusal: nothing a Legion pod runs is unreserved.
func guaranteedPod(pod *corev1.Pod, want map[string]corev1.ResourceRequirements, init *corev1.ResourceRequirements, cpuFloor resource.Quantity, ephemeralStorageClamped, extendedDurationAffinity bool) error {
	if pod.Spec.Affinity != nil && !(extendedDurationAffinity && isGKEExtendedDurationAffinity(pod.Spec.Affinity)) {
		return fmt.Errorf("carries an affinity, which no Legion pod asks for: %+v", pod.Spec.Affinity)
	}
	if pod.Status.QOSClass != corev1.PodQOSGuaranteed {
		return fmt.Errorf("is %q, want %s", pod.Status.QOSClass, corev1.PodQOSGuaranteed)
	}
	check := func(c corev1.Container, expected corev1.ResourceRequirements, source string) error {
		for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			request, limit := c.Resources.Requests[resourceName], c.Resources.Limits[resourceName]
			reserved := expected.Requests[resourceName]
			if resourceName == corev1.ResourceCPU && reserved.Cmp(cpuFloor) < 0 {
				reserved = cpuFloor
			}
			if request.IsZero() || limit.IsZero() {
				return fmt.Errorf("container %s reserves no %s: requests %v, limits %v", c.Name, resourceName, c.Resources.Requests, c.Resources.Limits)
			}
			if request.Cmp(limit) != 0 {
				return fmt.Errorf("container %s requests %s %s but is limited to %s; this run configures request = limit", c.Name, resourceName, request.String(), limit.String())
			}
			if request.Cmp(reserved) != 0 {
				return fmt.Errorf("container %s reserves %s %s, not the %s configured for %s", c.Name, resourceName, request.String(), reserved.String(), source)
			}
		}
		request, limit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]
		wantRequest, wantLimit := expected.Requests[corev1.ResourceEphemeralStorage], expected.Limits[corev1.ResourceEphemeralStorage]
		if ephemeralStorageClamped {
			wantLimit = wantRequest
		}
		switch {
		case request.IsZero() || limit.IsZero():
			return fmt.Errorf("container %s bounds no ephemeral-storage: requests %v, limits %v", c.Name, c.Resources.Requests, c.Resources.Limits)
		case request.Cmp(limit) > 0:
			return fmt.Errorf("container %s requests ephemeral-storage %s past its limit %s", c.Name, request.String(), limit.String())
		case request.Cmp(wantRequest) != 0 || limit.Cmp(wantLimit) != 0:
			return fmt.Errorf("container %s bounds ephemeral-storage at %s under a limit of %s, not the %s under %s configured for %s", c.Name, request.String(), limit.String(), wantRequest.String(), wantLimit.String(), source)
		}
		return nil
	}
	for _, c := range pod.Spec.InitContainers {
		if init == nil {
			return fmt.Errorf("init container %s: the pod is expected to run none", c.Name)
		}
		if err := check(c, *init, "the launching role"); err != nil {
			return fmt.Errorf("init container: %w", err)
		}
	}
	for _, c := range pod.Spec.Containers {
		expected, ok := want[c.Name]
		if !ok {
			return fmt.Errorf("container %s names no role a reservation is configured for", c.Name)
		}
		if err := check(c, expected, "the "+c.Name); err != nil {
			return err
		}
	}
	return nil
}

// TestIsGKEExtendedDurationAffinity covers the matcher's whole class of mistake: checking only the
// key of the one tolerated NodeAffinity shape, not its operator, would accept a refusal
// (NotIn/DoesNotExist on the same key) wearing the one shape meant to be tolerated. The `In`
// operator's own values are GKE's to pick per pod (confirmed live, 2026-10-17: a six-role root pod
// carried `["2500m", "X"]`, not a fixed literal), so any non-empty value list is tolerated.
func TestIsGKEExtendedDurationAffinity(t *testing.T) {
	key := "cloud.google.com/extended-duration-pods"
	required := func(exprs ...corev1.NodeSelectorRequirement) *corev1.Affinity {
		return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: exprs}},
			},
		}}
	}
	for _, tc := range []struct {
		name string
		a    *corev1.Affinity
		want bool
	}{
		{"nil affinity", nil, false},
		{"In [true], an earlier observed GKE shape", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}), true},
		{"In [2500m, X], this round's own live GKE shape, the pod's summed cpu beside an unexplained token", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"2500m", "X"}}), true},
		{"Exists, no values", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpExists}), true},
		{"NotIn [true] on the same key is a refusal, not a tolerance", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}}), false},
		{"DoesNotExist on the same key is a refusal, not a tolerance", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpDoesNotExist}), false},
		{"In with no values at all is not GKE's own shape", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn}), false},
		{"a different key entirely", required(corev1.NodeSelectorRequirement{Key: "some.other/key", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}), false},
		{"a second term is not the one tolerated shape", &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}}},
			}},
		}}, false},
		{"a pod affinity alongside is not the one tolerated shape", &corev1.Affinity{
			NodeAffinity: required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}).NodeAffinity,
			PodAffinity:  &corev1.PodAffinity{},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGKEExtendedDurationAffinity(tc.a); got != tc.want {
				t.Errorf("isGKEExtendedDurationAffinity(%+v) = %v, want %v", tc.a, got, tc.want)
			}
		})
	}
}

// TestGuaranteedPodAffinity covers guaranteedPod's own use of the matcher above: the
// extendedDurationAffinity parameter must gate on the matcher's real verdict, not merely on
// whether the pod carries any NodeAffinity at all.
func TestGuaranteedPodAffinity(t *testing.T) {
	extended := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "cloud.google.com/extended-duration-pods", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
			}}},
		},
	}}
	refusal := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "cloud.google.com/extended-duration-pods", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}},
			}}},
		},
	}}
	quantity := resource.MustParse("1")
	podWith := func(affinity *corev1.Affinity) *corev1.Pod {
		return &corev1.Pod{
			Spec: corev1.PodSpec{
				Affinity: affinity,
				Containers: []corev1.Container{{Name: "only", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
				}}},
			},
			Status: corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
		}
	}
	want := map[string]corev1.ResourceRequirements{"only": {
		Requests: corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
	}}
	for _, tc := range []struct {
		name                     string
		affinity                 *corev1.Affinity
		extendedDurationAffinity bool
		wantErr                  bool
	}{
		{"no affinity, flag false: fine", nil, false, false},
		{"no affinity, flag true: still fine", nil, true, false},
		{"the real shape, flag true: tolerated", extended, true, false},
		{"the real shape, flag false: refused — the probe's own call site passes false", extended, false, true},
		{"a refusal wearing the same key, flag true: still refused, the matcher's own job", refusal, true, true},
		{"some other affinity entirely, flag true: refused", &corev1.Affinity{PodAffinity: &corev1.PodAffinity{}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := guaranteedPod(podWith(tc.affinity), want, nil, resource.Quantity{}, false, tc.extendedDurationAffinity)
			if (err != nil) != tc.wantErr {
				t.Errorf("guaranteedPod(affinity=%+v, extendedDurationAffinity=%v) = %v, wantErr %v", tc.affinity, tc.extendedDurationAffinity, err, tc.wantErr)
			}
		})
	}
}
