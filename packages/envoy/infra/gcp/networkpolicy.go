package gcp

import (
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// labelPeer is a NetworkPolicy peer admitting the pods of the policy's own namespace that wear
// labels.
func labelPeer(labels map[string]string) networkingv1.NetworkPolicyPeerArgs {
	return networkingv1.NetworkPolicyPeerArgs{PodSelector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.ToStringMap(labels)}}
}

// ipBlockPeers are one ipBlock peer for each of cidrs, in their order: the sources a load balancer
// reaches a pod from, which no namespace or pod selector names.
func ipBlockPeers(cidrs []string) networkingv1.NetworkPolicyPeerArray {
	peers := make(networkingv1.NetworkPolicyPeerArray, 0, len(cidrs))
	for _, cidr := range cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeerArgs{IpBlock: &networkingv1.IPBlockArgs{Cidr: pulumi.String(cidr)}})
	}
	return peers
}

// namespacePeers are NetworkPolicy peers admitting every pod of each namespace named, by the
// kubernetes.io/metadata.name label Kubernetes sets on every namespace.
func namespacePeers(namespaces []string) networkingv1.NetworkPolicyPeerArray {
	peers := make(networkingv1.NetworkPolicyPeerArray, 0, len(namespaces))
	for _, namespace := range namespaces {
		peers = append(peers, networkingv1.NetworkPolicyPeerArgs{
			NamespaceSelector: &metav1.LabelSelectorArgs{
				MatchLabels: pulumi.StringMap{"kubernetes.io/metadata.name": pulumi.String(namespace)},
			},
		})
	}
	return peers
}

// ingressFrom is an ingress rule admitting peers to TCP port. A rule with an empty from admits
// every source, so a caller with no peer to name writes no rule.
func ingressFrom(port int, peers networkingv1.NetworkPolicyPeerArray) networkingv1.NetworkPolicyIngressRuleArgs {
	return networkingv1.NetworkPolicyIngressRuleArgs{
		From:  peers,
		Ports: networkingv1.NetworkPolicyPortArray{networkingv1.NetworkPolicyPortArgs{Port: pulumi.Int(port)}},
	}
}

// networkPolicy registers NetworkPolicy name in namespace: an Ingress policy on the pods wearing
// pods' labels, admitting what ingress names and nothing else.
func networkPolicy(ctx *pulumi.Context, name, namespace string, pods map[string]string, ingress networkingv1.NetworkPolicyIngressRuleArray, opts ...pulumi.ResourceOption) error {
	_, err := networkingv1.NewNetworkPolicy(ctx, name, &networkingv1.NetworkPolicyArgs{
		Metadata: objectMeta(name, namespace),
		Spec: &networkingv1.NetworkPolicySpecArgs{
			PodSelector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.ToStringMap(pods)},
			PolicyTypes: pulumi.StringArray{pulumi.String("Ingress")},
			Ingress:     ingress,
		},
	}, opts...)
	return err
}
