package main

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestShutdownTimeoutWithStreamInFlightIsNotAnError(t *testing.T) {
	started := make(chan struct{})
	handlerDone := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // a long stream that only ends when the connection is dropped
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started

	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	if err := shutdown(srv, 100*time.Millisecond, logger); err != nil {
		t.Fatalf("shutdown = %v, want nil", err)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "graceful shutdown timed out") {
		t.Fatalf("missing warn log:\n%s", logs.String())
	}

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request was not dropped by srv.Close")
	}
}

func TestShutdownIdleServerIsClean(t *testing.T) {
	srv := &http.Server{Handler: http.NotFoundHandler()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	logs := &syncBuffer{}
	if err := shutdown(srv, time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("shutdown = %v", err)
	}
	if strings.Contains(logs.String(), "timed out") {
		t.Fatalf("unexpected timeout log:\n%s", logs.String())
	}
}
