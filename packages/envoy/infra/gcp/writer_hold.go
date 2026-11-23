package gcp

import (
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// skipAwaitAnnotation is pulumi-kubernetes' annotation that makes an apply register an object
// without waiting for it to become ready.
const skipAwaitAnnotation = "pulumi.com/skipAwait"

// heldReplacement is the update rule of a Deployment whose pods create JetStream consumers (the
// clustered hub's session listener, and Dispatch's app and listener) while the component's
// replaceUnderHold input is set, for the replacement an operator runs under the writer hold, which
// refuses every NATS client but its own: the Recreate strategy, at any replica count, so every old
// pod has stopped before a new one starts, where a rolling update would keep an old pod serving
// beside a new one that cannot become ready under the hold; and skipAwaitAnnotation on the
// Deployment's own metadata, so the apply does not wait for that readiness, and the operator's
// check of the running pods, not the apply, shows the replacement. It adds the annotation to meta,
// which carries none of its own, and returns the strategy. The pod template is untouched, so the
// rule rolls no pod by itself.
func heldReplacement(meta *metav1.ObjectMetaArgs) dispatchStrategy {
	meta.Annotations = pulumi.StringMap{skipAwaitAnnotation: pulumi.String("true")}
	return dispatchStrategy{strategyType: dispatchStrategyRecreate}
}
