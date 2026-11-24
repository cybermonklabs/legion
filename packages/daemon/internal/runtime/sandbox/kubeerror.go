package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
)

// kubeError is an error of the runtime's Kubernetes clients as the runtime keeps, decides on and
// prints it (projectKubeError): the operation that failed, in the runtime's own words, and for an
// answer the API server gave, its status code, its reason where apimachinery knows it, the types
// of its causes and how long it asked to be waited out; for an error that carries no answer, a
// closed class. It holds nothing the answering party wrote — no message, no detail's words, no
// URL, no header, no token path — so every printer of it prints only the runtime's words and
// those values, and nothing it answers (Unwrap, As, Is, Status) leads to the original.
//
// Every decision client-go, apimachinery and the runtime take from such an error reads what it
// keeps: it is an apierrors.APIStatus whose Status carries the code, the reason and the causes'
// types (so apierrors' IsNotFound, IsAlreadyExists, IsConflict, IsForbidden, IsInternalError,
// IsTooManyRequests and IsResourceExpired answer as for the original, and the reflector's
// isTooLargeResourceVersionError reads its cause), and two fixed phrases its message keeps where
// the original's held them, which client-go's reflector and serverBusy read: "Too large resource
// version" and "denied the request". An error that carries no answer has a status of code 0 and
// no reason, which every such decision reads as the non-status error it was; errors.As still
// finds the original's syscall.Errno (the reflector's isWatchErrorRetriable backs a refused
// connection off on it), and errors.Is the context's error and io's EOFs it was.
type kubeError struct {
	op string
	// status is whether the API server answered with a status; class says why not otherwise.
	status     bool
	code       int32
	reason     metav1.StatusReason
	causes     []kubeCause
	retryAfter int32
	tooLarge   bool
	denied     bool
	class      kubeClass
	errno      syscall.Errno
	sentinel   error
}

// kubeCause is one cause of a status, by its type, with whether its message was the fixed text a
// server before 1.18.6 wrote for a resource version past its own (tooLargeResourceVersion).
type kubeCause struct {
	kind     metav1.CauseType
	tooLarge bool
}

// kubeClass is why an error of the runtime's Kubernetes clients carries no answer: the request
// ran out of time, was canceled, reached no server or no answer it could read (transport), could
// not be built from the client's configuration, could not read the bearer its client sends
// (credential-read), or got an answer that did not decode.
type kubeClass string

const (
	kubeTimeout        kubeClass = "timeout"
	kubeCanceled       kubeClass = "canceled"
	kubeTransport      kubeClass = "transport"
	kubeConfiguration  kubeClass = "configuration"
	kubeCredentialRead kubeClass = "credential-read"
	kubeDecode         kubeClass = "decode"
)

// kubeReasonOther is the reason a projection keeps for a status reason apimachinery does not
// know: a fixed word, not the one the answer wrote, which no apierrors check reads as a reason of
// its own, so each decides by the status code, as for any reason it does not know.
const kubeReasonOther metav1.StatusReason = "other"

// tooLargeResourceVersion is the text the reflector reads in a Timeout status's message or one of
// its causes' (isTooLargeResourceVersionError) from a server that sets no ResourceVersionTooLarge
// cause: 1.17.0 to 1.18.5, and before.
const tooLargeResourceVersion = "Too large resource version"

// admissionDenied is the phrase of a status whose message says an admission webhook denied the
// request, which serverBusy reads as a refusal whatever the status's reason or code.
const admissionDenied = "denied the request"

// knownReasons are the status reasons apimachinery knows (k8s.io/apimachinery's api/errors,
// knownReasons), each of which an apierrors check may read alone (IsAlreadyExists,
// IsResourceExpired, IsServerTimeout, IsStoreReadError), and the one an answer's status carries
// when it names none.
var knownReasons = map[metav1.StatusReason]bool{
	metav1.StatusReasonUnknown: true, metav1.StatusReasonUnauthorized: true, metav1.StatusReasonForbidden: true,
	metav1.StatusReasonNotFound: true, metav1.StatusReasonAlreadyExists: true, metav1.StatusReasonConflict: true,
	metav1.StatusReasonGone: true, metav1.StatusReasonInvalid: true, metav1.StatusReasonServerTimeout: true,
	metav1.StatusReasonStoreReadError: true, metav1.StatusReasonTimeout: true, metav1.StatusReasonTooManyRequests: true,
	metav1.StatusReasonBadRequest: true, metav1.StatusReasonMethodNotAllowed: true, metav1.StatusReasonNotAcceptable: true,
	metav1.StatusReasonRequestEntityTooLarge: true, metav1.StatusReasonUnsupportedMediaType: true,
	metav1.StatusReasonInternalError: true, metav1.StatusReasonExpired: true, metav1.StatusReasonServiceUnavailable: true,
}

// knownCauses are the cause types apimachinery declares (meta/v1's CauseType constants), and the
// two client-go gives the causes of an error it makes of an answer it could not read
// (UnexpectedServerResponse, a declared one, and ClientWatchDecoding, a watch's).
var knownCauses = map[metav1.CauseType]bool{
	metav1.CauseTypeFieldValueNotFound: true, metav1.CauseTypeFieldValueRequired: true, metav1.CauseTypeFieldValueDuplicate: true,
	metav1.CauseTypeFieldValueInvalid: true, metav1.CauseTypeFieldValueNotSupported: true, metav1.CauseTypeForbidden: true,
	metav1.CauseTypeTooLong: true, metav1.CauseTypeTooMany: true, metav1.CauseTypeInternal: true,
	metav1.CauseTypeTypeInvalid: true, metav1.CauseTypeUnexpectedServerResponse: true,
	metav1.CauseTypeFieldManagerConflict: true, metav1.CauseTypeResourceVersionTooLarge: true,
	"ClientWatchDecoding": true,
}

// projectKubeError is err, an error of one of the runtime's Kubernetes clients for the operation
// op (the runtime's own words, naming nothing an answer wrote), as the runtime keeps and prints
// it: a *kubeError, which keeps of err what every decision on it reads and nothing an answer or a
// request carried. It is the rule for the runtime's Kubernetes errors, applied where each leaves
// client-go: resend, for every read and write of the runtime's requests; the informers' list and
// watch, and each error a watch delivers (projectWatch); a pod log's stream, the events' list,
// and the boot's installation checks. Every decision on err the code takes from the words a
// projection drops is taken from err itself before it is projected (resend's serverBusy), or reads
// the projection's own status. A nil err is nil, and a projection is kept as it is.
func projectKubeError(op string, err error) error {
	if err == nil {
		return nil
	}
	if kept, ok := err.(*kubeError); ok {
		return kept
	}
	e := &kubeError{op: op}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		s := status.Status()
		e.status, e.code, e.reason = true, s.Code, s.Reason
		if !knownReasons[e.reason] {
			e.reason = kubeReasonOther
		}
		e.tooLarge = strings.Contains(s.Message, tooLargeResourceVersion)
		e.denied = strings.Contains(s.Message, admissionDenied)
		if s.Details != nil {
			e.retryAfter = s.Details.RetryAfterSeconds
			for _, cause := range s.Details.Causes {
				kind := cause.Type
				if !knownCauses[kind] {
					kind = metav1.CauseType(kubeReasonOther)
				}
				e.causes = append(e.causes, kubeCause{kind: kind, tooLarge: cause.Message == tooLargeResourceVersion})
			}
		}
		return e
	}
	e.class, e.sentinel = classOf(err)
	errors.As(err, &e.errno)
	return e
}

// classOf is why err, an error of a Kubernetes client that carries no status, carries no answer,
// and the sentinel error of the context or of io it is, which errors.Is finds in a projection.
// client-go hands a body's decoder refusal back as it came, and a bearer file's read refusal as it
// came or inside net/http's *url.Error, so those two are read there and nowhere deeper.
func classOf(err error) (kubeClass, error) {
	var urlErr *url.Error
	var netErr net.Error
	inURL := errors.As(err, &urlErr)
	switch {
	case errors.Is(err, context.Canceled):
		return kubeCanceled, context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return kubeTimeout, context.DeadlineExceeded
	case errors.As(err, &netErr) && netErr.Timeout():
		return kubeTimeout, nil
	case tokenFileError(err) || inURL && urlErr.Err != nil && tokenFileError(urlErr.Err):
		return kubeCredentialRead, nil
	case decodeError(err):
		return kubeDecode, nil
	case inURL && urlErr.Op == "parse":
		return kubeConfiguration, nil
	case errors.Is(err, io.ErrUnexpectedEOF):
		return kubeTransport, io.ErrUnexpectedEOF
	case errors.Is(err, io.EOF):
		return kubeTransport, io.EOF
	}
	return kubeTransport, nil
}

// tokenFileError is whether err is client-go's own refusal to read the file its bearer comes
// from (transport's fileTokenSource): fixed text, and a path the configuration names.
func tokenFileError(err error) bool {
	text := err.Error()
	return strings.HasPrefix(text, "failed to read token file ") || strings.HasPrefix(text, "read empty token from file ")
}

// decodeError is whether err is a decoder's refusal of an answer's body: apimachinery's runtime
// errors for an object it cannot place (which quote the body: runtime.IsMissingKind's
// "Object 'Kind' is missing in '<body>'"), an object of another kind than a Status where a watch
// event says error (apierrors.IsUnexpectedObjectError, which prints the object), encoding/json's
// errors, and client-go's JSON serializer's own refusal of a body that is no JSON
// ("couldn't get version/kind; json parse error: …").
func decodeError(err error) bool {
	var syntax *json.SyntaxError
	var unmarshal *json.UnmarshalTypeError
	return k8sruntime.IsMissingKind(err) || k8sruntime.IsMissingVersion(err) || k8sruntime.IsNotRegisteredError(err) ||
		k8sruntime.IsStrictDecodingError(err) || apierrors.IsUnexpectedObjectError(err) ||
		errors.As(err, &syntax) || errors.As(err, &unmarshal) || strings.HasPrefix(err.Error(), "couldn't get version/kind")
}

func (e *kubeError) Error() string {
	var b strings.Builder
	b.WriteString("kubernetes: ")
	b.WriteString(e.op)
	b.WriteString(": ")
	if !e.status {
		b.WriteString(e.classWords())
		return b.String()
	}
	fmt.Fprintf(&b, "the API server answered %d", e.code)
	if e.reason != "" {
		b.WriteString(" " + string(e.reason))
	}
	if e.denied {
		b.WriteString(" (an admission webhook " + admissionDenied + ")")
	}
	if e.tooLarge {
		b.WriteString(" (" + tooLargeResourceVersion + ")")
	}
	return b.String()
}

// classWords are the fixed words of e's class, with the operating system's name of the errno it
// kept, as net/http's error words it ("connection refused").
func (e *kubeError) classWords() string {
	switch e.class {
	case kubeCanceled:
		return context.Canceled.Error()
	case kubeTimeout:
		if e.sentinel != nil {
			return e.sentinel.Error()
		}
		return "the request timed out"
	case kubeCredentialRead:
		return "the client's bearer token file could not be read"
	case kubeDecode:
		return "the answer did not decode"
	case kubeConfiguration:
		return "the request could not be built from the client's configuration"
	}
	words := "no answer: the transport failed"
	switch {
	case e.errno != 0:
		words += " (" + e.errno.Error() + ")"
	case e.sentinel != nil:
		words += " (" + e.sentinel.Error() + ")"
	}
	return words
}

// Status is what e keeps of the answer's status, apierrors.APIStatus's: its code, its reason and
// its causes' types, with no word the answer wrote, under a message that is e's own text. An
// error that carries no answer has code 0 and no reason.
func (e *kubeError) Status() metav1.Status {
	s := metav1.Status{Status: metav1.StatusFailure, Code: e.code, Reason: e.reason, Message: e.Error()}
	if len(e.causes) > 0 || e.retryAfter != 0 {
		s.Details = &metav1.StatusDetails{RetryAfterSeconds: e.retryAfter}
		for _, cause := range e.causes {
			c := metav1.StatusCause{Type: cause.kind}
			if cause.tooLarge {
				c.Message = tooLargeResourceVersion
			}
			s.Details.Causes = append(s.Details.Causes, c)
		}
	}
	return s
}

// Is answers the context's error and io's EOFs e was.
func (e *kubeError) Is(target error) bool { return e.sentinel != nil && target == e.sentinel }

// As fills the errno e kept, and nothing else of the original.
func (e *kubeError) As(target any) bool {
	if errno, ok := target.(*syscall.Errno); ok && e.errno != 0 {
		*errno = e.errno
		return true
	}
	return false
}

// projectWatch is w, a watch of one of the runtime's informers for op, with the Status each of its
// error events carries rewritten as the projection of the error it says (projectKubeError): the
// reflector reads an error event's Status, as apierrors.FromObject makes an error of it, logs it
// and decides from it, and no watch-error handler sees it. A typed watch's error event carries a
// *metav1.Status, the dynamic client's an unstructured Status, and either may carry an object that
// is no Status, which FromObject prints whole; every one becomes the *metav1.Status of its
// projection. Its own goroutine forwards w's events until w's channel closes or the watch is
// stopped, so a reader that stopped reading leaves none of it waiting.
func projectWatch(op string, w watch.Interface) watch.Interface {
	p := &projectedWatch{Interface: w, op: op, result: make(chan watch.Event), stopped: make(chan struct{})}
	go p.forward()
	return p
}

type projectedWatch struct {
	watch.Interface
	op      string
	result  chan watch.Event
	stopped chan struct{}
	stop    sync.Once
}

func (p *projectedWatch) ResultChan() <-chan watch.Event { return p.result }

func (p *projectedWatch) Stop() {
	p.stop.Do(func() { close(p.stopped) })
	p.Interface.Stop()
}

func (p *projectedWatch) forward() {
	defer close(p.result)
	for event := range p.Interface.ResultChan() {
		if event.Type == watch.Error {
			event.Object = projectedStatus(projectKubeError(p.op, apierrors.FromObject(event.Object)))
		}
		select {
		case p.result <- event:
		case <-p.stopped:
			return
		}
	}
}

// projectedStatus is err as the Status a watch's error event carries: its own where it has one,
// and its text alone otherwise.
func projectedStatus(err error) *metav1.Status {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		s := status.Status()
		return &s
	}
	return &metav1.Status{Status: metav1.StatusFailure, Message: err.Error()}
}

// RouteClientLogs routes client-go's own logging (klog: its reflectors', its token source's,
// apimachinery's error handlers') through log, each error a line holds as the runtime's projection
// of it (projectKubeError), so no line client-go writes holds a Kubernetes error's words, whatever
// error reached it. It is process-wide (klog.SetSlogLogger): the daemon's boot calls it once,
// before it builds any client. klog's leveled lines print at slog's level of minus their
// verbosity, so where log's handler takes Debug (-4) the reflector's V(4) lines print through it,
// as client-go's V(8) request and answer bodies never do; a line klog formats itself, such as
// k8s.io/utils' trace at verbosity 2, which no daemon sets, carries its text in its message, which
// no handler can project, and the runtime's informers hand the reflector its errors projected.
func RouteClientLogs(log *slog.Logger) {
	klog.SetSlogLogger(slog.New(projectingHandler{log.Handler()}))
}

// projectingHandler is a slog.Handler that hands h each record with every error its attributes
// hold, at any depth of groups, as its projection.
type projectingHandler struct{ h slog.Handler }

func (p projectingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return p.h.Enabled(ctx, level)
}

func (p projectingHandler) Handle(ctx context.Context, record slog.Record) error {
	projected := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(a slog.Attr) bool {
		projected.AddAttrs(projectAttr(a))
		return true
	})
	return p.h.Handle(ctx, projected)
}

func (p projectingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	projected := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		projected[i] = projectAttr(a)
	}
	return projectingHandler{p.h.WithAttrs(projected)}
}

func (p projectingHandler) WithGroup(name string) slog.Handler {
	return projectingHandler{p.h.WithGroup(name)}
}

// projectAttr is a with the error its value holds, or the errors its group's attributes hold, as
// their projections.
func projectAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindGroup:
		group := a.Value.Group()
		projected := make([]slog.Attr, len(group))
		for i, inner := range group {
			projected[i] = projectAttr(inner)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(projected...)}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.Any(a.Key, projectKubeError("client-go", err))
		}
	}
	return a
}
