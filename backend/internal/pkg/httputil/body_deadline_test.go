package httputil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
	"time"
)

func TestRequestBodyAbsoluteDeadlineBoundsFirstMultipartBoundary(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(WithBodyReadTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		stop, err := BindRequestBodyReadDeadline(w, r, ctx)
		if err != nil {
			done <- err
			return
		}
		defer stop()
		reader, err := r.MultipartReader()
		if err == nil {
			_, err = reader.NextPart()
		}
		done <- err
	}), 5*time.Second))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\nContent-Type: multipart/form-data; boundary=files\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("want timeout at first boundary, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("absolute deadline did not interrupt first multipart boundary")
	}
}

func TestRequestBodyAbsoluteDeadlineBoundsContinuousDrip(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(WithBodyReadTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 150*time.Millisecond)
		defer cancel()
		stop, err := BindRequestBodyReadDeadline(w, r, ctx)
		if err == nil {
			defer stop()
			_, err = io.Copy(io.Discard, r.Body)
		}
		done <- err
	}), 100*time.Millisecond))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 1048576\r\n\r\nx")
	if err != nil {
		t.Fatal(err)
	}
	stopDrip := make(chan struct{})
	dripDone := make(chan struct{})
	go func() {
		defer close(dripDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopDrip:
				return
			case <-ticker.C:
				if _, err := io.WriteString(conn, "x"); err != nil {
					return
				}
			}
		}
	}()
	defer func() { close(stopDrip); conn.Close(); <-dripDone }()
	select {
	case err := <-done:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("want wall timeout during drip, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("body drip extended absolute deadline")
	}
}

func TestRequestBodyCancellationInterruptsRead(t *testing.T) {
	for _, idleWrapper := range []bool{false, true} {
		t.Run(fmt.Sprint("idle_wrapper=", idleWrapper), func(t *testing.T) {
			cancelReady := make(chan context.CancelFunc, 1)
			done := make(chan error, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				stop, err := BindRequestBodyReadDeadline(w, r, ctx)
				if err != nil {
					done <- err
					return
				}
				defer stop()
				if _, err = io.ReadFull(r.Body, make([]byte, 1)); err != nil {
					done <- err
					return
				}
				cancelReady <- cancel
				_, err = io.Copy(io.Discard, r.Body)
				done <- err
			})
			var h http.Handler = handler
			if idleWrapper {
				h = WithBodyReadTimeout(h, 5*time.Second)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, err = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\n\r\nx")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case cancel := <-cancelReady:
				cancel()
			case <-time.After(time.Second):
				t.Fatal("handler failed to start body read")
			}
			select {
			case err := <-done:
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("want transport interruption, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel retained blocked body reader")
			}
		})
	}
}

func TestRequestBodyAbsoluteDeadlineBoundsHTTP1ImplicitDrain(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(WithBodyReadTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		stop, err := BindRequestBodyReadDeadline(w, r, ctx)
		if err != nil {
			t.Error(err)
			close(done)
			return
		}
		defer stop()
		w.WriteHeader(http.StatusBadRequest)
		_ = http.NewResponseController(w).Flush()
		close(done)
	}), 5*time.Second))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\n\r\nx")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP/1 response drain extended absolute deadline")
	}
}

func TestRequestBodyDeadlineHTTP2KeepsOtherStreams(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ready := make(chan context.CancelFunc, 1)
			done := make(chan error, 1)
			srv := httptest.NewUnstartedServer(WithBodyReadTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/slow" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				ctx, cancel := context.WithCancel(r.Context())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(r.Context(), 300*time.Millisecond)
				}
				defer cancel()
				stop, err := BindRequestBodyReadDeadline(w, r, ctx)
				if err != nil {
					done <- err
					return
				}
				defer stop()
				ready <- cancel
				_, err = io.Copy(io.Discard, r.Body)
				done <- err
			}), 5*time.Second))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reader, writer := io.Pipe()
			slowConn := make(chan net.Conn, 1)
			traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { slowConn <- info.Conn }})
			slow, _ := http.NewRequestWithContext(traceCtx, http.MethodPost, srv.URL+"/slow", reader)
			clientDone := make(chan struct{})
			go func() {
				defer close(clientDone)
				resp, err := srv.Client().Do(slow)
				if err == nil {
					resp.Body.Close()
				}
			}()
			defer func() { cancel(); writer.Close(); reader.Close(); <-clientDone }()
			var interrupt context.CancelFunc
			select {
			case interrupt = <-ready:
			case <-ctx.Done():
				t.Fatal("slow stream did not reach reader")
			}
			var uploadConn net.Conn
			select {
			case uploadConn = <-slowConn:
			case <-ctx.Done():
				t.Fatal("missing slow connection")
			}
			checkFast := func() {
				t.Helper()
				var usedConn net.Conn
				fastCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { usedConn = info.Conn }})
				fast, _ := http.NewRequestWithContext(fastCtx, http.MethodGet, srv.URL+"/fast", nil)
				resp, err := srv.Client().Do(fast)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusNoContent || usedConn != uploadConn {
					t.Fatalf("other stream failed on same connection: proto=%s status=%d same_conn=%v", resp.Proto, resp.StatusCode, usedConn == uploadConn)
				}
			}
			checkFast()
			if mode == "cancel" {
				interrupt()
			}
			select {
			case err := <-done:
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("want interrupted stream, got %v", err)
				}
			case <-ctx.Done():
				t.Fatal("body deadline did not release HTTP/2 stream")
			}
			checkFast()
		})
	}
}

func TestRequestBodyAbsoluteDeadlineBoundsTrailingMultipartField(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(WithBodyReadTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		stop, err := BindRequestBodyReadDeadline(w, r, ctx)
		if err != nil {
			done <- err
			return
		}
		defer stop()
		reader, err := r.MultipartReader()
		if err == nil {
			var file io.Reader
			file, err = reader.NextPart()
			if err == nil {
				_, err = io.Copy(io.Discard, file)
			}
		}
		if err == nil {
			var metadata io.Reader
			metadata, err = reader.NextPart()
			if err == nil {
				_, err = io.Copy(io.Discard, metadata)
			}
		}
		done <- err
	}), 5*time.Second))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := "--files\r\nContent-Disposition: form-data; name=\"file\"; filename=\"sample.txt\"\r\n\r\nx\r\n--files\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nuser_data"
	_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\nContent-Type: multipart/form-data; boundary=files\r\n\r\n%s", body)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("want trailing field timeout, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("trailing metadata retained multipart handler")
	}
}
