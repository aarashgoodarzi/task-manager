package app

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewProfilerServerRejectsNonLoopbackAddresses(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:6060", ":6060", "192.0.2.10:6060", "[::]:6060", "example.com:6060"} {
		if _, _, err := newProfilerServer(addr, discardLog()); err == nil {
			t.Errorf("newProfilerServer(%q) succeeded, want a loopback-only rejection", addr)
		}
	}
}

func TestNewProfilerServerRejectsMalformedAddress(t *testing.T) {
	if _, _, err := newProfilerServer("127.0.0.1", discardLog()); err == nil {
		t.Fatal("newProfilerServer without a port succeeded, want an error")
	}
}

func TestProfilerServesPprofOnBoundListenerOnly(t *testing.T) {
	srv, listener, err := newProfilerServer("127.0.0.1:0", discardLog())
	if err != nil {
		t.Fatalf("newProfilerServer: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})

	addr := listener.Addr().String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("listener address %q: %v", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("listener bound %q, want a loopback address", addr)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Types of profiles available") {
		t.Fatalf("pprof index did not render: %s", body)
	}

	// The named-profile route is what the README's optional heap command uses;
	// it is served by pprof.Index through the same private mux.
	heap, err := client.Get("http://" + addr + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("GET /debug/pprof/heap: %v", err)
	}
	defer heap.Body.Close()
	if _, err := io.Copy(io.Discard, heap.Body); err != nil {
		t.Fatalf("read heap profile: %v", err)
	}
	if heap.StatusCode != http.StatusOK {
		t.Fatalf("heap status = %d, want 200", heap.StatusCode)
	}
}
