package fakegcs

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The parts of Google's V4 signing scheme for Cloud Storage
// (https://cloud.google.com/storage/docs/authentication/signatures) that a signed download the
// double serves must carry: the one algorithm, the form of X-Goog-Date, the longest life an address
// may state, and the payload hash of an address that signs no payload.
const (
	v4Algorithm       = "GOOG4-RSA-SHA256"
	v4DateFormat      = "20060102T150405Z"
	v4MaxExpires      = 604800
	v4UnsignedPayload = "UNSIGNED-PAYLOAD"
)

// v4Parameters are the query parameters a V4 signed address carries, each exactly once.
var v4Parameters = []string{"X-Goog-Algorithm", "X-Goog-Credential", "X-Goog-Date", "X-Goog-Expires", "X-Goog-SignedHeaders", "X-Goog-Signature"}

// Verifier checks a request against Google's V4 signing scheme for Cloud Storage, as Google's
// description of the scheme states it, not as any code that makes addresses reads it: a test holds
// it to the known answers of the pinned Storage module's conformance file. It takes an address
// signed by one of Signers, by email, with GOOG4-RSA-SHA256 over the host header alone and an
// unsigned payload, whose credential's scope is <date>/auto/storage/goog4_request on X-Goog-Date's
// day, and which Now, time.Now when nil, finds before X-Goog-Date plus X-Goog-Expires (at most
// 604800 seconds). That is the one form the cache signs a download in.
type Verifier struct {
	Signers map[string]*rsa.PublicKey
	Now     func() time.Time
}

// Refusal is why a Verifier refused a request: the code Cloud Storage's XML error answer names
// (AccessDenied, SignatureDoesNotMatch or ExpiredToken) and what the verifier found, and, for a
// signature that does not match, the canonical request and the string it signed, as Cloud Storage
// shows them, so a client's author can compare them with its own.
type Refusal struct {
	Code             string
	Details          string
	CanonicalRequest string
	StringToSign     string
}

// Error is the refusal's code and details.
func (r *Refusal) Error() string { return r.Code + ": " + r.Details }

// accessDenied is a Refusal with code AccessDenied and details.
func accessDenied(details string) *Refusal {
	return &Refusal{Code: "AccessDenied", Details: details}
}

// Verify checks a request of method for u, received with Host host, and answers the bucket and the
// object name its path names, or the Refusal that says why it does not hold. The canonical request
// signs the host without its port, as the conformance file's "Simple GET with non-default hostname"
// row signs localhost:8080 as host:localhost, and the path and query as RFC 3986 encodes them.
func (v Verifier) Verify(method, host string, u *url.URL) (bucket, object string, err error) {
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", "", accessDenied("the address's query does not parse: " + err.Error())
	}
	if !slices.ContainsFunc(v4Parameters, query.Has) {
		return "", "", accessDenied("Anonymous caller does not have storage.objects.get access to the Google Cloud Storage object. The address carries no V4 signature.")
	}
	for _, name := range v4Parameters {
		if n := len(query[name]); n != 1 {
			return "", "", accessDenied(fmt.Sprintf("the address carries %s %d times, not once", name, n))
		}
	}
	if got := query.Get("X-Goog-Algorithm"); got != v4Algorithm {
		return "", "", accessDenied(fmt.Sprintf("the address's X-Goog-Algorithm is %q, not %s", got, v4Algorithm))
	}
	date := query.Get("X-Goog-Date")
	signedAt, err := time.Parse(v4DateFormat, date)
	if err != nil {
		return "", "", accessDenied(fmt.Sprintf("the address's X-Goog-Date %q is no time of the form %s", date, v4DateFormat))
	}
	credential := query.Get("X-Goog-Credential")
	email, scope, _ := strings.Cut(credential, "/")
	if want := signedAt.Format("20060102") + "/auto/storage/goog4_request"; email == "" || scope != want {
		return "", "", accessDenied(fmt.Sprintf("the address's X-Goog-Credential %q is not <email>/%s", credential, want))
	}
	expires, err := strconv.ParseInt(query.Get("X-Goog-Expires"), 10, 64)
	if err != nil || expires < 1 || expires > v4MaxExpires {
		return "", "", accessDenied(fmt.Sprintf("the address's X-Goog-Expires %q is not a whole number of seconds from 1 to %d", query.Get("X-Goog-Expires"), v4MaxExpires))
	}
	if got := query.Get("X-Goog-SignedHeaders"); got != "host" {
		return "", "", accessDenied(fmt.Sprintf("the address's X-Goog-SignedHeaders is %q: a download the cache signs signs the host header alone", got))
	}
	key := v.Signers[email]
	if key == nil {
		return "", "", accessDenied(fmt.Sprintf("the address is signed as %s, which is no signer this double knows", email))
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if end := signedAt.Add(time.Duration(expires) * time.Second); !now().Before(end) {
		return "", "", &Refusal{Code: "ExpiredToken", Details: "The provided token has expired. Request signature expired at: " + end.UTC().Format(time.RFC3339)}
	}
	canonical, toSign := v4Canonical(method, host, u.Path, query)
	signature, err := hex.DecodeString(query.Get("X-Goog-Signature"))
	digest := sha256.Sum256([]byte(toSign))
	if err != nil || rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return "", "", &Refusal{
			Code:             "SignatureDoesNotMatch",
			Details:          "The request signature we calculated does not match the signature you provided. Check your Google secret key and signing method.",
			CanonicalRequest: canonical,
			StringToSign:     toSign,
		}
	}
	bucket, object, _ = strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if bucket == "" || object == "" {
		return "", "", accessDenied(fmt.Sprintf("the address's path %q names no bucket and object", u.Path))
	}
	return bucket, object, nil
}

// v4Canonical builds the V4 canonical request of a request of method for path (decoded) and query,
// received with Host host, and the string to sign over it: the method, the path and the query
// without X-Goog-Signature, each RFC 3986 encoded and the query sorted, the host header without its
// port, the signed headers ("host"), and the unsigned payload; then the algorithm, X-Goog-Date, the
// credential's scope and the canonical request's SHA-256 in hex.
func v4Canonical(method, host, path string, query url.Values) (canonical, toSign string) {
	type pair struct{ name, value string }
	var pairs []pair
	for name, values := range query {
		if name == "X-Goog-Signature" {
			continue
		}
		for _, value := range values {
			pairs = append(pairs, pair{rfc3986(name, false), rfc3986(value, false)})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		return strings.Compare(a.value, b.value)
	})
	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = p.name + "=" + p.value
	}
	if path == "" {
		path = "/"
	}
	canonical = strings.Join([]string{
		method,
		rfc3986(path, true),
		strings.Join(encoded, "&"),
		"host:" + (&url.URL{Host: host}).Hostname(),
		"",
		"host",
		v4UnsignedPayload,
	}, "\n")
	date := query.Get("X-Goog-Date")
	_, scope, _ := strings.Cut(query.Get("X-Goog-Credential"), "/")
	digest := sha256.Sum256([]byte(canonical))
	return canonical, strings.Join([]string{v4Algorithm, date, scope, hex.EncodeToString(digest[:])}, "\n")
}

// rfc3986 percent-encodes s as RFC 3986 asks of a V4 canonical request: every byte but the
// unreserved letters, digits, '-', '.', '_' and '~', and '/' too where slash is true (a path's
// separators), as %XX in upper-case hex.
func rfc3986(s string, slash bool) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' || slash && c == '/' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&0xf])
	}
	return b.String()
}

// xmlError is Cloud Storage's XML error answer, which its XML API, the one a signed download
// reaches, answers a refusal with.
type xmlError struct {
	XMLName          xml.Name `xml:"Error"`
	Code             string   `xml:"Code"`
	Message          string   `xml:"Message"`
	Details          string   `xml:"Details,omitempty"`
	StringToSign     string   `xml:"StringToSign,omitempty"`
	CanonicalRequest string   `xml:"CanonicalRequest,omitempty"`
}

// writeXMLError answers status with Cloud Storage's XML error.
func writeXMLError(w http.ResponseWriter, status int, answer xmlError) {
	body, err := xml.Marshal(answer)
	if err != nil {
		http.Error(w, "fakegcs: encode the answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_, _ = w.Write(body)
}

// download serves a signed download, a path-style GET /<bucket>/<object> of Cloud Storage's XML
// API: 403 with the Refusal where the double's Verifier refuses the address, 404 where it holds no
// such object, and otherwise the object's bytes, streamed from its file with http.ServeContent,
// with the headers Cloud Storage sends of an object.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeXMLError(w, http.StatusMethodNotAllowed, xmlError{Code: "MethodNotAllowed", Message: "fakegcs serves a signed GET alone, not " + r.Method + "."})
		return
	}
	bucket, name, err := s.verifier.Verify(r.Method, r.Host, r.URL)
	if err != nil {
		var refusal *Refusal
		_ = errors.As(err, &refusal)
		message := "Access denied."
		switch refusal.Code {
		case "SignatureDoesNotMatch":
			message = refusal.Details
		case "ExpiredToken":
			message = "Invalid argument."
		}
		writeXMLError(w, http.StatusForbidden, xmlError{Code: refusal.Code, Message: message, Details: refusal.Details, StringToSign: refusal.StringToSign, CanonicalRequest: refusal.CanonicalRequest})
		return
	}
	s.mu.Lock()
	o := s.objects[objectKey{bucket, name}]
	var f *os.File
	if o != nil {
		f, err = os.Open(o.path)
	}
	s.mu.Unlock()
	switch {
	case o == nil:
		writeXMLError(w, http.StatusNotFound, xmlError{Code: "NoSuchKey", Message: "The specified key does not exist.", Details: "No such object: " + bucket + "/" + name})
		return
	case err != nil:
		writeXMLError(w, http.StatusInternalServerError, xmlError{Code: "InternalError", Message: "fakegcs: open the object's bytes: " + err.Error()})
		return
	}
	defer f.Close()
	header := w.Header()
	header.Set("Content-Type", o.contentType)
	header.Set("ETag", `"`+hex.EncodeToString(o.md5)+`"`)
	header.Set("X-Goog-Generation", strconv.FormatInt(o.generation, 10))
	header.Set("X-Goog-Metageneration", "1")
	header.Set("X-Goog-Stored-Content-Length", strconv.FormatInt(o.size, 10))
	header.Set("X-Goog-Stored-Content-Encoding", "identity")
	header.Add("X-Goog-Hash", "crc32c="+encodeCRC32C(o.crc32c))
	header.Add("X-Goog-Hash", "md5="+base64.StdEncoding.EncodeToString(o.md5))
	http.ServeContent(w, r, "", o.created, f)
}

// rpcError is a Google API's JSON error answer, IAM's among them, as testdata/iam-sign-403.json
// holds it: the status, the message, the RPC status name, and the error's details.
type rpcError struct {
	Error rpcErrorBody `json:"error"`
}

// rpcErrorBody is an rpcError's one field.
type rpcErrorBody struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Status  string         `json:"status"`
	Details []rpcErrorInfo `json:"details,omitempty"`
}

// rpcErrorInfo is one detail of an rpcError, a google.rpc.ErrorInfo: why, in which domain, and the
// metadata IAM gives a refusal of a permission.
type rpcErrorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata"`
}

// writeRPCError answers code with a Google API's JSON error of status, message and details.
func writeRPCError(w http.ResponseWriter, code int, status, message string, details []rpcErrorInfo) {
	writeJSON(w, code, rpcError{Error: rpcErrorBody{Code: code, Message: message, Status: status, Details: details}})
}

// signBlobDenied answers IAM's refusal of signBlob to a caller that may not sign as the account, in
// testdata/iam-sign-403.json's shape, with an error id of its own.
func signBlobDenied(w http.ResponseWriter) {
	id := strings.ToLower(rand.Text())
	troubleshooter := "https://console.cloud.google.com/iam-admin/troubleshooter/summary;errorId=" + id
	writeRPCError(w, http.StatusForbidden, "PERMISSION_DENIED",
		"Permission 'iam.serviceAccounts.signBlob' denied on resource (or it may not exist). Remediate access with this Troubleshooter URL or share it with your administrator - "+troubleshooter+" .",
		[]rpcErrorInfo{{
			Type:   "type.googleapis.com/google.rpc.ErrorInfo",
			Reason: "IAM_PERMISSION_DENIED",
			Domain: "iam.googleapis.com",
			Metadata: map[string]string{
				"error_info_id":      id,
				"permission":         "iam.serviceAccounts.signBlob",
				"troubleshooter_url": troubleshooter,
			},
		}})
}

// signBlobRequest is the IAM Credentials API's SignBlobRequest: the bytes to sign, in base64, and a
// delegation chain, which the double does not implement.
type signBlobRequest struct {
	Payload   string   `json:"payload"`
	Delegates []string `json:"delegates"`
}

// signBlobResponse is the IAM Credentials API's SignBlobResponse, as testdata/iam-sign-200.json
// holds it: the id of the key that signed, and the signature in base64.
type signBlobResponse struct {
	KeyID      string `json:"keyId"`
	SignedBlob string `json:"signedBlob"`
}

// signBlob answers POST /v1/projects/<project>/serviceAccounts/<email>:signBlob, IAM Credentials'
// signBlob: an RSA-SHA256 PKCS #1 v1.5 signature of the payload with the double's key, for its own
// account alone, as IAM signs with a Google-managed key of the account. It answers IAM's 403 for any
// other account and while RefuseSigning is on.
func (s *Server) signBlob(w http.ResponseWriter, r *http.Request) {
	email, ok := signBlobAccount(r.URL.Path)
	if !ok {
		writeRPCError(w, http.StatusNotFound, "NOT_FOUND", "fakegcs serves projects.serviceAccounts.signBlob alone under /v1/, not "+r.URL.Path, nil)
		return
	}
	if r.Method != http.MethodPost {
		writeRPCError(w, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "signBlob takes POST, not "+r.Method, nil)
		return
	}
	s.mu.Lock()
	refuse := s.refuseSigning
	s.mu.Unlock()
	if refuse || email != s.email {
		signBlobDenied(w)
		return
	}
	var req signBlobRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxResourceBytes)).Decode(&req); err != nil {
		writeRPCError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid JSON payload received. "+err.Error(), nil)
		return
	}
	if len(req.Delegates) > 0 {
		writeRPCError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "fakegcs signs as its own account alone, through no delegation chain", nil)
		return
	}
	payload, err := base64.StdEncoding.DecodeString(req.Payload)
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(req.Payload)
	}
	if err != nil || len(payload) == 0 {
		writeRPCError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid value at 'payload' (TYPE_BYTES): it is no bytes in base64", nil)
		return
	}
	digest := sha256.Sum256(payload)
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		writeRPCError(w, http.StatusInternalServerError, "INTERNAL", "fakegcs: sign the payload: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, signBlobResponse{KeyID: s.keyID, SignedBlob: base64.StdEncoding.EncodeToString(signature)})
}

// signBlobAccount reads the account a signBlob path names, decoded:
// /v1/projects/<project>/serviceAccounts/<email>:signBlob, where the project is "-" or any one
// segment.
func signBlobAccount(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/v1/projects/")
	if !ok {
		return "", false
	}
	project, rest, ok := strings.Cut(rest, "/serviceAccounts/")
	if !ok || project == "" || strings.Contains(project, "/") {
		return "", false
	}
	email, ok := strings.CutSuffix(rest, ":signBlob")
	if !ok || email == "" || strings.Contains(email, "/") {
		return "", false
	}
	return email, true
}

// metadataFlavored answers the metadata server's 403 and false for a request without the header
// Metadata-Flavor: Google, which the metadata server requires of every request, and for any method
// but GET its 405. Every answer carries Metadata-Flavor: Google, as the metadata server's do.
func metadataFlavored(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Metadata-Flavor", "Google")
	if r.Header.Get("Metadata-Flavor") != "Google" {
		http.Error(w, `Missing required header "Metadata-Flavor": "Google"`, http.StatusForbidden)
		return false
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// metadataEmail answers the metadata server's instance/service-accounts/default/email: the
// account's address as plain text, with no line feed, as testdata/metadata-email.txt holds it.
func (s *Server) metadataEmail(w http.ResponseWriter, r *http.Request) {
	if !metadataFlavored(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/text")
	_, _ = io.WriteString(w, s.email)
}

// tokenAnswer is the metadata server's answer to instance/service-accounts/default/token, as
// testdata/metadata-token.json holds it.
type tokenAnswer struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// metadataToken answers the metadata server's instance/service-accounts/default/token: the
// double's token, which Options.RequireToken makes every Storage and IAM request present, good for
// the hour less a second the metadata server states. It ignores the scopes a client asks for.
func (s *Server) metadataToken(w http.ResponseWriter, r *http.Request) {
	if !metadataFlavored(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, tokenAnswer{AccessToken: s.token, ExpiresIn: 3599, TokenType: "Bearer"})
}
