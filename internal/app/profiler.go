package app

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// profilerReadHeaderTimeout bounds how long a profiling client may take to
// send request headers. Profiling responses are slow by design (a CPU profile
// streams for its full duration), so only the header phase is bounded.
const profilerReadHeaderTimeout = 5 * time.Second

// newProfilerServer binds the opt-in pprof listener.
//
// Profiling is deliberately not a route on the public Gin router: the listener
// is a separate http.Server that only exists when PPROF_ADDR is set, and the
// address is rejected unless it is a loopback address. That keeps pprof off
// the production-facing port and off every interface by default.
//
// The listener is bound here (during initialization) rather than in Start so a
// bad or already-taken address fails startup instead of leaving a running
// service without the diagnostics the operator asked for.
func newProfilerServer(addr string, log *slog.Logger) (*http.Server, net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid PPROF_ADDR %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		// An empty host (":6060") would bind every interface, which is the
		// exact exposure this listener must avoid.
		return nil, nil, fmt.Errorf("PPROF_ADDR %q must bind a loopback address such as 127.0.0.1:6060", addr)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on PPROF_ADDR %q: %w", addr, err)
	}

	// net/http/pprof's handlers are registered explicitly on a private mux
	// rather than through the package-level DefaultServeMux, so importing this
	// package cannot expose them anywhere else.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: profilerReadHeaderTimeout,
	}
	if log != nil {
		log.Info("pprof listener enabled", slog.String("addr", listener.Addr().String()))
	}
	return srv, listener, nil
}

// isLoopbackHost reports whether host names the local machine. "localhost" is
// accepted by name; everything else must parse as a loopback IP.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
