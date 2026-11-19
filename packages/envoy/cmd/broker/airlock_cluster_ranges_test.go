// cmd/broker/airlock_cluster_ranges_test.go
// BROKER_AIRLOCK_CLUSTER_RANGES against the REAL compiled broker. config's own rows prove how the
// setting is read and when it is required; these prove that the boot runs that check before it
// binds anything, and that the ranges reach the proxy port main builds, so a tunnel into them is
// refused by a broker started the way a deployment starts one.
package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// clusterServiceHost is the address a cluster's kubernetes Service has in GKE's default Service
// range, 34.118.224.0/20: what Kubernetes puts in KUBERNETES_SERVICE_HOST in every container.
const clusterServiceHost = "34.118.224.1"

// TestMainRefusesToStartInAClusterWithoutItsRanges drives the real binary in a cluster - with
// KUBERNETES_SERVICE_HOST set and the airlock on - with each setting it must refuse, and asserts
// it exits non-zero naming the setting, without ever binding. The database it is given is
// unreachable, so a broker that read the setting after opening the store would fail on the host
// instead of on the setting.
func TestMainRefusesToStartInAClusterWithoutItsRanges(t *testing.T) {
	binary := buildBroker(t)
	for _, row := range []struct {
		name, host, ranges string
		want               []string
	}{
		{"the setting missing", clusterServiceHost, "",
			[]string{"BROKER_AIRLOCK_CLUSTER_RANGES is required", "KUBERNETES_SERVICE_HOST"}},
		{"an element that is no prefix", clusterServiceHost, "34.118.224.0/20, not-a-range",
			[]string{"BROKER_AIRLOCK_CLUSTER_RANGES", "element 2 is not a CIDR prefix"}},
		{"an empty element", clusterServiceHost, "34.118.224.0/20,,198.51.100.0/24",
			[]string{"BROKER_AIRLOCK_CLUSTER_RANGES", "element 2 is empty"}},
		{"a KUBERNETES_SERVICE_HOST that is not an address", "kubernetes.default.svc", "34.118.224.0/20",
			[]string{"BROKER_AIRLOCK_CLUSTER_RANGES", "KUBERNETES_SERVICE_HOST is not an IP address"}},
		{"a range that does not hold KUBERNETES_SERVICE_HOST", clusterServiceHost, "198.51.100.0/24",
			[]string{"BROKER_AIRLOCK_CLUSTER_RANGES", "KUBERNETES_SERVICE_HOST " + clusterServiceHost + " is inside no range"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			out := runRefusedBroker(t, binary, append(airlockEnv(t),
				"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
				"BROKER_PUBLIC_URL=https://secrets.internal.example",
				"BROKER_AIRLOCK_RULES_FILE="+airlockRulesFile(t, `{"secrets": [{"name": "DEEL_API_KEY", "hosts": ["api.deel.com"]}]}`),
				"KUBERNETES_SERVICE_HOST="+row.host,
				"BROKER_AIRLOCK_CLUSTER_RANGES="+row.ranges,
			)...)
			for _, want := range row.want {
				if !strings.Contains(out, want) {
					t.Fatalf("broker refused, but its output does not say %q: %s", want, out)
				}
			}
			if strings.Contains(out, "not-a-range") {
				t.Fatalf("the refusal quotes an element that is no CIDR prefix: %s", out)
			}
		})
	}
}

// TestMainInAClusterStartsWithItsRangesAndItsProxyRefusesThem: with ranges that hold
// KUBERNETES_SERVICE_HOST the broker starts, and a CONNECT through its proxy port, with the proxy
// password of a real grant, is refused for an address inside one of those ranges before anything
// is dialled. The control is the same password's CONNECT to the host the grant's entry binds,
// which the proxy takes over. The address inside the range is a documentation address no service
// answers: a proxy that did not refuse it would dial it and answer nothing within the deadline.
func TestMainInAClusterStartsWithItsRangesAndItsProxyRefusesThem(t *testing.T) {
	const (
		podAudience  = "example-broker-pod"
		service      = "example-service"
		loginAccount = "system:serviceaccount:example-system:example-service-login"
		podAccount   = "system:serviceaccount:example:example-worker"
		secretName   = "AIRLOCK_TOKEN"
		// boundHost is the name the entry binds, as the reread row's is: a rules host is a host
		// name, since a client sends no TLS server name for an address. BROKER_AIRLOCK_TEST_RESOLVE
		// reads it as boundAddress, RFC 5737's TEST-NET-1, so the proxy's address check needs no
		// resolver, and a CONNECT to it is taken over without a dial: a proxy that dialled it
		// instead would reach nothing and answer nothing within the deadline.
		boundHost    = "bound.airlock.test"
		boundAddress = "192.0.2.1"
		// insideRange is in the second range below, 198.51.100.0/24, which is RFC 5737's
		// TEST-NET-2: global unicast, so only the setting refuses it.
		insideRange = "198.51.100.7"
	)
	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	writeWhole(t, fakeSecretsFile, `{"secrets": [
		{"name": "`+testSecretsPrefix+`airlock-token", "kms_key_id": "`+testSecretsKMSKeyARN+`",
		 "tags": {"owner": "`+service+`", "tier": "agent"}, "value": "the-real-airlock-token"}
	]}`)
	broker := startBroker(t, brokerOptions{Env: append(airlockEnv(t),
		"BROKER_DATABASE_URL="+storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER="+issuer.URL(),
		"BROKER_K8S_OIDC_AUDIENCE="+podAudience,
		"BROKER_SERVICE_LOGINS="+service+"="+loginAccount,
		"BROKER_SERVICES="+service+"="+podAccount,
		"BROKER_AIRLOCK_RULES_FILE="+airlockRulesFile(t,
			`{"secrets": [{"name": "`+secretName+`", "hosts": ["`+boundHost+`"], "headers": ["authorization"]}]}`),
		"BROKER_AIRLOCK_TEST_RESOLVE="+boundHost+"="+boundAddress,
		"KUBERNETES_SERVICE_HOST="+clusterServiceHost,
		"BROKER_AIRLOCK_CLUSTER_RANGES=34.118.224.0/20, 198.51.100.0/24",
	)})
	requireHealthy(t, broker.Addr)
	proxyAddr := airlockProxyAddr(t, broker)

	api := podBroker{t: t, base: "http://" + broker.Addr, issuer: issuer, key: podKey, audience: podAudience}
	launcher := api.logIn(service, loginAccount)
	p := api.enroll(launcher, "airlock-pod", podAccount, "")
	granted := api.ask(p, secretName)
	if granted.GrantID == nil {
		t.Fatalf("the pod's request for %s = %+v, want a grant at once", secretName, granted)
	}
	password, _ := airlockOf(api, p, *granted.GrantID)
	if password == "" {
		t.Fatal("the release carries no proxy password")
	}

	status, body := connectThrough(t, proxyAddr, password, net.JoinHostPort(insideRange, "443"))
	if status != http.StatusForbidden || !strings.Contains(body, "inside BROKER_AIRLOCK_CLUSTER_RANGES, the broker cluster's own addresses") {
		t.Fatalf("CONNECT %s through the broker's proxy = %d %q; want 403 naming BROKER_AIRLOCK_CLUSTER_RANGES", insideRange, status, body)
	}
	if status, body := connectThrough(t, proxyAddr, password, net.JoinHostPort(boundHost, "443")); status != http.StatusOK {
		t.Fatalf("control: CONNECT %s, the bound host, through the broker's proxy = %d %q; want 200", boundHost, status, body)
	}
}

// TestMainOutsideAClusterStartsWithoutTheRanges: with KUBERNETES_SERVICE_HOST unset - a dev box,
// a local run - the setting is optional, and a broker with the airlock on and no ranges starts.
func TestMainOutsideAClusterStartsWithoutTheRanges(t *testing.T) {
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: append(airlockEnv(t),
		"BROKER_DATABASE_URL="+storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE="+fakeSecretsFile,
		"BROKER_AIRLOCK_RULES_FILE="+airlockRulesFile(t, `{"secrets": [{"name": "DEEL_API_KEY", "hosts": ["api.deel.com"]}]}`),
		"KUBERNETES_SERVICE_HOST=",
		"BROKER_AIRLOCK_CLUSTER_RANGES=",
	)})
	requireHealthy(t, broker.Addr)
}

// connectThrough writes one CONNECT for target to the proxy at proxyAddr with password as its
// proxy password, and answers the status and, for any answer but 200 (whose body is the tunnel),
// the body. No answer within ten seconds fails t: a proxy that is still dialling an address no
// service answers has not refused it.
func connectThrough(t *testing.T, proxyAddr, password, target string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial the broker's proxy port: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	credential := base64.StdEncoding.EncodeToString([]byte("agent:" + password))
	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: Basic " + credential + "\r\n\r\n")); err != nil {
		t.Fatalf("write the CONNECT: %v", err)
	}
	answer, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("CONNECT %s: no answer within 10s; the proxy is dialling it rather than refusing it", target)
		}
		t.Fatalf("read the answer to CONNECT %s: %v", target, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode == http.StatusOK {
		return answer.StatusCode, ""
	}
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the refusal of CONNECT %s: %v", target, err)
	}
	return answer.StatusCode, string(body)
}
