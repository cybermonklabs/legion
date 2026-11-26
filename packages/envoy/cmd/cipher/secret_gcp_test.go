// packages/envoy/cmd/cipher/secret_gcp_test.go
//
// The GCP secret forms run in-process here, against a fake Secret Manager gRPC server
// (fakeSecretManager) standing in for the real API, with the credential seams
// (gcpCredentialsFile, gcpMetadataToken, gcpMetadataPrincipal) and gcpUserinfo replaced so no
// form makes a network call for its identity, a stub gcloud alone on PATH (useGcloud) where a
// form can reach for a gcloud login, and the shared fake broker (secretBroker, secret_test.go)
// answering GET /v1/settings with the GCP store's shape and the reread.
// secret_gcp_order_test.go runs the order of those sources through the real ones instead.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

const (
	// testGCPProject is a project NUMBER, the form the broker's gcp_project setting and the
	// secrets' own resource-name prefix take.
	testGCPProject = "100000000001"
	testKMSKeyName = "projects/acme-deployment/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	testGCPPrefix  = "projects/100000000001/secrets/agent-secrets-"
)

// gcpTestSettings are a GCP broker's settings, as the clean address https://broker.example
// answered them (Settings.answeredBy), so a refusal a test calls directly names their fields.
func gcpTestSettings() Settings {
	return Settings{
		Store: secrets.StoreGCP, SecretsPrefix: testGCPPrefix,
		KMSKeyName: testKMSKeyName, GCPProject: testGCPProject,
		SecretTypeTag: true,
		answeredBy:    cipheraddr.New("https://broker.example"),
	}
}

// fakeSecret is one secret fakeSecretManager holds: payloads[i] is versions[i]'s value.
type fakeSecret struct {
	secret      *secretmanagerpb.Secret
	versions    []*secretmanagerpb.SecretVersion
	payloads    [][]byte
	etagCounter int
}

// fakeSecretManager is a minimal in-memory Secret Manager server, implementing only the RPCs the
// GCP secret forms call.
type fakeSecretManager struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer

	mu      sync.Mutex
	secrets map[string]*fakeSecret // full resource name -> secret
	// refuseAnnotationUpdates, when set, makes UpdateSecret refuse an annotations-field-mask
	// request with PermissionDenied, as IAM refuses a person changing a shared secret's tags.
	refuseAnnotationUpdates bool
	// race, when set, runs once on the secret named by the first GetSecret or UpdateSecret after the
	// first GetSecret, before that call is answered, as another writer's update landing between a
	// read and the next call does; gets counts GetSecret calls.
	race  func(*fakeSecret)
	gets  int
	raced bool
	// refuse, when set, is an authorization every call carrying it is refused with
	// PermissionDenied, as IAM refuses an identity it grants no role on the secrets.
	refuse string
	// refuseMethods is, for each method it names (CreateSecret, AddSecretVersion, DeleteSecret,
	// ...), the code every call of that method is refused with, as IAM refuses an identity one
	// permission it does not grant while granting it the others.
	refuseMethods map[string]codes.Code
	// answerAfter is, for each method it names, the code every call of that method is answered
	// with once the fake has applied it, as a call the server carried out and whose answer was lost
	// (DeadlineExceeded, Unavailable) reaches the caller. beforeCall, when set, runs with each
	// call's method before the fake answers it, outside f.mu.
	answerAfter map[string]codes.Code
	beforeCall  func(method string)
	afterCall   func(method string)
	// authorizations is the authorization metadata each call carried, in order: the sign-in it
	// ran under (recordAuthorization). methods is the method each call named, in the same order.
	authorizations []string
	methods        []string
	// refuseAccess, when not OK, is the code every AccessSecretVersion answers: PERMISSION_DENIED
	// as IAM answers a sign-in without secretmanager.versions.access, or any other failure;
	// accesses counts the AccessSecretVersion calls, answered or refused.
	refuseAccess codes.Code
	accesses     int
	// withheld are the permissions TestIamPermissions answers the caller does not hold, of those
	// it asks about; it answers every other one held, as IAM does for a sign-in that holds them.
	withheld []string
}

// raceOnce runs f.race on entry once, on a call after the first GetSecret.
func (f *fakeSecretManager) raceOnce(entry *fakeSecret) {
	if f.race != nil && !f.raced && f.gets > 0 {
		f.raced = true
		f.race(entry)
	}
}

// recordAuthorization notes the authorization metadata and the method of each call before the
// fake answers it, as the real API reads the caller's sign-in from it, runs f.beforeCall, refuses
// a call carrying f.refuse, or naming a method f.refuseMethods refuses, and answers a call naming
// a method f.answerAfter names with that code once it has applied it.
func (f *fakeSecretManager) recordAuthorization(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	authorization := strings.Join(md.Get("authorization"), ",")
	method := info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]
	f.mu.Lock()
	f.authorizations = append(f.authorizations, authorization)
	f.methods = append(f.methods, method)
	refused := f.refuse != "" && authorization == f.refuse
	code, after, before := f.refuseMethods[method], f.answerAfter[method], f.beforeCall
	f.mu.Unlock()
	if before != nil {
		before(method)
	}
	if refused {
		return nil, status.Errorf(codes.PermissionDenied, "Permission denied on %s for the caller", info.FullMethod)
	}
	if code != codes.OK {
		return nil, status.Errorf(code, "the fake refuses %s", info.FullMethod)
	}
	resp, err := handler(ctx, req)
	if f.afterCall != nil {
		f.afterCall(method)
	}
	if err == nil && after != codes.OK {
		return nil, status.Errorf(after, "the fake applied %s and answers it failed", info.FullMethod)
	}
	return resp, err
}

// called is the method of every call so far, in order, refused ones included.
func (f *fakeSecretManager) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.methods)
}

// calledUnder is the authorization every call so far carried, and how many calls there were; ""
// when the calls carried different ones or none was made.
func (f *fakeSecretManager) calledUnder() (authorization string, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.authorizations {
		if i > 0 && a != authorization {
			return "", len(f.authorizations)
		}
		authorization = a
	}
	return authorization, len(f.authorizations)
}

// written is how many secrets f holds.
func (f *fakeSecretManager) written() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.secrets)
}

func newFakeSecretManager() *fakeSecretManager {
	return &fakeSecretManager{secrets: map[string]*fakeSecret{}}
}

func (f *fakeSecretManager) CreateSecret(_ context.Context, req *secretmanagerpb.CreateSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := req.GetParent() + "/secrets/" + req.GetSecretId()
	if _, ok := f.secrets[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "already exists")
	}
	secret := req.GetSecret()
	secret.Name = name
	secret.Etag = "1"
	f.secrets[name] = &fakeSecret{secret: secret, etagCounter: 1}
	return secret, nil
}

func (f *fakeSecretManager) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetName()]
	if ok {
		f.raceOnce(entry)
	}
	f.gets++
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return entry.secret, nil
}

// UpdateSecret refuses an Etag that does not match the secret's current one with
// FAILED_PRECONDITION, as the real API does for optimistic concurrency, and bumps the etag on
// every applied update; a delete or restore, which sends no Etag, skips the check.
func (f *fakeSecretManager) UpdateSecret(_ context.Context, req *secretmanagerpb.UpdateSecretRequest) (*secretmanagerpb.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path == "annotations" && f.refuseAnnotationUpdates {
			return nil, status.Error(codes.PermissionDenied, "caller does not have permission to update this secret's annotations")
		}
	}
	entry, ok := f.secrets[req.GetSecret().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	f.raceOnce(entry)
	if etag := req.GetSecret().GetEtag(); etag != "" && etag != entry.secret.GetEtag() {
		return nil, status.Error(codes.FailedPrecondition, "etag mismatch")
	}
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "annotations":
			entry.secret.Annotations = req.GetSecret().GetAnnotations()
		case "expire_time":
			entry.secret.Expiration = req.GetSecret().GetExpiration()
		}
	}
	entry.etagCounter++
	entry.secret.Etag = strconv.Itoa(entry.etagCounter)
	return entry.secret, nil
}

func (f *fakeSecretManager) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &secretmanagerpb.ListSecretsResponse{}
	for name, entry := range f.secrets {
		if strings.HasPrefix(name, req.GetParent()+"/secrets/") {
			resp.Secrets = append(resp.Secrets, entry.secret)
		}
	}
	return resp, nil
}

func (f *fakeSecretManager) AddSecretVersion(_ context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetParent()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	number := len(entry.versions) + 1
	name := req.GetParent() + "/versions/" + strconv.Itoa(number)
	version := &secretmanagerpb.SecretVersion{Name: name, State: secretmanagerpb.SecretVersion_ENABLED}
	entry.versions = append(entry.versions, version)
	entry.payloads = append(entry.payloads, req.GetPayload().GetData())
	return version, nil
}

// DeleteSecret deletes the secret outright, with every version it holds, as the real API does.
func (f *fakeSecretManager) DeleteSecret(_ context.Context, req *secretmanagerpb.DeleteSecretRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	if etag := req.GetEtag(); etag != "" && etag != entry.secret.GetEtag() {
		return nil, status.Error(codes.FailedPrecondition, "etag mismatch")
	}
	delete(f.secrets, req.GetName())
	return &emptypb.Empty{}, nil
}

// AccessSecretVersion answers versions/latest, the one version name the forms access, with the
// payload AddSecretVersion stored for the newest version, or refuseAccess when that is set.
func (f *fakeSecretManager) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accesses++
	if f.refuseAccess != codes.OK {
		return nil, status.Error(f.refuseAccess, "the fake refuses access to "+req.GetName())
	}
	parent, latest := strings.CutSuffix(req.GetName(), "/versions/latest")
	entry, ok := f.secrets[parent]
	if !latest || !ok || len(entry.versions) == 0 {
		return nil, status.Error(codes.NotFound, "not found")
	}
	newest := len(entry.versions) - 1
	var data []byte
	if newest < len(entry.payloads) {
		data = entry.payloads[newest]
	}
	return &secretmanagerpb.AccessSecretVersionResponse{Name: entry.versions[newest].GetName(), Payload: &secretmanagerpb.SecretPayload{Data: data}}, nil
}

func (f *fakeSecretManager) GetSecretVersion(_ context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := req.GetName()[:strings.LastIndex(req.GetName(), "/versions/")]
	entry, ok := f.secrets[parent]
	if !ok || len(entry.versions) == 0 {
		return nil, status.Error(codes.NotFound, "not found")
	}
	if strings.HasSuffix(req.GetName(), "/versions/latest") {
		return entry.versions[len(entry.versions)-1], nil
	}
	for _, v := range entry.versions {
		if v.GetName() == req.GetName() {
			return v, nil
		}
	}
	return nil, status.Error(codes.NotFound, "not found")
}

func (f *fakeSecretManager) ListSecretVersions(_ context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[req.GetParent()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &secretmanagerpb.ListSecretVersionsResponse{Versions: entry.versions}, nil
}

// TestIamPermissions answers, of the permissions asked about on the secret, every one f does not
// withhold, as IAM answers a sign-in the permissions its roles grant on that resource. It refuses a
// secret it does not hold NotFound.
func (f *fakeSecretManager) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.secrets[req.GetResource()]; !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	held := slices.DeleteFunc(slices.Clone(req.GetPermissions()), func(p string) bool { return slices.Contains(f.withheld, p) })
	return &iampb.TestIamPermissionsResponse{Permissions: held}, nil
}

// startFakeSecretManager serves f over a real gRPC server on loopback for the rest of t, and
// points gcpClientDial at it. The fake takes no authentication, but each call carries the bearer
// token of the credentials it was dialed with (bearerOf), which the fake records
// (recordAuthorization), so a test sees which sign-in a call ran under.
func startFakeSecretManager(t *testing.T, f *fakeSecretManager) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(f.recordAuthorization))
	secretmanagerpb.RegisterSecretManagerServiceServer(server, f)
	go server.Serve(lis)
	t.Cleanup(server.Stop)

	useGCPDialTo(t, lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// replaceGCPDial leaves the rest of t with gcpClientDial as dial, and puts the real one back after:
// the one way a test points the Secret Manager client at a stand-in.
func replaceGCPDial(t *testing.T, dial func(ctx context.Context, creds *google.Credentials, interceptor grpc.UnaryClientInterceptor) (*secretmanager.Client, error)) {
	t.Helper()
	restore := gcpClientDial
	gcpClientDial = dial
	t.Cleanup(func() { gcpClientDial = restore })
}

// useGCPDialTo leaves the rest of t with gcpClientDial building its client on a connection to
// target dialed with opts (replaceGCPDial). A client built on a connection dialed here ignores the
// client's own dial options, so the interceptor that words every failure is installed on the
// connection itself, as the real dial installs it, and each call carries the bearer token of the
// credentials it signs in with (bearerOf).
func useGCPDialTo(t *testing.T, target string, opts ...grpc.DialOption) {
	t.Helper()
	replaceGCPDial(t, func(ctx context.Context, creds *google.Credentials, interceptor grpc.UnaryClientInterceptor) (*secretmanager.Client, error) {
		dial := append(slices.Clip(opts), grpc.WithChainUnaryInterceptor(interceptor))
		if creds != nil && creds.TokenSource != nil {
			dial = append(dial, grpc.WithPerRPCCredentials(bearerOf{creds.TokenSource}))
		}
		conn, err := grpc.NewClient(target, dial...)
		if err != nil {
			return nil, err
		}
		return secretmanager.NewClient(ctx, option.WithGRPCConn(conn))
	})
}

// bearerOf hands each call the access token source answers, as the real client does; the fake
// listens without TLS, so it asks for none.
type bearerOf struct{ source oauth2.TokenSource }

func (b bearerOf) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := b.source.Token()
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token.AccessToken}, nil
}

func (bearerOf) RequireTransportSecurity() bool { return false }

// gcpServedBy answers a reread as the broker would from what f holds: absent for a secret it does
// not hold, or one scheduled for deletion (expire_time set, whether or not it has passed - a
// broker reread treats a scheduled deletion as absent the moment it is scheduled, as AWS's
// DeletedDate does), pending-value for one with no version that carries the create label (a
// create waiting for its value: the fake makes every secret now, so it is always young enough),
// no-current-value for any other with no version, and otherwise what its annotations answer
// (servedAs): refused for a type without the GitHub owners it needs, served as its type if not.
func gcpServedBy(f *fakeSecretManager, settings Settings) func(name string) Reread {
	return func(name string) Reread {
		slug, err := policy.NameToSlug(name)
		if err != nil {
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		entry, ok := f.secrets[settings.SecretsPrefix+slug]
		switch {
		case !ok:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case entry.secret.GetExpireTime() != nil:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case len(entry.versions) == 0 && entry.secret.GetLabels()[createLabel] != "":
			return Reread{Name: name, Reason: policy.ReasonPendingValue}
		case len(entry.versions) == 0:
			return Reread{Name: name, Reason: policy.ReasonNoCurrentValue}
		}
		return servedAs(name, entry.secret.GetAnnotations())
	}
}

// useGCPIdentity makes the credential sources answer kind/email for the rest of t, and
// gcpUserinfo email, with no network call. kind "" is the metadata server's identity: it makes
// gcpMetadataToken yield a token and gcpMetadataPrincipal answer email, which signs in only
// where no source before the metadata server is present (a test that also puts a stub gcloud
// holding a login on PATH runs under that login). "authorized_user" is a person's own
// Application Default Credentials, where
// `gcloud auth application-default login` writes them: the well-known credentials file. Any other
// kind is a machine's credentials file GOOGLE_APPLICATION_CREDENTIALS names, of that "type"
// ("service_account" for a key file, whose client_email is email, "external_account" for a
// workload identity federation credential such as a dev box's, which impersonates email). It
// writes the file and makes gcpCredentialsFile answer credentials carrying its JSON. Either way it
// clears CLOUDSDK_AUTH_ACCESS_TOKEN, which would outrank them, and the variables a developer's own
// shell could set to choose a sign-in (GOOGLE_APPLICATION_CREDENTIALS, CLOUDSDK_CORE_ACCOUNT),
// which the commands a form prints carry, and points HOME and CLOUDSDK_CONFIG at an empty
// directory, so no credentials file or gcloud configuration of the developer's own is read; a
// test that calls useGcloud, which does that again, calls it before this.
func useGCPIdentity(t *testing.T, kind, email string) {
	t.Helper()
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	useEmptyGcloudConfigDir(t)
	restoreFile, restoreToken, restorePrincipal, restoreUserinfo := gcpCredentialsFile, gcpMetadataToken, gcpMetadataPrincipal, gcpUserinfo
	t.Cleanup(func() {
		gcpCredentialsFile, gcpMetadataToken, gcpMetadataPrincipal, gcpUserinfo = restoreFile, restoreToken, restorePrincipal, restoreUserinfo
	})
	gcpUserinfo = userinfoOf(email)
	token := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	if kind == "" {
		gcpMetadataToken = func(context.Context) (*google.Credentials, error) {
			return &google.Credentials{TokenSource: token}, nil
		}
		gcpMetadataPrincipal = func(context.Context) string { return email }
		return
	}
	var data string
	switch kind {
	case "service_account":
		data = `{"type":"service_account","client_email":"` + email + `"}`
	case "external_account":
		data = `{"type":"external_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/` + email + `:generateAccessToken"}`
	default:
		data = `{"type":"` + kind + `"}`
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if kind == "authorized_user" {
		path = wellKnownCredentialsFile()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind != "authorized_user" {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	}
	gcpCredentialsFile = func(_ context.Context, named string) (*google.Credentials, error) {
		if named != path {
			return nil, fmt.Errorf("the test wrote no credentials file at %s", named)
		}
		return &google.Credentials{JSON: []byte(data), TokenSource: token}, nil
	}
}

// useEmptyGcloudConfigDir points HOME and CLOUDSDK_CONFIG at an empty directory for the rest of
// t, so no well-known credentials file and no gcloud configuration of the developer's own is
// read, and clears CLOUDSDK_ACTIVE_CONFIG_NAME.
func useEmptyGcloudConfigDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDSDK_CONFIG", filepath.Join(home, ".config", "gcloud"))
	t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "")
}

// useNoADC leaves the rest of t with none of the credentials Application Default Credentials
// would find: no credentials file GOOGLE_APPLICATION_CREDENTIALS names, no well-known file (HOME
// and CLOUDSDK_CONFIG an empty directory), and no metadata server (gcpMetadataToken answers
// googleidentity.ErrNoMetadataServer, as it does off Google Cloud); and it clears
// CLOUDSDK_AUTH_ACCESS_TOKEN.
func useNoADC(t *testing.T) {
	t.Helper()
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	useEmptyGcloudConfigDir(t)
	restoreToken, restorePrincipal, restoreUserinfo := gcpMetadataToken, gcpMetadataPrincipal, gcpUserinfo
	gcpMetadataToken = func(context.Context) (*google.Credentials, error) {
		return nil, fmt.Errorf("%w: the test has none", googleidentity.ErrNoMetadataServer)
	}
	gcpMetadataPrincipal = func(context.Context) string { return "" }
	gcpUserinfo = userinfoOf("")
	t.Cleanup(func() {
		gcpMetadataToken, gcpMetadataPrincipal, gcpUserinfo = restoreToken, restorePrincipal, restoreUserinfo
	})
}

// useLentGCPAccessToken lends token in CLOUDSDK_AUTH_ACCESS_TOKEN for the rest of t, and makes
// userinfo answer email for it.
func useLentGCPAccessToken(t *testing.T, token, email string) {
	t.Helper()
	restoreUserinfo := gcpUserinfo
	gcpUserinfo = userinfoOf(email)
	t.Cleanup(func() { gcpUserinfo = restoreUserinfo })
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", token)
}

// cloudPlatformToken is a lent access token minted for the cloud-platform scope alone, as the
// broker mints a google-login secret's token: it carries no email scope.
const cloudPlatformToken = "lent-cloud-platform-token"

// useCloudPlatformToken lends cloudPlatformToken in CLOUDSDK_AUTH_ACCESS_TOKEN for the rest of t,
// with no Application Default Credentials and no gcloud, and makes userinfo refuse it as Google's
// endpoint refuses every token without an email scope: 401, naming no account.
func useCloudPlatformToken(t *testing.T) {
	t.Helper()
	useNoADC(t)
	useNoGcloud(t)
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", cloudPlatformToken)
	restoreUserinfo := gcpUserinfo
	gcpUserinfo = func(_ context.Context, creds *google.Credentials) (string, error) {
		token, err := creds.TokenSource.Token()
		if err != nil {
			return "", err
		}
		if token.AccessToken == cloudPlatformToken {
			return "", errors.New("read the Google sign-in's email from " + googleUserinfoURL + ": the server answered HTTP 401 UNAUTHENTICATED")
		}
		return restoreUserinfo(context.Background(), creds)
	}
	t.Cleanup(func() { gcpUserinfo = restoreUserinfo })
}

// userinfoOf answers as Google's userinfo endpoint does, the account a token was issued to: the
// account the stub gcloud lent a token for (stubTokenPrefix), and email for any other token.
func userinfoOf(email string) func(context.Context, *google.Credentials) (string, error) {
	return func(_ context.Context, creds *google.Credentials) (string, error) {
		token, err := creds.TokenSource.Token()
		if err != nil {
			return "", err
		}
		if account, ok := strings.CutPrefix(token.AccessToken, stubTokenPrefix); ok {
			return account, nil
		}
		return email, nil
	}
}

// stubTokenPrefix starts every access token the stub gcloud lends (useGcloud): it lends account
// the token stubTokenPrefix + account, so a test reads whose login a call ran under.
const stubTokenPrefix = "stub-gcloud-token-for-"

// machineEmail is the service account a machine's own Application Default Credentials and its
// own gcloud login name.
const machineEmail = "robot@acme-deployment.iam.gserviceaccount.com"

// machineADCKinds are the Application Default Credentials that are a machine's own identity, as
// useGCPIdentity takes them: a service account key file, a workload identity federation credential
// (a dev box's), and the metadata server's identity, which carries no JSON.
var machineADCKinds = []string{"service_account", "external_account", ""}

// Federation logins as `gcloud auth list` names them after `gcloud auth login --cred-file`: by the
// principal identifier token introspection answers, a workforce one ending in the person's email.
const (
	workloadPrincipal  = "principal://iam.googleapis.com/projects/100000000001/locations/global/workloadIdentityPools/acme-pool/subject/robot"
	workforcePrincipal = "principal://iam.googleapis.com/locations/global/workforcePools/acme-staff/subject/ada@example.com"
)

// gcloudVariables are the variables the stub gcloud records from its environment on every run:
// those that choose the login or the token gcloud lends (an access token, an account, impersonation,
// an access token file, a credential file override), and those that keep it from prompting, from
// writing log files and from printing its HTTP traffic.
var gcloudVariables = []string{
	"CLOUDSDK_AUTH_ACCESS_TOKEN",
	"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE",
	"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
	"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT",
	"CLOUDSDK_CORE_ACCOUNT",
	"CLOUDSDK_CORE_DISABLE_FILE_LOGGING",
	"CLOUDSDK_CORE_DISABLE_PROMPTS",
	"CLOUDSDK_CORE_LOG_HTTP",
}

// gcloudStub is a stand-in gcloud alone on PATH (useGcloud).
type gcloudStub struct{ dir string }

// useGcloud puts a stub gcloud alone on PATH for the rest of t, holding a login for each of
// accounts. It answers `auth list` with them as `gcloud auth list --format=json` lists them, the
// one CLOUDSDK_CORE_ACCOUNT names marked active, as gcloud marks its core/account property, which
// that variable sets - so none is active where it names one gcloud holds no login for - and the
// first one where it is unset. It answers `config list` with that account and the auth properties
// its environment sets, as gcloud shows environment overrides there, and `auth print-access-token
// … <account>` with stubTokenPrefix plus that account (the last word that is not a flag). It
// refuses any other command, and records every command line it runs and, for each run,
// gcloudVariables as its environment holds them. It also clears the variables a developer's own
// shell could set to steer the sign-in, GOOGLE_APPLICATION_CREDENTIALS, CLOUDSDK_CORE_ACCOUNT,
// CLOUDSDK_ACTIVE_CONFIG_NAME and CLOUDSDK_AUTH_ACCESS_TOKEN, and points HOME and CLOUDSDK_CONFIG
// at an empty directory, so no test reaches the developer's own gcloud or credentials.
func useGcloud(t *testing.T, accounts ...string) gcloudStub {
	t.Helper()
	dir := t.TempDir()
	words := make([]string, len(accounts))
	for i, account := range accounts {
		words[i] = shellWord(account)
	}
	first := ""
	if len(accounts) > 0 {
		first = shellWord(accounts[0])
	}
	environments := shellWord(filepath.Join(dir, "environments"))
	script := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "$*" >>` + shellWord(filepath.Join(dir, "commands")),
		`for name in ` + strings.Join(gcloudVariables, " ") + `; do eval "value=\${$name-unset}"; printf '%s=%s\n' "$name" "$value"; done >>` + environments,
		`printf '%s\n' -- >>` + environments,
		`case "$1 $2" in`,
		`"auth list") active=${CLOUDSDK_CORE_ACCOUNT:-` + first + `}; sep=; printf '['; for account in ` + strings.Join(words, " ") + `; do status=; [ "$account" = "$active" ] && status=ACTIVE; printf '%s{"account":"%s","status":"%s"}' "$sep" "$account" "$status"; sep=,; done; printf ']\n' ;;`,
		`"config list") printf '{"auth":{"impersonate_service_account":"%s","access_token_file":"%s","credential_file_override":"%s"},"core":{"account":"%s"}}\n' "$CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT" "$CLOUDSDK_AUTH_ACCESS_TOKEN_FILE" "$CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE" "${CLOUDSDK_CORE_ACCOUNT:-` + first + `}" ;;`,
		`"auth print-access-token") for word; do case "$word" in --*) ;; *) account=$word ;; esac; done; printf '` + stubTokenPrefix + `%s\n' "$account" ;;`,
		`*) printf 'stub gcloud: unexpected command: %s\n' "$*" >&2; exit 2 ;;`,
		"esac",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
	useEmptyGcloudConfigDir(t)
	return gcloudStub{dir}
}

// environments is, for each run of the stub in order, gcloudVariables as its environment held
// them, "unset" for one it did not hold.
func (g gcloudStub) environments(t *testing.T) []map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "environments"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs []map[string]string
	run := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--" {
			runs, run = append(runs, run), map[string]string{}
			continue
		}
		name, value, _ := strings.Cut(line, "=")
		run[name] = value
	}
	return runs
}

// useNoGcloud leaves PATH holding no gcloud for the rest of t, and clears the variables useGcloud
// clears.
func useNoGcloud(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "")
}

// commands is every command line the stub ran, in order.
func (g gcloudStub) commands(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "commands"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// lent is each account the stub was asked for an access token of, in order: the last word of
// each `auth print-access-token` command line that is not a flag.
func (g gcloudStub) lent(t *testing.T) []string {
	t.Helper()
	var accounts []string
	for _, command := range g.commands(t) {
		words := strings.Fields(command)
		if len(words) <= 2 || words[0] != "auth" || words[1] != "print-access-token" {
			continue
		}
		words = slices.DeleteFunc(words, func(word string) bool { return strings.HasPrefix(word, "--") })
		accounts = append(accounts, words[len(words)-1])
	}
	return accounts
}

// refuseValueReads makes reading a secret's value fail t for the rest of it (failingStdin): no
// value may be asked for before the sign-in is settled.
func refuseValueReads(t *testing.T) {
	t.Helper()
	restore := secretStdin
	secretStdin = failingStdin{t}
	t.Cleanup(func() { secretStdin = restore })
}

func alwaysServed(name string) Reread { return Reread{Name: name, Served: true} }

func TestGCPSecretCreateShowSetRetagDeleteRestoreLifecycle(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")
	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	// Each line names the store it wrote to, so an answer of this CLI's is never read as
	// another secrets CLI's (source.go).
	if !strings.Contains(stdout, "created SCRATCH_TEST in GCP Secret Manager project ") || !strings.Contains(stdout, "(owner=ada@example.com, tier=agent)") {
		t.Fatalf("create stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner: ada@example.com") || !strings.Contains(stdout, "tier: agent") {
		t.Fatalf("show stdout: %q", stdout)
	}
	if strings.Contains(stdout, "shh") {
		t.Fatalf("show printed the value: %q", stdout)
	}

	useStdin(t, "shh2")
	_, stderr, code = runSecret("set", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("set: exit %d, stderr %q", code, stderr)
	}

	stdout, stderr, code = runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "retagged SCRATCH_TEST in GCP Secret Manager project ") || !strings.Contains(stdout, "(owner=ada@example.com, tier=human)") {
		t.Fatalf("retag stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("delete", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("delete: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deleted SCRATCH_TEST in GCP Secret Manager project ") {
		t.Fatalf("delete stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show after delete: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deleted:") {
		t.Fatalf("show after delete did not print deleted: %q", stdout)
	}
	if strings.Contains(stdout, "earliest_purge") {
		t.Fatalf("GCP show printed an AWS-only earliest_purge line: %q", stdout)
	}

	_, stderr, code = runSecret("restore", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("restore: exit %d, stderr %q", code, stderr)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show after restore: exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "deleted:") {
		t.Fatalf("show after restore still shows deleted: %q", stdout)
	}
}

// ownerOf is the owner annotation of the secret f holds under slug, "" when it holds none.
func (f *fakeSecretManager) ownerOf(slug string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.secrets[testGCPPrefix+slug]
	if !ok {
		return ""
	}
	return entry.secret.GetAnnotations()[policy.TagOwner]
}

// requireNothingWritten fails t unless Secret Manager holds no secret and the broker was asked
// for no reread: what a refused write leaves.
func requireNothingWritten(t *testing.T, fake *fakeSecretManager, broker *secretBroker) {
	t.Helper()
	if n := fake.written(); n != 0 {
		t.Fatalf("Secret Manager holds %d secrets after a refused write", n)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 0 {
		t.Fatalf("rereads = %v after a refused write, want none", rereads)
	}
}

// requireRunUnder fails t unless every Secret Manager call so far, and at least one, carried the
// token the stub gcloud lends account, and gcloud lent tokens for account alone.
func requireRunUnder(t *testing.T, fake *fakeSecretManager, gcloud gcloudStub, account string) {
	t.Helper()
	if got, calls := fake.calledUnder(); got != "Bearer "+stubTokenPrefix+account || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only %s's gcloud token", got, calls, account)
	}
	lent := gcloud.lent(t)
	if len(lent) == 0 || slices.ContainsFunc(lent, func(a string) bool { return a != account }) {
		t.Fatalf("gcloud lent tokens for %v, want %s's alone", lent, account)
	}
}

// holdScratchTest puts SCRATCH_TEST in f, owned by ada@example.com at tier agent, with one
// enabled version, as an earlier write left it, and answers its resource name.
func holdScratchTest(f *fakeSecretManager) string {
	id := testGCPPrefix + "scratch-test"
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[id] = &fakeSecret{
		secret:      &secretmanagerpb.Secret{Name: id, Annotations: map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}, Etag: "1"},
		versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
		etagCounter: 1,
	}
	return id
}

// TestGCPSecretWriteUnderAMachineIdentityRunsUnderIt: every write runs under a machine's own
// credentials file whatever identity it is - a service account key or a workload identity
// federation credential (a dev box's own) - and never under the gcloud login gcloud also holds:
// create, set, retag, delete and restore each write, every Secret Manager call carrying the
// credentials' own token, with gcloud never run. The metadata server's identity comes after the
// active gcloud account in R1's order, so where gcloud holds a login the same writes run under
// that login. Where the store's IAM refuses the identity (PermissionDenied), the refusal (exit 1)
// names its source and principal, and nothing changes.
func TestGCPSecretWriteUnderAMachineIdentityRunsUnderIt(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	for _, kind := range machineADCKinds {
		t.Run(cmp.Or(kind, "metadata server"), func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			gcloud := useGcloud(t, "ada@example.com")
			useGCPIdentity(t, kind, machineEmail)
			bearer := "Bearer fake"
			identity := "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: " + machineEmail
			if kind == "" {
				bearer = "Bearer " + stubTokenPrefix + "ada@example.com"
				identity = "the active gcloud account (gcloud configuration default): ada@example.com"
			}
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			for _, step := range []struct {
				stdin string
				args  []string
			}{
				{"first", []string{"create", "SCRATCH_TEST", "--owner", "ada@example.com", "--tier", "agent"}},
				{"second", []string{"set", "SCRATCH_TEST"}},
				{"", []string{"retag", "SCRATCH_TEST", "--tier", "human"}},
				{"", []string{"delete", "SCRATCH_TEST"}},
				{"", []string{"restore", "SCRATCH_TEST"}},
			} {
				useStdin(t, step.stdin)
				if _, stderr, code := runSecret(step.args...); code != 0 {
					t.Fatalf("%v: exit %d, stderr %q; want 0", step.args, code, stderr)
				}
			}
			id := testGCPPrefix + "scratch-test"
			fake.mu.Lock()
			annotations, expire, versions := maps.Clone(fake.secrets[id].secret.GetAnnotations()), fake.secrets[id].secret.GetExpireTime(), len(fake.secrets[id].versions)
			fake.mu.Unlock()
			if annotations[policy.TagTier] != policy.TierHuman || expire != nil || versions != 2 {
				t.Fatalf("after the writes: annotations %v, expire_time %v, %d versions; want tier human, none, 2", annotations, expire, versions)
			}
			if got, calls := fake.calledUnder(); got != bearer || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only %s", got, calls, bearer)
			}
			if commands := gcloud.commands(t); kind != "" && len(commands) != 0 {
				t.Fatalf("gcloud ran %q, want nothing", commands)
			}
			if rereads, _ := broker.recorded(); len(rereads) != 5 {
				t.Fatalf("rereads = %v, want one after each of the five writes", rereads)
			}

			fake.mu.Lock()
			fake.refuse = bearer
			fake.mu.Unlock()
			_, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "agent")
			if code != 1 || !strings.Contains(stderr, "PermissionDenied") || !strings.Contains(stderr, "the store refused "+identity) {
				t.Fatalf("a refused retag: exit %d, stderr %q; want 1 naming the store's refusal of %s", code, stderr, identity)
			}
			fake.mu.Lock()
			tier := fake.secrets[id].secret.GetAnnotations()[policy.TagTier]
			fake.mu.Unlock()
			if tier != policy.TierHuman {
				t.Fatalf("tier = %q after a refused retag, want human", tier)
			}
		})
	}
}

// TestGCPSecretWritesUnderALentTokenWithoutAnEmailScope: an access token lent in
// CLOUDSDK_AUTH_ACCESS_TOKEN and minted for the cloud-platform scope alone, whose account Google's
// userinfo endpoint refuses to name, writes an existing secret all the same: set, retag, and
// delete then restore each write, every Secret Manager call carrying the lent token, and the
// broker is asked to reread each.
func TestGCPSecretWritesUnderALentTokenWithoutAnEmailScope(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useCloudPlatformToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useCloudPlatformToken, gcpUserinfo set at secret_gcp_test.go:569).
	for _, row := range []struct {
		name  string
		stdin string
		forms [][]string
		wrote func(entry *fakeSecret) bool
	}{
		{"set", "second", [][]string{{"set", "SCRATCH_TEST"}}, func(e *fakeSecret) bool { return len(e.versions) == 2 }},
		{"retag", "", [][]string{{"retag", "SCRATCH_TEST", "--tier", "human"}}, func(e *fakeSecret) bool {
			return e.secret.GetAnnotations()[policy.TagTier] == policy.TierHuman
		}},
		{"delete and restore", "", [][]string{{"delete", "SCRATCH_TEST"}, {"restore", "SCRATCH_TEST"}}, func(e *fakeSecret) bool {
			return e.secret.GetExpireTime() == nil && e.etagCounter == 3
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useCloudPlatformToken(t)
			id := holdScratchTest(fake)
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, row.stdin)
			for _, args := range row.forms {
				if _, stderr, code := runSecret(args...); code != 0 {
					t.Fatalf("%v: exit %d, stderr %q; want 0", args, code, stderr)
				}
			}
			fake.mu.Lock()
			wrote := row.wrote(fake.secrets[id])
			fake.mu.Unlock()
			if !wrote {
				t.Fatalf("%s left SCRATCH_TEST as it was", row.name)
			}
			if got, calls := fake.calledUnder(); got != "Bearer "+cloudPlatformToken || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the lent token", got, calls)
			}
			if rereads, _ := broker.recorded(); len(rereads) != len(row.forms) {
				t.Fatalf("rereads = %v, want one after each write", rereads)
			}
		})
	}
}

// TestGCPSecretOwnerMeUnderALentTokenWithoutAnEmailScopeIsRefused: --owner me is the email of the
// person the sign-in is, which Google's userinfo endpoint does not answer for a token without an
// email scope, so create and retag refuse it (exit 2), naming the lent token and --owner <email>,
// before the value is read and with nothing written.
func TestGCPSecretOwnerMeUnderALentTokenWithoutAnEmailScopeIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useCloudPlatformToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.refuseValueReads, secretStdin set at secret_gcp_test.go:751).
	for _, args := range [][]string{{"create", "NEW_TEST", "--owner", "me", "--tier", "agent"}, {"retag", "SCRATCH_TEST", "--owner", "me"}} {
		t.Run(args[0], func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useCloudPlatformToken(t)
			holdScratchTest(fake)
			broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
			refuseValueReads(t)

			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "--owner <email>") || !strings.Contains(stderr, "CLOUDSDK_AUTH_ACCESS_TOKEN") {
				t.Fatalf("exit %d, stderr %q; want %d naming the lent token and --owner <email>", code, stderr, exitUsageError)
			}
			if n := fake.written(); n != 1 {
				t.Fatalf("Secret Manager holds %d secrets after a refused write, want SCRATCH_TEST alone", n)
			}
			if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
				t.Fatalf("SCRATCH_TEST's owner = %q after a refused retag", owner)
			}
			if rereads, _ := broker.recorded(); len(rereads) != 0 {
				t.Fatalf("rereads = %v, want none", rereads)
			}
		})
	}
}

// TestGCPSecretOwnerNamedByEmail: --owner names the owning person by email outright, lowercased,
// on create and on retag, so a sign-in whose person Google cannot name - here a lent token without
// an email scope - creates a secret for its person and gives one to another, every call under the
// lent token.
func TestGCPSecretOwnerNamedByEmail(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useCloudPlatformToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useCloudPlatformToken, gcpUserinfo set at secret_gcp_test.go:569).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useCloudPlatformToken(t)
	broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "Someone@Example.com", "--tier", "human")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	if owner := fake.ownerOf("scratch-test"); owner != "someone@example.com" || !strings.Contains(stdout, "owner=someone@example.com") {
		t.Fatalf("owner annotation %q, stdout %q; want someone@example.com", owner, stdout)
	}
	if _, stderr, code := runSecret("retag", "SCRATCH_TEST", "--owner", "grace@example.com"); code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if owner := fake.ownerOf("scratch-test"); owner != "grace@example.com" {
		t.Fatalf("owner annotation %q after the retag, want grace@example.com", owner)
	}
	if got, calls := fake.calledUnder(); got != "Bearer "+cloudPlatformToken || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the lent token", got, calls)
	}
	if rereads, _ := broker.recorded(); !slices.Equal(rereads, []string{"SCRATCH_TEST", "SCRATCH_TEST"}) {
		t.Fatalf("rereads = %v, want one after each write", rereads)
	}
}

// TestGCPSecretALentTokenOutranksEveryOtherCredential: CLOUDSDK_AUTH_ACCESS_TOKEN is the caller's
// own choice, so every form signs in with it, a read and a write alike, beside any other
// credential - Application Default Credentials GOOGLE_APPLICATION_CREDENTIALS names, a person's
// own Application Default Credentials, or, with none, a gcloud login - with gcloud never run.
func TestGCPSecretALentTokenOutranksEveryOtherCredential(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	for _, row := range []struct {
		name string
		use  func(t *testing.T)
	}{
		{"over credentials GOOGLE_APPLICATION_CREDENTIALS names", func(t *testing.T) {
			useGCPIdentity(t, "service_account", machineEmail)
		}},
		{"over a person's own Application Default Credentials", func(t *testing.T) { useGCPIdentity(t, "authorized_user", "grace@example.com") }},
		{"over a gcloud login", useNoADC},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			gcloud := useGcloud(t, "grace@example.com")
			row.use(t)
			useLentGCPAccessToken(t, "lent-ada-token", "ada@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "shh")

			if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
				t.Fatalf("create: exit %d, stderr %q", code, stderr)
			}
			if _, stderr, code := runSecret("list"); code != 0 {
				t.Fatalf("list: exit %d, stderr %q", code, stderr)
			}
			if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
				t.Fatalf("owner annotation %q, want the lent token's ada@example.com", owner)
			}
			if got, calls := fake.calledUnder(); got != "Bearer lent-ada-token" || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the lent token", got, calls)
			}
			if commands := gcloud.commands(t); len(commands) != 0 {
				t.Fatalf("gcloud ran %q, want nothing", commands)
			}
		})
	}
}

// TestGCPSecretGcloudLoginIsTheOneGcloudUses: with no Application Default Credentials, every form
// runs under the login gcloud itself would use, whatever kind of account it is - a service
// account's, a workload or workforce identity federation login's, or a person's: the one
// CLOUDSDK_CORE_ACCOUNT names, else the active one. Where CLOUDSDK_CORE_ACCOUNT names a login
// gcloud does not hold, a form is refused (exit 1), naming that account and each login gcloud
// holds, with no token lent and no Secret Manager call - never run as another login, though gcloud
// holds only one.
func TestGCPSecretGcloudLoginIsTheOneGcloudUses(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1137); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	several := []string{machineEmail, "ada@example.com", "grace@example.com"}
	for _, row := range []struct {
		name, named, want string
		accounts          []string
	}{
		{"a service account's login alone", "", machineEmail, []string{machineEmail}},
		{"a workload identity federation login alone", "", workloadPrincipal, []string{workloadPrincipal}},
		{"a workforce identity federation login alone", "", workforcePrincipal, []string{workforcePrincipal}},
		{"the active one of several", "", machineEmail, several},
		{"the one CLOUDSDK_CORE_ACCOUNT names", "grace@example.com", "grace@example.com", several},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useNoADC(t)
			gcloud := useGcloud(t, row.accounts...)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", row.named)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "shh")

			if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "human"); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			requireRunUnder(t, fake, gcloud, row.want)
		})
	}
	for _, accounts := range [][]string{{machineEmail}, several} {
		t.Run(fmt.Sprintf("a named login gcloud does not hold, beside %d", len(accounts)), func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useNoADC(t)
			gcloud := useGcloud(t, accounts...)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "linus@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

			_, stderr, code := runSecret("list")
			if code != 1 || !strings.Contains(stderr, "CLOUDSDK_CORE_ACCOUNT names an account gcloud holds no login for (not printed;") || strings.Contains(stderr, "linus@example.com") {
				t.Fatalf("exit %d, stderr %q; want 1 naming CLOUDSDK_CORE_ACCOUNT, never the account it names", code, stderr)
			}
			for _, account := range accounts {
				if !strings.Contains(stderr, account) {
					t.Fatalf("stderr %q does not name the login %s gcloud holds", stderr, account)
				}
			}
			if _, calls := fake.calledUnder(); calls != 0 {
				t.Fatalf("Secret Manager got %d calls, want none", calls)
			}
			if lent := gcloud.lent(t); len(lent) != 0 {
				t.Fatalf("gcloud lent tokens for %v, want none", lent)
			}
		})
	}
}

// TestGCPSecretGcloudRunsWithItsOwnSettingsAndNoLogs: gcloud runs with the caller's own gcloud
// settings - here impersonation, which the token it lends then acts as, and which the "using"
// line names - and always with file logging, HTTP logging and prompts off, though this command's
// environment turns file logging and HTTP logging on and their token redaction off; its token
// command prints the token in the private format, which bypasses gcloud's log file, and clears
// none of gcloud's own auth properties.
func TestGCPSecretGcloudRunsWithItsOwnSettingsAndNoLogs(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1188); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	t.Run("the caller's settings, and no logs", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useNoADC(t)
		gcloud := useGcloud(t, "ada@example.com")
		t.Setenv("CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT", "robot@example.iam.gserviceaccount.com")
		t.Setenv("CLOUDSDK_CORE_DISABLE_FILE_LOGGING", "false")
		t.Setenv("CLOUDSDK_CORE_LOG_HTTP", "true")
		t.Setenv("CLOUDSDK_CORE_LOG_HTTP_REDACT_TOKEN", "false")
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent")
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stderr, "agent-secrets secret create: using the active gcloud account (gcloud configuration default): ada@example.com impersonating robot@example.iam.gserviceaccount.com\n") {
			t.Fatalf("stderr %q does not name the login and the account it impersonates", stderr)
		}
		requireGcloudRuns(t, gcloud, map[string]string{
			"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT": "robot@example.iam.gserviceaccount.com",
			"CLOUDSDK_CORE_DISABLE_FILE_LOGGING":        "1",
			"CLOUDSDK_CORE_DISABLE_PROMPTS":             "1",
			"CLOUDSDK_CORE_LOG_HTTP":                    "false",
		}, "")
		var token []string
		for _, command := range gcloud.commands(t) {
			if words := strings.Fields(command); len(words) > 1 && words[1] == "print-access-token" {
				token = words
			}
		}
		if !slices.Contains(token, "--format=value[private](token)") {
			t.Errorf("gcloud's token command %q lacks --format=value[private](token)", token)
		}
		for _, word := range token {
			for _, cleared := range []string{"--impersonate-service-account=", "--access-token-file=", "--credential-file-override="} {
				if strings.HasPrefix(word, cleared) {
					t.Errorf("gcloud's token command %q carries %s, which overrides gcloud's own setting", token, word)
				}
			}
		}
		if len(token) == 0 || token[len(token)-1] != "ada@example.com" {
			t.Errorf("gcloud's token command %q does not end with the account", token)
		}
	})
}

// requireGcloudRuns fails t unless gcloud ran, every run's environment held each variable of want
// at its value ("unset" for one it must not hold), and, where flag is not empty, every command
// line carried flag.
func requireGcloudRuns(t *testing.T, gcloud gcloudStub, want map[string]string, flag string) {
	t.Helper()
	runs, commands := gcloud.environments(t), gcloud.commands(t)
	if len(runs) == 0 {
		t.Fatal("gcloud never ran")
	}
	for i, run := range runs {
		for name, value := range want {
			if run[name] != value {
				t.Errorf("gcloud run %d (%s): %s=%q, want %q", i, commands[i], name, run[name], value)
			}
		}
		if flag != "" && !slices.Contains(strings.Fields(commands[i]), flag) {
			t.Errorf("gcloud run %d (%s) lacks %s", i, commands[i], flag)
		}
	}
}

// TestGCPSecretPipeCommandKeepsTheGcloudAccountChoice: the command a write prints for piping a
// value in, after refusing a value of more than one line at the prompt, starts with the
// CLOUDSDK_CORE_ACCOUNT that chose the gcloud login, shell-quoted, so running it picks the same
// login among several; with a single login it chose none, and the command carries none.
func TestGCPSecretPipeCommandKeepsTheGcloudAccountChoice(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1275); it signals the test process or changes its signal handling (var main.readHidden, main.readHiddenAtTerminal, (*promptTerminal).raise, a signal to this process (unix.Kill) at secret_prompt_unix.go:520); it sets a package variable the package's other tests read (readHidden set at secret_gcp_test.go:1278).
	for _, row := range []struct {
		name     string
		accounts []string
		named    string
		want     string
	}{
		{"several logins, one named", []string{machineEmail, "ada@example.com", "grace@example.com"}, "grace@example.com",
			"CLOUDSDK_CORE_ACCOUNT=grace@example.com agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
		{"a named login that needs quoting", []string{"ada@example.com", "grace+ops@example.com"}, "grace+ops@example.com",
			"CLOUDSDK_CORE_ACCOUNT='grace+ops@example.com' agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
		{"one login, none named", []string{"ada@example.com"}, "",
			": agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useNoADC(t)
			useGcloud(t, row.accounts...)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", row.named)
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useTerminal(t)
			readHidden = func(int, time.Time, func(), func(error)) ([]byte, error) { return nil, errMoreThanOneLine }

			_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
			if code != exitUsageError || !strings.Contains(stderr, row.want) {
				t.Fatalf("exit %d, stderr %q; want %d naming %q", code, stderr, exitUsageError, row.want)
			}
			requireNothingLeft(t, fake, broker, "SCRATCH_TEST")
		})
	}
}

// TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline: an expired gcloud token is
// refreshed through gcloudToken.Token, which runs `gcloud auth print-access-token`. A gcloud that
// never answers - here a script whose child holds its output open, as the real gcloud's Python does
// under its shell wrapper - ends at the deadline on each store call (storeCallDeadline), so a stalled
// refresh cannot outlast the store call it serves; the error names the account and the deadline.
// The refresh is timed against that deadline, less the time this process was held up
// (endsByItsDeadline).
func TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline(t *testing.T) {
	// Not parallel: it signals the test process or changes its signal handling ((gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStoreCallDeadline, storeCallDeadline set at secret_create_test.go:36); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.endsByItsDeadline, main.requireOnTime, onTimeWithin at waits_test.go:239).
	useStoreCallDeadline(t, 300*time.Millisecond)
	dir := t.TempDir()
	gcloud := filepath.Join(dir, "gcloud")
	if err := os.WriteFile(gcloud, []byte("#!/bin/sh\nsleep 3600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	expired := &oauth2.Token{AccessToken: "old", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)}
	source := oauth2.ReuseTokenSource(expired, gcloudToken{ctx: context.Background(), gcloud: gcloudCommand{path: gcloud}, account: "ada@example.com"})
	done := make(chan error, 1)
	meter := startStallMeter(t)
	start := time.Now()
	go func() {
		_, err := source.Token()
		done <- err
	}()
	err := endsByItsDeadline(t, meter, "the refresh of an expired token whose gcloud never answers", start, 300*time.Millisecond, done)
	if err == nil || !strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "did not answer within 300ms") {
		t.Fatalf("Token() = %v; want a failure naming the account and the 300ms deadline", err)
	}
}

// useGcloudThatAnswers puts a stub gcloud alone on PATH for the rest of t that holds one login,
// account, answers `auth print-access-token` with stubTokenPrefix + account, and answers
// `auth list` (account, active) and `config list` (no auth property) as authList and configList
// say: "" answers, "fail" fails as gcloud does, naming the failure on its standard error, and
// "hang" answers nothing for 30 s. It first clears every other source, as useNoADC does.
func useGcloudThatAnswers(t *testing.T, account, authList, configList string) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	useNoADC(t)
	behave := func(command, answer, mode string) string {
		switch mode {
		case "fail":
			return `echo 'ERROR: (gcloud.` + command + `) the test fails it' >&2; exit 1`
		case "hang":
			return "exec " + shellWord(sleep) + " 30"
		}
		return "printf '%s\\n' " + shellWord(answer)
	}
	script := strings.Join([]string{
		"#!/bin/sh",
		`case "$1 $2" in`,
		`"auth list") ` + behave("auth.list", `[{"account":"`+account+`","status":"ACTIVE"}]`, authList) + ` ;;`,
		`"config list") ` + behave("config.list", `{"auth":{}}`, configList) + ` ;;`,
		`"auth print-access-token") printf '%s\n' ` + shellWord(stubTokenPrefix+account) + ` ;;`,
		`*) printf 'stub gcloud: unexpected command: %s\n' "$*" >&2; exit 2 ;;`,
		"esac",
		"",
	}, "\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
}

// TestGCPSecretGcloudSettingsReadIsBestEffort: the gcloud source reads gcloud's auth settings only
// to name whom its token acts as (R2), so a `gcloud config list` that fails, or that does not
// answer within gcloudSettingsDeadline, never stops the command: the form runs under the login's
// token, and the using line names the login with the principal unknown. The form is timed against
// gcloudSettingsDeadline where the read hangs, and against no deadline where it fails, less the
// time this process was held up (requireOnTime).
func TestGCPSecretGcloudSettingsReadIsBestEffort(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGcloudThatAnswers, main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGcloudThatAnswers, main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.requireOnTime, onTimeWithin at waits_test.go:239).
	for _, mode := range []string{"fail", "hang"} {
		t.Run(mode, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGcloudThatAnswers(t, "ada@example.com", "", mode)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			waits := time.Duration(0)
			if mode == "hang" {
				waits = gcloudSettingsDeadline
			}
			meter := startStallMeter(t)
			started := time.Now()
			_, stderr, code := runSecret("list")
			took := requireOnTime(t, meter, "the form whose gcloud settings read "+mode+"s", started, waits)
			if code != 0 {
				t.Fatalf("exit %d after %s, stderr %q; want 0", code, took, stderr)
			}
			if want := "agent-secrets secret list: using the active gcloud account (gcloud configuration default): ada@example.com (principal unknown: gcloud's auth settings could not be read)\n"; !strings.Contains(stderr, want) {
				t.Fatalf("stderr %q does not say %q", stderr, want)
			}
			if got, calls := fake.calledUnder(); got != "Bearer "+stubTokenPrefix+"ada@example.com" || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only ada@example.com's gcloud token", got, calls)
			}
		})
	}
}

// TestGCPSecretGcloudLoginListThatNeverAnswersEndsAtTheStoreDeadline: `gcloud auth list`, which
// decides whether the gcloud source is present, runs under storeCallDeadline as the token's
// refresh does, so a gcloud that never answers stops the command at the deadline, naming it,
// before any Secret Manager call, timed against that deadline (requireOnTime).
func TestGCPSecretGcloudLoginListThatNeverAnswersEndsAtTheStoreDeadline(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGcloudThatAnswers, main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStoreCallDeadline, storeCallDeadline set at secret_create_test.go:36); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.requireOnTime, onTimeWithin at waits_test.go:239).
	useStoreCallDeadline(t, 300*time.Millisecond)
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGcloudThatAnswers(t, "ada@example.com", "hang", "")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	meter := startStallMeter(t)
	started := time.Now()
	_, stderr, code := runSecret("list")
	took := requireOnTime(t, meter, "the form whose `gcloud auth list` never answers", started, 300*time.Millisecond)
	if code != 1 || !strings.Contains(stderr, "`gcloud auth list` did not answer within 300ms") {
		t.Fatalf("exit %d after %s, stderr %q; want 1 naming the 300ms deadline", code, took, stderr)
	}
	if _, calls := fake.calledUnder(); calls != 0 {
		t.Fatalf("Secret Manager saw %d calls, want none", calls)
	}
}

// TestGCPSecretWithNoApplicationDefaultCredentialsUsesTheGcloudLogin: where there are no
// Application Default Credentials and GOOGLE_APPLICATION_CREDENTIALS is unset, every form, a read
// included, runs under the gcloud login.
func TestGCPSecretWithNoApplicationDefaultCredentialsUsesTheGcloudLogin(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	gcloud := useGcloud(t, "ada@example.com", machineEmail)
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	held, err := fake.CreateSecret(context.Background(), &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/" + testGCPProject, SecretId: "agent-secrets-demo-deploy-token",
		Secret: &secretmanagerpb.Secret{Annotations: map[string]string{policy.TagOwner: "grace@example.com", policy.TagTier: "agent"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: held.GetName()}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "DEMO_DEPLOY_TOKEN") {
		t.Fatalf("list stdout: %q", stdout)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")

	useStdin(t, "shh")
	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")
}

// TestGCPSecretWithNoCredentialAtAllIsRefused: with no CLOUDSDK_AUTH_ACCESS_TOKEN, no Application
// Default Credentials, and no gcloud login either, a read is refused (exit 1), naming the sources
// it tried, before any Secret Manager call.
func TestGCPSecretWithNoCredentialAtAllIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	for _, row := range []struct {
		name   string
		gcloud func(t *testing.T)
	}{
		{"gcloud holds no login", func(t *testing.T) { useGcloud(t) }},
		{"no gcloud on PATH", useNoGcloud},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useNoADC(t)
			row.gcloud(t)
			startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

			_, stderr, code := runSecret("list")
			if code != 1 || !strings.Contains(stderr, "CLOUDSDK_AUTH_ACCESS_TOKEN") || !strings.Contains(stderr, "gcloud") {
				t.Fatalf("exit %d, stderr %q; want 1 naming each source it tried", code, stderr)
			}
			if _, calls := fake.calledUnder(); calls != 0 {
				t.Fatalf("Secret Manager got %d calls, want none", calls)
			}
		})
	}
}

// TestGCPSecretCreateWithLentPersonsAccessToken: without ADC or a gcloud login, a person's lent
// token whose account Google's userinfo endpoint names creates the secret and supplies its
// --owner me email.
func TestGCPSecretCreateWithLentPersonsAccessToken(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useLentGCPAccessToken, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useLentGCPAccessToken, gcpUserinfo set at secret_gcp_test.go:551).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	useNoGcloud(t)
	useLentGCPAccessToken(t, "lent-ada-token", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	if got, calls := fake.calledUnder(); got != "Bearer lent-ada-token" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want the lent token", got, calls)
	}
}

// TestGCPSecretOwnerMeUnderAServiceAccountIsRefused: --owner me under a sign-in Google's userinfo
// endpoint names as a service account - here a lent token beside a service account's Application
// Default Credentials, the lent token being the one userinfo is asked about - is refused (exit 2),
// naming the account and --owner <email>, before the value is read and with nothing written: a
// service account names no person.
func TestGCPSecretOwnerMeUnderAServiceAccountIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1521); it signals the test process or changes its signal handling ((gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (gcpUserinfo set at secret_gcp_test.go:1524).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "service_account", machineEmail)
	useNoGcloud(t)
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-service-token")
	var inspected string
	restoreUserinfo := gcpUserinfo
	gcpUserinfo = func(_ context.Context, creds *google.Credentials) (string, error) {
		token, err := creds.TokenSource.Token()
		if err != nil {
			return "", err
		}
		inspected = token.AccessToken
		return machineEmail, nil
	}
	t.Cleanup(func() { gcpUserinfo = restoreUserinfo })
	broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	refuseValueReads(t)

	_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != exitUsageError || !strings.Contains(stderr, "--owner <email>") || !strings.Contains(stderr, machineEmail) {
		t.Fatalf("exit %d, stderr %q; want %d naming the service account and --owner <email>", code, stderr, exitUsageError)
	}
	if inspected != "lent-service-token" {
		t.Fatalf("userinfo inspected token %q, want the lent token", inspected)
	}
	requireNothingWritten(t, fake, broker)
}

// TestGCPSecretLentTokenWithNoUserinfoEmailIsRefused: no empty userinfo email can become an
// owner: --owner me is refused (exit 2), naming --owner <email>, before a value is read or
// anything is written.
func TestGCPSecretLentTokenWithNoUserinfoEmailIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1555); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.refuseValueReads, secretStdin set at secret_gcp_test.go:751).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	useNoGcloud(t)
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-unknown-token")
	broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	refuseValueReads(t)

	_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != exitUsageError || !strings.Contains(stderr, "names no email") || !strings.Contains(stderr, "--owner <email>") {
		t.Fatalf("exit %d, stderr %q; want the empty-userinfo refusal naming --owner <email>", code, stderr)
	}
	requireNothingWritten(t, fake, broker)
}

// TestGCPSecretUnderGoogleApplicationCredentialsNeverAsksGcloud: GOOGLE_APPLICATION_CREDENTIALS
// names the credentials every form uses, so the gcloud login never stands in for them, though
// gcloud holds a person's login: a file there that cannot be loaded is refused before any Secret
// Manager call, and a write runs under a machine's credentials named there, with gcloud never run.
func TestGCPSecretUnderGoogleApplicationCredentialsNeverAsksGcloud(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1577); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	t.Run("a file that cannot be loaded", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useNoADC(t)
		gcloud := useGcloud(t, "ada@example.com")
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "absent.json"))
		startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

		_, stderr, code := runSecret("list")
		if code != 1 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if _, calls := fake.calledUnder(); calls != 0 {
			t.Fatalf("Secret Manager got %d calls, want none", calls)
		}
		if commands := gcloud.commands(t); len(commands) != 0 {
			t.Fatalf("gcloud ran %q, want nothing", commands)
		}
	})
	t.Run("a machine's credentials, for a write", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		gcloud := useGcloud(t, "ada@example.com")
		useGCPIdentity(t, "service_account", machineEmail)
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "human"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if got, calls := fake.calledUnder(); got != "Bearer fake" || calls == 0 {
			t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the named credentials", got, calls)
		}
		if commands := gcloud.commands(t); len(commands) != 0 {
			t.Fatalf("gcloud ran %q, want nothing", commands)
		}
	})
}

// TestGCPSecretWriteUnderAPersonsADCNeverAsksGcloud: a person's own Application Default
// Credentials (a laptop's) write as they always have, every call under them, with gcloud never
// run, though it holds another person's login.
func TestGCPSecretWriteUnderAPersonsADCNeverAsksGcloud(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	gcloud := useGcloud(t, "grace@example.com")
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if owner := fake.ownerOf("scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	if got, calls := fake.calledUnder(); got != "Bearer fake" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the person's own ADC", got, calls)
	}
	if commands := gcloud.commands(t); len(commands) != 0 {
		t.Fatalf("gcloud ran %q, want nothing", commands)
	}
}

// TestGCPSecretListSucceedsWithoutUserinfoScope: a read makes no userinfo call, so it succeeds
// even under a token that lacks the scope that call needs. On a machine whose gcloud holds a login
// beside the machine's own identity, it runs under that login, the active gcloud account, which
// R1 puts before the metadata server.
func TestGCPSecretListSucceedsWithoutUserinfoScope(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (gcpUserinfo set at secret_gcp_test.go:1648).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	gcloud := useGcloud(t, "ada@example.com")
	useGCPIdentity(t, "", machineEmail)
	var userinfoCalls int
	gcpUserinfo = func(context.Context, *google.Credentials) (string, error) {
		userinfoCalls++
		return "", errors.New("insufficient authentication scopes")
	}
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")
	if userinfoCalls != 0 {
		t.Fatalf("a read made %d userinfo calls, want none", userinfoCalls)
	}
}

// TestGCPSecretRetagKeepsUnrelatedAnnotations: retag changes only owner and tier, never an
// annotation it does not know about (an UpdateSecret whose mask names "annotations" replaces the
// whole map unless retag reads and merges the current one first).
func TestGCPSecretRetagKeepsUnrelatedAnnotations(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")
	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}

	id := testGCPPrefix + "scratch-test"
	fake.mu.Lock()
	fake.secrets[id].secret.Annotations["unrelated-note"] = "do-not-delete"
	fake.mu.Unlock()

	if _, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human"); code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}

	fake.mu.Lock()
	got := fake.secrets[id].secret.Annotations["unrelated-note"]
	fake.mu.Unlock()
	if got != "do-not-delete" {
		t.Fatalf("retag dropped an unrelated annotation: got %q", got)
	}
}

// TestGCPSecretOwnerMeIsTheSignInEmailLowercased: --owner me is the userinfo email, lowercased.
func TestGCPSecretOwnerMeIsTheSignInEmailLowercased(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "Ada@Example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner=ada@example.com") {
		t.Fatalf("stdout: %q", stdout)
	}
}

// TestGCPSecretRetagOnlyNamedFieldReplacesHeldOne: retag with only --tier keeps the held owner,
// sending both as one UpdateSecret call (the annotations map is written whole).
func TestGCPSecretRetagOnlyNamedFieldReplacesHeldOne(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner=shared, tier=human") {
		t.Fatalf("stdout: %q", stdout)
	}
}

// TestGCPSecretRetagOfASharedSecretSaysAnAdministratorChangesIt: IAM (PermissionDenied) refuses a
// person's change to a shared secret's annotations; the CLI prints GCP's refusal and says whose
// change it is.
func TestGCPSecretRetagOfASharedSecretSaysAnAdministratorChangesIt(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	fake.mu.Lock()
	fake.refuseAnnotationUpdates = true
	fake.mu.Unlock()

	_, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "PermissionDenied") {
		t.Fatalf("stderr does not show GCP's refusal: %q", stderr)
	}
	if !strings.Contains(stderr, "a shared secret's owner and tier are an administrator's to change") {
		t.Fatalf("stderr does not say whose change it is: %q", stderr)
	}
}

// TestGCPSecretListShowsOwnerTierAndValue: list shows every secret under the prefix, its owner,
// tier and whether it has a value, never the value.
func TestGCPSecretListShowsOwnerTierAndValue(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "SCRATCH_TEST") || !strings.Contains(stdout, "ada@example.com") || !strings.Contains(stdout, "agent") || !strings.Contains(stdout, "yes") {
		t.Fatalf("list stdout: %q", stdout)
	}
	if strings.Contains(stdout, "shh") {
		t.Fatalf("list printed the value: %q", stdout)
	}
}

// TestGCPSecretReadsRefuseBadSettings: an invalid kms_key_name is refused before any GCP call.
// TestCheckSettingsKMSKeyName covers the other checkSettingsKMSKeyName refusals directly.
func TestGCPSecretReadsRefuseBadSettings(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	startFakeSecretManager(t, newFakeSecretManager())
	useGCPIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, Settings{
		Store: secrets.StoreGCP, SecretsPrefix: testGCPPrefix, GCPProject: testGCPProject,
		KMSKeyName: "not-a-key-name",
	}, alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "invalid KMS key name") {
		t.Fatalf("stderr: %q", stderr)
	}
}

func TestCheckSettingsKMSKeyName(t *testing.T) {
	t.Parallel()
	if err := checkSettingsKMSKeyName(gcpTestSettings()); err != nil {
		t.Fatalf("valid settings refused: %v", err)
	}
	bad := gcpTestSettings()
	bad.SecretsPrefix = "not-a-prefix"
	if err := checkSettingsKMSKeyName(bad); err == nil {
		t.Fatal("an invalid secrets_prefix was accepted")
	}
	mismatched := gcpTestSettings()
	mismatched.GCPProject = "999999999999"
	if err := checkSettingsKMSKeyName(mismatched); err == nil {
		t.Fatal("a gcp_project not matching secrets_prefix's project number was accepted")
	} else if !strings.Contains(err.Error(), "999999999999") || !strings.Contains(err.Error(), testGCPProject) {
		t.Fatalf("error does not name both projects: %v", err)
	}
	badKey := gcpTestSettings()
	badKey.KMSKeyName = "bad"
	if err := checkSettingsKMSKeyName(badKey); err == nil {
		t.Fatal("an invalid key name was accepted")
	}
}

// TestOpenSecretBackendRefusesUnknownStore: a store name neither backend names is refused before
// any cloud call.
func TestOpenSecretBackendRefusesUnknownStore(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293).
	startSecretBrokerWith(t, Settings{Store: "azure-key-vault"}, alwaysServed)
	_, stderr, code := runSecret("list")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, `unknown secret store "azure-key-vault"`) {
		t.Fatalf("stderr: %q", stderr)
	}
}

// TestGCPSecretChosenConfigurationNamesAGcloudLogin: CLOUDSDK_ACTIVE_CONFIG_NAME names a gcloud
// configuration, and a form runs under that configuration's login, though another is active.
func TestGCPSecretChosenConfigurationNamesAGcloudLogin(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1852); it sets a package variable the package's other tests read (main.startFakeSecretManager, main.useGCPDialTo, main.replaceGCPDial, gcpClientDial set at secret_gcp_test.go:381).
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useGcloudConfigurations(t, "box", orderConfigurations)
	t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "person")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	_, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	if got, calls := fake.calledUnder(); got != "Bearer gcloud-not-a-secret-for-grace@example.com" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the person configuration's grace@example.com", got, calls)
	}
}

// TestGCPSecretRefusalNamesTheSourceAndTheChoice: a store refusal names the source the
// credential came from and whom it acts as - or that the principal is unknown, with no userinfo
// call to learn it - and how to choose another, built from that source: a lent token outranks
// every other source, so only unsetting it chooses another; a credentials file outranks the gcloud
// variables, so they choose only once it is unset; under every later source the gcloud variables
// and GOOGLE_APPLICATION_CREDENTIALS choose, with the gcloud configurations here. Where
// CLOUDSDK_CORE_ACCOUNT is set, it outranks a configuration's account, so a configuration chooses
// only once it is unset, and the advice says so. A create that IAM lets make the secret but not
// add its value says the same.
func TestGCPSecretRefusalNamesTheSourceAndTheChoice(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1884); it sets a package variable the package's other tests read (gcpUserinfo set at secret_gcp_test.go:1924).
	const (
		later       = "choose another with a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME, an account in CLOUDSDK_CORE_ACCOUNT, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS (gcloud configurations here: box, person)"
		lent        = "choose another by unsetting CLOUDSDK_AUTH_ACCESS_TOKEN, which outranks every other source, or by lending another token in it"
		file        = "choose another by naming another credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting it, which outranks every gcloud source, to choose a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME or an account in CLOUDSDK_CORE_ACCOUNT (gcloud configurations here: box, person)"
		account     = "choose another with an account in CLOUDSDK_CORE_ACCOUNT, which outranks a configuration's account, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting CLOUDSDK_CORE_ACCOUNT to choose a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME (gcloud configurations here: box, person)"
		fileAccount = "choose another by naming another credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting it, which outranks every gcloud source, to choose an account in CLOUDSDK_CORE_ACCOUNT, which outranks a configuration's account, or, with CLOUDSDK_CORE_ACCOUNT unset too, a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME (gcloud configurations here: box, person)"
	)
	useCredentialsFile := func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credentials.json")
		writeServiceAccountFile(t, path, "gac@example.iam.gserviceaccount.com", startGoogleTokenServer(t))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	}
	for _, row := range []struct {
		name, refused, choice string
		use                   func(t *testing.T)
	}{
		{"metadata alone", "the metadata server: machine@example.iam.gserviceaccount.com", later, func(t *testing.T) { t.Setenv("PATH", t.TempDir()) }},
		{"active gcloud", "the active gcloud account (gcloud configuration box): box@example.iam.gserviceaccount.com", later, func(*testing.T) {}},
		{"chosen configuration", "the gcloud configuration person (CLOUDSDK_ACTIVE_CONFIG_NAME): grace@example.com", later, func(t *testing.T) { t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "person") }},
		{"chosen account, a configuration chosen too", "the gcloud account (CLOUDSDK_CORE_ACCOUNT): grace@example.com", account, func(t *testing.T) {
			t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "box")
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		}},
		{"lent token", "the access token in CLOUDSDK_AUTH_ACCESS_TOKEN: principal unknown", lent, func(t *testing.T) { t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-token") }},
		{"credentials file", "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: gac@example.iam.gserviceaccount.com", file, useCredentialsFile},
		{"credentials file, an account exported", "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: gac@example.iam.gserviceaccount.com", fileAccount, func(t *testing.T) {
			useCredentialsFile(t)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		}},
	} {
		for _, refusal := range []struct {
			name, says string
			setUp      func(fake *fakeSecretManager)
		}{
			{"the store refuses the create", "PermissionDenied", func(fake *fakeSecretManager) {
				fake.refuseMethods = map[string]codes.Code{"CreateSecret": codes.PermissionDenied}
			}},
			{"IAM grants the create and not the add", "may create " + testGCPPrefix + "new-test but not add its value", func(fake *fakeSecretManager) {
				fake.withheld = []string{addPermission}
			}},
		} {
			t.Run(row.name+", "+refusal.name, func(t *testing.T) {
				fake := newFakeSecretManager()
				refusal.setUp(fake)
				startFakeSecretManager(t, fake)
				startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
				useGcloudConfigurations(t, "box", orderConfigurations)
				row.use(t)
				restoreUserinfo := gcpUserinfo
				var userinfoCalls int
				gcpUserinfo = func(context.Context, *google.Credentials) (string, error) {
					userinfoCalls++
					return "", errors.New("the test answers no userinfo")
				}
				t.Cleanup(func() { gcpUserinfo = restoreUserinfo })
				startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
				useStdin(t, "")

				_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "shared", "--tier", "agent")
				if code != 1 || !strings.Contains(stderr, refusal.says) {
					t.Fatalf("exit %d, stderr %q; want 1 saying %q", code, stderr, refusal.says)
				}
				for _, said := range []string{row.refused, row.choice} {
					if !strings.Contains(stderr, said) {
						t.Fatalf("stderr %q does not say %q", stderr, said)
					}
				}
				for _, other := range []string{later, lent, file, account, fileAccount} {
					if other != row.choice && strings.Contains(stderr, other) {
						t.Fatalf("stderr %q gives advice for another source than %s: %q", stderr, row.name, other)
					}
				}
				if userinfoCalls != 0 {
					t.Fatalf("the refusal made %d userinfo calls, want none", userinfoCalls)
				}
			})
		}
	}
}

// TestGCPSecretRefusalNamesThePrincipalOfEachSource proves R2's per-source naming: a store refusal
// names the account the credential acts as for each source that can name one - the metadata server
// (its email), a service-account key file (its client_email), a gcloud login (its account); names
// "principal unknown" for an authorized_user file that holds no email; and, when the metadata
// server's email read does not answer, still signs in under its token and names the generic
// identity. No source makes a userinfo call to learn a principal, and every row reaches a store
// call, which is what signing in proves.
func TestGCPSecretRefusalNamesThePrincipalOfEachSource(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1973); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (gcpMetadataPrincipal set at secret_gcp_test.go:1977).
	const (
		dev   = "dev-box@acme.iam.gserviceaccount.com"
		robot = "robot@acme.iam.gserviceaccount.com"
		ada   = "ada@example.com"
	)
	for _, row := range []struct {
		name    string
		setUp   func(t *testing.T)
		refused string
	}{
		{"metadata names the account", func(t *testing.T) { useGCPIdentity(t, "", dev); t.Setenv("PATH", t.TempDir()) }, "the metadata server: " + dev},
		{"metadata email read does not answer, generic wording", func(t *testing.T) {
			useGCPIdentity(t, "", dev)
			t.Setenv("PATH", t.TempDir())
			gcpMetadataPrincipal = func(context.Context) string { return "" }
		}, "the metadata server: this machine's or this pod's identity"},
		{"service-account key file names client_email", func(t *testing.T) { useGCPIdentity(t, "service_account", robot) }, ": " + robot},
		{"authorized_user file cannot name", func(t *testing.T) { useGCPIdentity(t, "authorized_user", ada) }, ": principal unknown"},
		{"gcloud login names the account", func(t *testing.T) {
			useNoADC(t)
			useGcloud(t, ada)
		}, "the active gcloud account (gcloud configuration default): " + ada},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			fake.refuseMethods = map[string]codes.Code{"CreateSecret": codes.PermissionDenied}
			startFakeSecretManager(t, fake)
			row.setUp(t)
			var userinfoCalls int
			restoreUserinfo := gcpUserinfo
			gcpUserinfo = func(context.Context, *google.Credentials) (string, error) {
				userinfoCalls++
				return "", errors.New("the test answers no userinfo")
			}
			t.Cleanup(func() { gcpUserinfo = restoreUserinfo })
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "")

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "shared", "--tier", "agent")
			if code != 1 || !strings.Contains(stderr, "PermissionDenied") {
				t.Fatalf("exit %d, stderr %q; want 1 with the store's PermissionDenied", code, stderr)
			}
			if !strings.Contains(stderr, "the store refused ") || !strings.Contains(stderr, row.refused) {
				t.Fatalf("stderr %q does not name the refused principal %q", stderr, row.refused)
			}
			if _, calls := fake.calledUnder(); calls == 0 {
				t.Fatalf("no store call was made, so the credential never signed in")
			}
			if userinfoCalls != 0 {
				t.Fatalf("the refusal made %d userinfo calls, want none", userinfoCalls)
			}
		})
	}
}
