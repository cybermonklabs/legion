package testwait

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// spanGrace is how long after a span ends a failure can still be that span's give-up. A test that
// already failed before it took the span is not diagnosed under the span.
const spanGrace = time.Second

// Bound is one bound an internal testwait package handed to a wait. A deadline bound ends at the
// test binary's deadline less a caller's spare; a span ends by itself.
type Bound struct {
	Function       string
	EndDescription string
	End            time.Time
	Span           bool
	// Site is the caller's source line. Empty makes Watch find its direct caller.
	Site string
}

type observedBound struct {
	Bound
	started       time.Time
	startedFailed bool
	site          string
}

func (b observedBound) String() string {
	if !b.started.Before(b.End) {
		return fmt.Sprintf("testwait.%s gave an already-spent bound at %s", b.Function, clockOf(b.End))
	}
	return fmt.Sprintf("the bound of %s testwait.%s gave at %s at %s, which ends at %s",
		b.End.Sub(b.started).Round(time.Millisecond), b.Function, b.site, clockOf(b.started), clockOf(b.End))
}

// watcher is one test's bounds. Its cleanup runs in the test goroutine, so a span's failure stack
// is read during FailNow's cleanup before its wait's frames have unwound. No timer goroutine reads
// testing.T.Failed: testing deliberately writes its done field without a lock after cleanup.
type watcher struct {
	mu       sync.Mutex
	bounds   []observedBound
	explicit bool
	ended    bool
}

var watchers sync.Map

// Watch records a bound handed to a wait. A passing test has no stack read: only its cleanup, when
// failed, selects the latest bound that ended. A span taken after t.Error is skipped, so an earlier
// assertion cannot label a later held negative check as the cause.
func Watch(t testing.TB, bound Bound) {
	key := watcherKey(t)
	value, loaded := watchers.LoadOrStore(key, &watcher{})
	w := value.(*watcher)
	if !loaded {
		t.Cleanup(func() { w.finish(t, key) })
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return
	}
	started := time.Now()
	site := bound.Site
	if site == "" {
		site = callSite()
	}
	w.bounds = append(w.bounds, observedBound{
		Bound:         bound,
		started:       started,
		startedFailed: t.Failed(),
		site:          site,
	})
}

// MarkExplicit tells t's watch that an Until, Receive or Remaining failure will print its own
// diagnostics, so its cleanup must not add a second dump.
func MarkExplicit(t testing.TB) {
	if value, ok := watchers.Load(watcherKey(t)); ok {
		w := value.(*watcher)
		w.mu.Lock()
		w.explicit = true
		w.mu.Unlock()
	}
}

func (w *watcher) finish(t testing.TB, key any) {
	now := time.Now()
	w.mu.Lock()
	w.ended = true
	candidate, ok := w.latest(now)
	explicit := w.explicit
	w.mu.Unlock()
	// testing writes done without its lock after every cleanup. Watch holds w.mu while it asks
	// Failed and returns once ended is set, so no Watch call can ask Failed beside that write.
	failed := t.Failed()
	watchers.CompareAndDelete(key, w)
	if !failed || explicit || !ok {
		return
	}
	wait := Wait{
		What:           "a wait of its own until the end of " + candidate.String(),
		Began:          candidate.started,
		End:            candidate.End,
		EndDescription: candidate.EndDescription,
		Spent:          !candidate.started.Before(candidate.End),
		When:           "read at " + clockOf(now) + ", as the test ended, " + now.Sub(candidate.End).Round(time.Millisecond).String() + " after the end of " + candidate.String(),
	}
	diagnostics := Diagnostics(t, wait)
	Print(t, diagnostics+fmt.Sprintf("testwait: %s: the test ended failed at %s, %s after those stacks were read\n",
		t.Name(), clockOf(now), now.Sub(candidate.End).Round(time.Millisecond)))
}

// latest is the latest bound that ended and can explain this failure. A span is excluded where the
// test had already failed before it began, or the test failed later than spanGrace after it ended.
func (w *watcher) latest(now time.Time) (observedBound, bool) {
	var latest observedBound
	found := false
	for _, bound := range w.bounds {
		if now.Before(bound.End) {
			continue
		}
		if bound.Span && (bound.startedFailed || now.Sub(bound.End) > spanGrace) {
			continue
		}
		if !found || bound.End.After(latest.End) {
			latest, found = bound, true
		}
	}
	return latest, found
}

func watcherKey(t testing.TB) any {
	value := reflect.ValueOf(t)
	if value.IsValid() && value.Type().Comparable() {
		return t
	}
	return t.Name()
}

func clockOf(t time.Time) string { return t.UTC().Format("15:04:05.000Z") }

func callSite() string {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if !strings.HasPrefix(frame.Function, packagePrefix) || strings.HasSuffix(frame.File, "_test.go") {
			return filepath.Base(frame.File) + ":" + fmt.Sprint(frame.Line)
		}
		if !more {
			return "an unknown line"
		}
	}
}

var packagePrefix = reflect.TypeOf(watcher{}).PkgPath() + "."

