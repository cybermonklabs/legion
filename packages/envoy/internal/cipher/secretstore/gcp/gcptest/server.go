package gcptest

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// FakeSecret is one secret FakeSecretManager holds: Secret is its metadata as the API answers it
// (annotations, labels, replication, create and expire times, etag), Versions its versions in the
// order they were made, and Payloads[i] Versions[i]'s value.
type FakeSecret struct {
	Secret      *secretmanagerpb.Secret
	Versions    []*secretmanagerpb.SecretVersion
	Payloads    [][]byte
	EtagCounter int
	// DenyVersion makes GetSecretVersion on the secret answer PERMISSION_DENIED, as for a secret
	// outside the namespace the broker's condition grants it.
	DenyVersion bool
	// KeyUnusable makes AccessSecretVersion answer FAILED_PRECONDITION on an ENABLED version, as
	// an unusable CMEK key might.
	KeyUnusable bool
}

// Value is the secret's newest version's value, nil for a secret with none.
func (s *FakeSecret) Value() []byte {
	if len(s.Payloads) == 0 {
		return nil
	}
	return s.Payloads[len(s.Payloads)-1]
}

// FakeSecretManager is an in-memory Secret Manager server answering every call the GCP store and
// its writer make as the real service answers them: resource names with the project number,
// NOT_FOUND for a missing secret or version, FAILED_PRECONDITION for a stale etag and for a
// disabled or destroyed version's access. A test that reads or changes its fields while a client
// may call it holds Mu. Start serves it to the broker's own client over TLS; UseFake serves it to
// the agent-secrets writer.
type FakeSecretManager struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer

	Mu      sync.Mutex
	Secrets map[string]*FakeSecret // full resource name -> secret
	// FailList, when set, is what ListSecrets answers.
	FailList error
	// Calls counts each method's calls the fake answered or refused, and Unavailable is how many
	// calls of each method still answer UNAVAILABLE first, as a transient outage does.
	Calls, Unavailable map[string]int
	// RefuseAnnotationUpdates, when set, makes UpdateSecret refuse an annotations-field-mask
	// request with PermissionDenied, as IAM refuses a person changing a shared secret's tags.
	RefuseAnnotationUpdates bool
	// Race, when set, runs once on the secret named by the first GetSecret or UpdateSecret after the
	// first GetSecret, before that call is answered, as another writer's update landing between a
	// read and the next call does; gets counts GetSecret calls.
	Race  func(*FakeSecret)
	Gets  int
	Raced bool
	// Refuse, when set, is an authorization every call carrying it is refused with
	// PermissionDenied, as IAM refuses an identity it grants no role on the secrets.
	Refuse string
	// RefuseMethods is, for each method it names (CreateSecret, AddSecretVersion, DeleteSecret,
	// ...), the code every call of that method is refused with, as IAM refuses an identity one
	// permission it does not grant while granting it the others.
	RefuseMethods map[string]codes.Code
	// RefuseText is added to the text of a refusal RefuseMethods makes, as the service's own words,
	// which no line agent-secrets prints may carry.
	RefuseText string
	// AnswerAfter is, for each method it names, the code every call of that method is answered
	// with once the fake has applied it, as a call the server carried out and whose answer was lost
	// (DeadlineExceeded, Unavailable) reaches the caller. beforeCall, when set, runs with each
	// call's method before the fake answers it, outside f.mu.
	AnswerAfter map[string]codes.Code
	BeforeCall  func(method string)
	AfterCall   func(method string)
	// Authorizations is the authorization metadata each call carried, in order: the sign-in it
	// ran under (Intercept). methods is the method each call named, in the same order.
	Authorizations []string
	Methods        []string
	// RefuseAccess, when not OK, is the code every AccessSecretVersion answers: PERMISSION_DENIED
	// as IAM answers a sign-in without secretmanager.versions.access, or any other failure;
	// accesses counts the AccessSecretVersion calls, answered or refused.
	RefuseAccess codes.Code
	Accesses     int
	// Withheld are the permissions TestIamPermissions answers the caller does not hold, of those
	// it asks about; it answers every other one held, as IAM does for a sign-in that holds them.
	Withheld []string
}

// raceOnce runs f.race on entry once, on a call after the first GetSecret.
func (f *FakeSecretManager) raceOnce(entry *FakeSecret) {
	if f.Race != nil && !f.Raced && f.Gets > 0 {
		f.Raced = true
		f.Race(entry)
	}
}

// Intercept notes the authorization metadata and the method of each call before the
// fake answers it, as the real API reads the caller's sign-in from it, runs f.beforeCall, refuses
// a call carrying f.refuse, or naming a method f.refuseMethods refuses, and answers a call naming
// a method f.answerAfter names with that code once it has applied it.
func (f *FakeSecretManager) Intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	authorization := strings.Join(md.Get("authorization"), ",")
	method := info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]
	f.Mu.Lock()
	f.Authorizations = append(f.Authorizations, authorization)
	f.Methods = append(f.Methods, method)
	refused := f.Refuse != "" && authorization == f.Refuse
	code, after, before, text := f.RefuseMethods[method], f.AnswerAfter[method], f.BeforeCall, f.RefuseText
	f.Mu.Unlock()
	if before != nil {
		before(method)
	}
	if refused {
		return nil, status.Errorf(codes.PermissionDenied, "Permission denied on %s for the caller", info.FullMethod)
	}
	if code != codes.OK {
		return nil, status.Errorf(code, "the fake refuses %s%s", info.FullMethod, text)
	}
	resp, err := handler(ctx, req)
	if f.AfterCall != nil {
		f.AfterCall(method)
	}
	if err == nil && after != codes.OK {
		return nil, status.Errorf(after, "the fake applied %s and answers it failed", info.FullMethod)
	}
	return resp, err
}

// Called is the method of every call so far, in order, refused ones included.
func (f *FakeSecretManager) Called() []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return slices.Clone(f.Methods)
}

// CalledUnder is the authorization every call so far carried, and how many calls there were; ""
// when the calls carried different ones or none was made.
func (f *FakeSecretManager) CalledUnder() (authorization string, calls int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for i, a := range f.Authorizations {
		if i > 0 && a != authorization {
			return "", len(f.Authorizations)
		}
		authorization = a
	}
	return authorization, len(f.Authorizations)
}

// Written is how many secrets f holds.
func (f *FakeSecretManager) Written() int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return len(f.Secrets)
}

// Reads is the calls made so far that Secret Manager counts against its 600-per-minute read
// quota: every metadata read, which is every call but AccessSecretVersion and the writes.
func (f *FakeSecretManager) Reads() int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Calls["ListSecrets"] + f.Calls["GetSecret"] + f.Calls["GetSecretVersion"]
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

// NewFakeSecretManager is a fake holding no secret.
func NewFakeSecretManager() *FakeSecretManager {
	return &FakeSecretManager{Secrets: map[string]*FakeSecret{}}
}

func (f *FakeSecretManager) CreateSecret(_ context.Context, req *secretmanagerpb.CreateSecretRequest) (*secretmanagerpb.Secret, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("CreateSecret"); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/secrets/" + req.GetSecretId()
	if _, ok := f.Secrets[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "already exists")
	}
	secret := req.GetSecret()
	secret.Name = name
	secret.Etag = "1"
	f.Secrets[name] = &FakeSecret{Secret: secret, EtagCounter: 1}
	return secret, nil
}

func (f *FakeSecretManager) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("GetSecret"); err != nil {
		return nil, err
	}
	entry, ok := f.Secrets[req.GetName()]
	if ok {
		f.raceOnce(entry)
	}
	f.Gets++
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Secret [%s] not found", req.GetName())
	}
	return entry.Secret, nil
}

// UpdateSecret refuses an Etag that does not match the secret's current one with
// FAILED_PRECONDITION, as the real API does for optimistic concurrency, and bumps the etag on
// every applied update; a delete or restore, which sends no Etag, skips the check.
func (f *FakeSecretManager) UpdateSecret(_ context.Context, req *secretmanagerpb.UpdateSecretRequest) (*secretmanagerpb.Secret, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("UpdateSecret"); err != nil {
		return nil, err
	}
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path == "annotations" && f.RefuseAnnotationUpdates {
			return nil, status.Error(codes.PermissionDenied, "caller does not have permission to update this secret's annotations")
		}
	}
	entry, ok := f.Secrets[req.GetSecret().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	f.raceOnce(entry)
	if etag := req.GetSecret().GetEtag(); etag != "" && etag != entry.Secret.GetEtag() {
		return nil, status.Error(codes.FailedPrecondition, "etag mismatch")
	}
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "annotations":
			entry.Secret.Annotations = req.GetSecret().GetAnnotations()
		case "expire_time":
			entry.Secret.Expiration = req.GetSecret().GetExpiration()
		}
	}
	entry.EtagCounter++
	entry.Secret.Etag = strconv.Itoa(entry.EtagCounter)
	return entry.Secret, nil
}

// ListSecrets answers, in one page, every secret of the request's project, and of them, under a
// name: filter, those whose id holds the filter's text, as Secret Manager's ":" matches: a
// case-insensitive substring.
func (f *FakeSecretManager) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("ListSecrets"); err != nil {
		return nil, err
	}
	if f.FailList != nil {
		return nil, f.FailList
	}
	needle, filtered := strings.CutPrefix(req.GetFilter(), "name:")
	if req.GetFilter() != "" && !filtered {
		return nil, status.Errorf(codes.InvalidArgument, "filter %q", req.GetFilter())
	}
	resp := &secretmanagerpb.ListSecretsResponse{}
	for name, entry := range f.Secrets {
		id, under := strings.CutPrefix(name, req.GetParent()+"/secrets/")
		if under && strings.Contains(strings.ToLower(id), strings.ToLower(needle)) {
			resp.Secrets = append(resp.Secrets, entry.Secret)
		}
	}
	resp.TotalSize = int32(len(resp.Secrets))
	return resp, nil
}

func (f *FakeSecretManager) AddSecretVersion(_ context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("AddSecretVersion"); err != nil {
		return nil, err
	}
	entry, ok := f.Secrets[req.GetParent()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	number := len(entry.Versions) + 1
	name := req.GetParent() + "/versions/" + strconv.Itoa(number)
	version := &secretmanagerpb.SecretVersion{Name: name, State: secretmanagerpb.SecretVersion_ENABLED}
	entry.Versions = append(entry.Versions, version)
	entry.Payloads = append(entry.Payloads, req.GetPayload().GetData())
	return version, nil
}

// DeleteSecret deletes the secret outright, with every version it holds, as the real API does.
func (f *FakeSecretManager) DeleteSecret(_ context.Context, req *secretmanagerpb.DeleteSecretRequest) (*emptypb.Empty, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("DeleteSecret"); err != nil {
		return nil, err
	}
	entry, ok := f.Secrets[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	if etag := req.GetEtag(); etag != "" && etag != entry.Secret.GetEtag() {
		return nil, status.Error(codes.FailedPrecondition, "etag mismatch")
	}
	delete(f.Secrets, req.GetName())
	return &emptypb.Empty{}, nil
}

// AccessSecretVersion answers versions/latest, the one version name the store and the forms
// access, with the payload of the newest version, or RefuseAccess when that is set. A newest
// version that is not ENABLED, or one whose key is unusable (KeyUnusable), is refused
// FAILED_PRECONDITION, as the real API refuses a disabled or destroyed version's access.
func (f *FakeSecretManager) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("AccessSecretVersion"); err != nil {
		return nil, err
	}
	f.Accesses++
	if f.RefuseAccess != codes.OK {
		return nil, status.Error(f.RefuseAccess, "the fake refuses access to "+req.GetName())
	}
	parent, latest := strings.CutSuffix(req.GetName(), "/versions/latest")
	entry, ok := f.Secrets[parent]
	if !latest || !ok || len(entry.Versions) == 0 {
		return nil, status.Errorf(codes.NotFound, "Secret Version [%s] not found", req.GetName())
	}
	newest := len(entry.Versions) - 1
	if state := entry.Versions[newest].GetState(); state != secretmanagerpb.SecretVersion_ENABLED || entry.KeyUnusable {
		return nil, status.Errorf(codes.FailedPrecondition, "Secret Version [%s] is in %s state", req.GetName(), state)
	}
	var data []byte
	if newest < len(entry.Payloads) {
		data = entry.Payloads[newest]
	}
	return &secretmanagerpb.AccessSecretVersionResponse{Name: entry.Versions[newest].GetName(), Payload: &secretmanagerpb.SecretPayload{Data: data}}, nil
}

func (f *FakeSecretManager) GetSecretVersion(_ context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("GetSecretVersion"); err != nil {
		return nil, err
	}
	parent := req.GetName()[:max(strings.LastIndex(req.GetName(), "/versions/"), 0)]
	entry, ok := f.Secrets[parent]
	if !ok || len(entry.Versions) == 0 {
		return nil, status.Errorf(codes.NotFound, "Secret Version [%s] not found", req.GetName())
	}
	if entry.DenyVersion {
		return nil, status.Errorf(codes.PermissionDenied, "Permission denied on %s", req.GetName())
	}
	if strings.HasSuffix(req.GetName(), "/versions/latest") {
		return entry.Versions[len(entry.Versions)-1], nil
	}
	for _, v := range entry.Versions {
		if v.GetName() == req.GetName() {
			return v, nil
		}
	}
	return nil, status.Error(codes.NotFound, "not found")
}

func (f *FakeSecretManager) ListSecretVersions(_ context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("ListSecretVersions"); err != nil {
		return nil, err
	}
	entry, ok := f.Secrets[req.GetParent()]
	if !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &secretmanagerpb.ListSecretVersionsResponse{Versions: entry.Versions, TotalSize: int32(len(entry.Versions))}, nil
}

// TestIamPermissions answers, of the permissions asked about on the secret, every one f does not
// withhold, as IAM answers a sign-in the permissions its roles grant on that resource. It refuses a
// secret it does not hold NotFound.
func (f *FakeSecretManager) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if err := f.call("TestIamPermissions"); err != nil {
		return nil, err
	}
	if _, ok := f.Secrets[req.GetResource()]; !ok {
		return nil, status.Error(codes.NotFound, "not found")
	}
	held := slices.DeleteFunc(slices.Clone(req.GetPermissions()), func(p string) bool { return slices.Contains(f.Withheld, p) })
	return &iampb.TestIamPermissionsResponse{Permissions: held}, nil
}
