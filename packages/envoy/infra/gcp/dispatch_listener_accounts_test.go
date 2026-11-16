package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// With listenerAudience the listener verifies the app's projected token by its issuer and
// audience, and the cluster mints a token for that audience to any workload that asks for one. So
// the listener holds its projected-token branch to the app's own account (ENVOY_OIDC_ACCOUNTS,
// cmd/listener), and a token another workload of the cluster mints for the same audience is
// refused rather than admitted. Without listenerAudience the listener carries no such setting and
// renders as before.
//
// Found by t-e-14's acceptance at 6f0b223383: on the real listener binary with the pair
// gcp:Dispatch renders, a token of system:serviceaccount:example-ns:another-workload for the
// listener audience was answered 200 on GET /v1/sessions.
func TestDispatchsListenerAdmitsOnlyTheAppsAccount(t *testing.T) {
	m, err := runDispatch(t, dispatchServiceIdentityArgs(), true)
	require.NoError(t, err)
	listener := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch-listener"), "envoy-listener"))
	require.Equal(t, "system:serviceaccount:dispatch:example-dispatch", listener["ENVOY_OIDC_ACCOUNTS"],
		"with listenerAudience the listener admits the app's own account alone: ENVOY_OIDC_ACCOUNTS names the subject of ServiceAccount <hostname> in <namespace>")

	m, err = runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	listener = containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch-listener"), "envoy-listener"))
	require.NotContains(t, listener, "ENVOY_OIDC_ACCOUNTS", "without listenerAudience the listener renders as before")
}
