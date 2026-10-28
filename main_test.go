package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A stop signal cancels running scrapes: they answer with what they have and
// serve returns without waiting for their scrape timeout.
func TestServeCancelsRunningRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			_, _ = io.WriteString(w, "partial")
		case <-time.After(time.Minute):
			_, _ = io.WriteString(w, "complete")
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln) }()

	answer := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			answer <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		answer <- string(body)
	}()
	<-started
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve waits for the running request")
	}
	if got := <-answer; got != "partial" {
		t.Errorf("answer %q, want the partial answer", got)
	}
}
