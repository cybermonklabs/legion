// Package gcptest holds the GCP store's fake: FakeSecretManager, which answers the Secret Manager
// calls the GCP store makes over gRPC with TLS as the real service answers them, and Start, which
// serves it to the broker's own client. Only tests import it.
package gcptest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
)

// Project is the one project the fake holds secrets in, by its number, as Secret Manager names it.
const Project = "projects/000000000000"

// FakeSecret is one secret the fake Secret Manager holds: its metadata, its latest version's state
// and key, and its value.
type FakeSecret struct {
	Annotations map[string]string
	// Labels are the secret's labels, which `agent-secrets secret create` sets one of
	// (agent-secrets-create) and the broker reads none of.
	Labels map[string]string
	// ReplicationKey is the key the secret's replication policy names; "" is Google's default.
	ReplicationKey string
	// VersionKey is the key the latest version was encrypted with; "" is Google's default.
	VersionKey string
	// NoVersion is a secret with no version at all.
	NoVersion bool
	State     secretmanagerpb.SecretVersion_State
	// Expires, when set, is the secret's expire_time, and Created its create_time.
	Expires, Created time.Time
	// DenyVersion makes GetSecretVersion on the secret answer PERMISSION_DENIED, as for a secret
	// outside the namespace the broker's condition grants it.
	DenyVersion bool
	// KeyUnusable makes AccessSecretVersion answer FAILED_PRECONDITION on an ENABLED version, as
	// an unusable CMEK key might.
	KeyUnusable bool
	Value       []byte
	// Number is the latest version's number, the last segment of the name GetSecretVersion
	// answers for versions/latest; "" is 1.
	Number string
}

// FakeSecretManager is the Secret Manager calls the GCP store makes, over gRPC with TLS, as the
// real service answers them: resource names with the project number, NOT_FOUND for a missing
// secret or version, FAILED_PRECONDITION for a disabled or destroyed version's access. A test
// that reads or changes its fields while a client may call it holds Mu.
type FakeSecretManager struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer
	Mu      sync.Mutex
	Secrets map[string]*FakeSecret // keyed by the secret's resource name
	// FailList, when set, is what ListSecrets answers.
	FailList error
	// Calls counts each method's calls, and Unavailable is how many calls of each method still
	// answer UNAVAILABLE first, as a transient outage does.
	Calls, Unavailable map[string]int
}

// call counts one call of method and answers UNAVAILABLE while Unavailable holds some for it.
// f.Mu is held.
func (f *FakeSecretManager) call(method string) error {
	if f.Calls == nil {
		f.Calls = map[string]int{}
	}
	f.Calls[method]++
	if f.Unavailable[method] > 0 {
		f.Unavailable[method]--
		return status.Error(codes.Unavailable, "transient")
	}
	return nil
}

// Reads is the calls made so far that Secret Manager counts against its 600-per-minute read
// quota: every call but AccessSecretVersion.
func (f *FakeSecretManager) Reads() int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Calls["ListSecrets"] + f.Calls["GetSecret"] + f.Calls["GetSecretVersion"]
}

func (f *FakeSecretManager) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("ListSecrets"); err != nil {
		return nil, err
	}
	if f.FailList != nil {
		return nil, f.FailList
	}
	if req.GetParent() != Project {
		return nil, status.Errorf(codes.NotFound, "project %q", req.GetParent())
	}
	needle, ok := strings.CutPrefix(req.GetFilter(), "name:")
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "filter %q", req.GetFilter())
	}
	resp := &secretmanagerpb.ListSecretsResponse{}
	for name, s := range f.Secrets {
		id := name[strings.LastIndex(name, "/")+1:]
		// Secret Manager's ":" is a case-insensitive substring match.
		if !strings.Contains(strings.ToLower(id), strings.ToLower(needle)) {
			continue
		}
		resp.Secrets = append(resp.Secrets, f.secretProto(name, s))
	}
	resp.TotalSize = int32(len(resp.Secrets))
	return resp, nil
}

func (f *FakeSecretManager) secretProto(name string, s *FakeSecret) *secretmanagerpb.Secret {
	p := &secretmanagerpb.Secret{Name: name, Annotations: s.Annotations, Labels: s.Labels}
	auto := &secretmanagerpb.Replication_Automatic{}
	if s.ReplicationKey != "" {
		auto.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: s.ReplicationKey}
	}
	p.Replication = &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: auto}}
	if !s.Created.IsZero() {
		p.CreateTime = timestamppb.New(s.Created)
	}
	if !s.Expires.IsZero() {
		p.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(s.Expires)}
	}
	return p
}

func (f *FakeSecretManager) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("GetSecret"); err != nil {
		return nil, err
	}
	s, ok := f.Secrets[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Secret [%s] not found", req.GetName())
	}
	return f.secretProto(req.GetName(), s), nil
}

// latest splits "<secret>/versions/latest" and answers the secret, or NOT_FOUND.
func (f *FakeSecretManager) latest(name string) (*FakeSecret, string, error) {
	secret, version, ok := strings.Cut(name, "/versions/")
	if !ok || version != "latest" {
		return nil, "", status.Errorf(codes.InvalidArgument, "version name %q", name)
	}
	s, found := f.Secrets[secret]
	if !found || s.NoVersion {
		return nil, "", status.Errorf(codes.NotFound, "Secret Version [%s] not found", name)
	}
	return s, secret, nil
}

// number is s's latest version's number.
func (s *FakeSecret) number() string {
	if s.Number == "" {
		return "1"
	}
	return s.Number
}

func (f *FakeSecretManager) GetSecretVersion(_ context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("GetSecretVersion"); err != nil {
		return nil, err
	}
	s, secret, err := f.latest(req.GetName())
	if err != nil {
		return nil, err
	}
	if s.DenyVersion {
		return nil, status.Errorf(codes.PermissionDenied, "Permission denied on %s", req.GetName())
	}
	v := &secretmanagerpb.SecretVersion{Name: secret + "/versions/" + s.number(), State: s.State}
	auto := &secretmanagerpb.ReplicationStatus_AutomaticStatus{}
	if s.VersionKey != "" {
		auto.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: s.VersionKey + "/cryptoKeyVersions/3"}
	}
	v.ReplicationStatus = &secretmanagerpb.ReplicationStatus{ReplicationStatus: &secretmanagerpb.ReplicationStatus_Automatic{Automatic: auto}}
	return v, nil
}

func (f *FakeSecretManager) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("AccessSecretVersion"); err != nil {
		return nil, err
	}
	s, secret, err := f.latest(req.GetName())
	if err != nil {
		return nil, err
	}
	if s.State != secretmanagerpb.SecretVersion_ENABLED || s.KeyUnusable {
		return nil, status.Errorf(codes.FailedPrecondition, "Secret Version [%s] is in %s state", req.GetName(), s.State)
	}
	return &secretmanagerpb.AccessSecretVersionResponse{Name: secret + "/versions/1", Payload: &secretmanagerpb.SecretPayload{Data: s.Value}}, nil
}

// ListSecretVersions answers, in one page, the one version the fake keeps of the secret Parent
// names, by its number and state, and none for a secret with no version.
func (f *FakeSecretManager) ListSecretVersions(_ context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("ListSecretVersions"); err != nil {
		return nil, err
	}
	s, ok := f.Secrets[req.GetParent()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Secret [%s] not found", req.GetParent())
	}
	resp := &secretmanagerpb.ListSecretVersionsResponse{}
	if !s.NoVersion {
		resp.Versions = []*secretmanagerpb.SecretVersion{{Name: req.GetParent() + "/versions/" + s.number(), State: s.State}}
	}
	resp.TotalSize = int32(len(resp.Versions))
	return resp, nil
}

// Start serves f over TLS on a loopback port and answers the broker's own Secret Manager client
// (gcp.NewClient, its retries included) dialled to it with no credentials. Both end with t.
func Start(t *testing.T, f *FakeSecretManager) *secretmanager.Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake-secretmanager"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})))
	secretmanagerpb.RegisterSecretManagerServiceServer(srv, f)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})))
	if err != nil {
		t.Fatal(err)
	}
	client, err := gcp.NewClient(context.Background(), option.WithGRPCConn(conn), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
