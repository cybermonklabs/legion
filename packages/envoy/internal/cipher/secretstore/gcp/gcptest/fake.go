// Package gcptest holds the GCP store's fake and the test helpers its tests and agent-secrets'
// tests share: FakeSecretManager, which answers every Secret Manager call the GCP store and its
// writer make, as the real service answers them; Start, which serves it to the broker's own
// client; UseFake, which serves it to the writer; and the helpers that choose the writer's Google
// sign-in and stand in for gcloud (use.go). Only tests import it.
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
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
)

// Project is the project the store's tests hold secrets in, by its number, as Secret Manager
// names it.
const Project = "projects/000000000000"

// Stored is a secret as a store test describes it, with one version at most: its metadata, its
// latest version's state and key, and its value.
type Stored struct {
	Annotations map[string]string
	// Labels are the secret's labels, which `agent-secrets secret create` sets one of
	// (agent-secrets-create): the GCP store reads that one alone (secretstore.CreateLabel), to mark
	// a secret the create left with no version as unfilled (Listing.Unfilled).
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
	// DenyVersion and KeyUnusable are FakeSecret's.
	DenyVersion, KeyUnusable bool
	Value                    []byte
	// Number is the latest version's number, the last segment of the name GetSecretVersion
	// answers for versions/latest; "" is 1.
	Number string
}

// Secret is s as the fake holds the secret name.
func (s Stored) Secret(name string) *FakeSecret {
	secret := &secretmanagerpb.Secret{Name: name, Annotations: s.Annotations, Labels: s.Labels, Etag: "1"}
	auto := &secretmanagerpb.Replication_Automatic{}
	if s.ReplicationKey != "" {
		auto.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: s.ReplicationKey}
	}
	secret.Replication = &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: auto}}
	if !s.Created.IsZero() {
		secret.CreateTime = timestamppb.New(s.Created)
	}
	if !s.Expires.IsZero() {
		secret.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(s.Expires)}
	}
	held := &FakeSecret{Secret: secret, EtagCounter: 1, DenyVersion: s.DenyVersion, KeyUnusable: s.KeyUnusable}
	if s.NoVersion {
		return held
	}
	number := s.Number
	if number == "" {
		number = "1"
	}
	status := &secretmanagerpb.ReplicationStatus_AutomaticStatus{}
	if s.VersionKey != "" {
		status.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: s.VersionKey + "/cryptoKeyVersions/3"}
	}
	held.Versions = []*secretmanagerpb.SecretVersion{{
		Name: name + "/versions/" + number, State: s.State,
		ReplicationStatus: &secretmanagerpb.ReplicationStatus{ReplicationStatus: &secretmanagerpb.ReplicationStatus_Automatic{Automatic: status}},
	}}
	held.Payloads = [][]byte{s.Value}
	return held
}

// NewFake is a fake holding secrets, each by its full resource name.
func NewFake(secrets map[string]Stored) *FakeSecretManager {
	f := NewFakeSecretManager()
	for name, s := range secrets {
		f.Secrets[name] = s.Secret(name)
	}
	return f
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
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})), grpc.UnaryInterceptor(f.Intercept))
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
