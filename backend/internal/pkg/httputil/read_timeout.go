package httputil

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// WithBodyReadTimeout bounds each socket read while receiving a body. It does
// not set a whole-request deadline or limit the duration of an SSE response.
func WithBodyReadTimeout(next http.Handler, timeout time.Duration) http.Handler {
	if timeout <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			deadlines := &bodyReadDeadlines{controller: http.NewResponseController(w)}
			r = r.WithContext(context.WithValue(r.Context(), bodyReadDeadlinesKey{}, deadlines))
			body := &bodyReadTimeout{ReadCloser: r.Body, deadlines: deadlines, timeout: timeout}
			r.Body = body
			if r.ProtoMajor == 1 {
				writer := &bodyDrainTimeoutWriter{ResponseWriter: w, body: body}
				defer func() {
					if !writer.hijacked.Load() {
						// net/http closes its original body after ServeHTTP returns,
						// bypassing our Read and Close methods.
						body.startDrain()
					}
				}()
				next.ServeHTTP(writer, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// BindRequestBodyReadDeadline binds an upload context to the request's body
// transport before any multipart read. Its deadline caps the existing per-read
// timeout; cancellation interrupts an already blocked Read. An unsupported
// transport returns http.ErrNotSupported, so callers must not silently continue.
//
// Call stop before returning from the handler. It waits for any cancellation
// callback, but deliberately keeps the absolute/cancellation deadline armed:
// HTTP/1 may still drain unread body bytes after the handler returns. This helper
// is for bounded uploads, not long-lived responses after a partially read body.
func BindRequestBodyReadDeadline(w http.ResponseWriter, r *http.Request, ctx context.Context) (stop func(), err error) {
	if ctx == nil {
		return nil, errors.New("request body deadline requires a context")
	}
	deadlines, _ := r.Context().Value(bodyReadDeadlinesKey{}).(*bodyReadDeadlines)
	if deadlines == nil {
		deadlines = &bodyReadDeadlines{controller: http.NewResponseController(w)}
		*r = *r.WithContext(context.WithValue(r.Context(), bodyReadDeadlinesKey{}, deadlines))
	}
	deadline, _ := ctx.Deadline()
	if ctx.Err() != nil {
		deadline = time.Now()
	}
	if err := deadlines.cap(deadline); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stopCallback := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = deadlines.cap(time.Now())
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stopCallback() {
				<-done
			}
			// stop can race cancellation before AfterFunc starts. Preserve its
			// fence even when we successfully prevented that callback.
			if ctx.Err() != nil {
				_ = deadlines.cap(time.Now())
			}
		})
	}, nil
}

type bodyReadDeadlinesKey struct{}

// All socket deadline changes share this lock. In particular a successful Read
// clearing its idle deadline cannot overwrite an earlier cancellation callback.
type bodyReadDeadlines struct {
	mu         sync.Mutex
	controller *http.ResponseController
	absolute   time.Time
	idle       time.Time
}

func (d *bodyReadDeadlines) cap(deadline time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !deadline.IsZero() && (d.absolute.IsZero() || deadline.Before(d.absolute)) {
		d.absolute = deadline
	}
	return d.apply()
}

func (d *bodyReadDeadlines) set(deadline time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.idle = deadline
	return d.apply()
}

func (d *bodyReadDeadlines) apply() error {
	deadline := d.idle
	if !d.absolute.IsZero() && (deadline.IsZero() || d.absolute.Before(deadline)) {
		deadline = d.absolute
	}
	return d.controller.SetReadDeadline(deadline)
}

type bodyReadTimeout struct {
	io.ReadCloser
	deadlines *bodyReadDeadlines
	timeout   time.Duration
	finished  atomic.Bool
	failed    atomic.Bool
}

func (r *bodyReadTimeout) Read(p []byte) (int, error) {
	err := r.deadlines.set(time.Now().Add(r.timeout))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	n, readErr := r.ReadCloser.Read(p)
	if errors.Is(readErr, io.EOF) {
		r.finished.Store(true)
	} else if readErr != nil {
		r.failed.Store(true)
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		_ = r.deadlines.set(time.Time{})
	}
	// Keep a failed read's deadline: net/http may drain an unread HTTP/1 body
	// after the handler returns. Clearing a timeout here could block that drain.
	return n, readErr
}

func (r *bodyReadTimeout) Close() error {
	armed := r.startDrain()
	err := r.ReadCloser.Close()
	if err == nil {
		r.finished.Store(true)
	} else {
		r.failed.Store(true)
	}
	r.finishDrain(armed)
	return err
}

func (r *bodyReadTimeout) startDrain() bool {
	if r.finished.Load() || r.failed.Load() {
		return false
	}
	return r.deadlines.set(time.Now().Add(r.timeout)) == nil
}

func (r *bodyReadTimeout) finishDrain(armed bool) {
	if armed && !r.failed.Load() {
		_ = r.deadlines.set(time.Time{})
	}
}

// HTTP/1 can drain an unread request body during its first response write or
// flush as well as after the handler returns. These reads use net/http's original
// body, so guard the response operations too. HTTP/2 has no such implicit drain.
type bodyDrainTimeoutWriter struct {
	http.ResponseWriter
	body       *bodyReadTimeout
	hijacked   atomic.Bool
	fullDuplex atomic.Bool
}

func (w *bodyDrainTimeoutWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *bodyDrainTimeoutWriter) startDrain() bool {
	return !w.fullDuplex.Load() && w.body.startDrain()
}

func (w *bodyDrainTimeoutWriter) Write(p []byte) (int, error) {
	armed := w.startDrain()
	defer w.body.finishDrain(armed)
	return w.ResponseWriter.Write(p)
}

func (w *bodyDrainTimeoutWriter) WriteString(s string) (int, error) {
	armed := w.startDrain()
	defer w.body.finishDrain(armed)
	return io.WriteString(w.ResponseWriter, s)
}

func (w *bodyDrainTimeoutWriter) WriteHeader(status int) {
	armed := w.startDrain()
	defer w.body.finishDrain(armed)
	w.ResponseWriter.WriteHeader(status)
}

func (w *bodyDrainTimeoutWriter) FlushError() error {
	armed := w.startDrain()
	defer w.body.finishDrain(armed)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *bodyDrainTimeoutWriter) Flush() { _ = w.FlushError() }

func (w *bodyDrainTimeoutWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked.Store(true)
		_ = conn.SetReadDeadline(time.Time{})
	}
	return conn, rw, err
}

func (w *bodyDrainTimeoutWriter) EnableFullDuplex() error {
	if err := http.NewResponseController(w.ResponseWriter).EnableFullDuplex(); err != nil {
		return err
	}
	w.fullDuplex.Store(true)
	return nil
}

func (w *bodyDrainTimeoutWriter) CloseNotify() <-chan bool {
	if notifier, ok := w.ResponseWriter.(http.CloseNotifier); ok { //nolint:staticcheck // Gin's ResponseWriter still requires legacy CloseNotify forwarding.
		return notifier.CloseNotify()
	}
	return make(chan bool)
}
