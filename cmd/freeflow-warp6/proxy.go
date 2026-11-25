package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chipmax/freeflow-warp6/internal/warp"
)

// The proxy only reaches these hosts; subdomains are included (e.g.
// www.opencode.ai) but nothing else can be dialed through the tunnel.
var allowedHosts = []string{
	"opencode.ai",
	"api.kilo.ai",
	"oai.endpoints.kepler.ai.cloud.ovh.net",
}

func hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, allowed := range allowedHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

// splitAuthority accepts "host", "host:443" and "[ipv6]:443". A missing port
// defaults to 443, the only port these HTTPS endpoints normally use.
func splitAuthority(authority string) (string, string, error) {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return "", "", errors.New("empty CONNECT authority")
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = authority, "443"
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", "", errors.New("empty CONNECT host")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("invalid CONNECT port %q", port)
	}
	return host, port, nil
}

func newConnectHandler(rel *relay, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			serveAbsoluteForm(w, r, rel, log)
			return
		}
		authority := r.Host
		if authority == "" {
			authority = r.URL.Host
		}
		host, port, err := splitAuthority(authority)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !hostAllowed(host) {
			log.Warn("CONNECT host rejected", "component", "proxy", "relay", rel.Port, "host", host)
			http.Error(w, "host not allowlisted: "+host, http.StatusForbidden)
			return
		}

		le, err := rel.Pool.Acquire(r.Context())
		if err != nil {
			http.Error(w, "no WARP egress available: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer le.Release()

		network := le.DialNetwork()
		if network == "" {
			network = "tcp6"
		}
		target := net.JoinHostPort(host, port)
		upstream, err := le.DialContext(r.Context(), netNetwork(network), target)
		if err != nil {
			http.Error(w, "WARP dial failed: "+err.Error(), http.StatusBadGateway)
			return
		}

		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		client, bufrw, err := hj.Hijack()
		if err != nil {
			_ = upstream.Close()
			log.Error("CONNECT hijack failed", "component", "proxy", "relay", rel.Port, "error", err)
			return
		}
		_ = client.SetDeadline(time.Time{})
		if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			_ = client.Close()
			_ = upstream.Close()
			return
		}
		if err := bufrw.Flush(); err != nil {
			_ = client.Close()
			_ = upstream.Close()
			return
		}
		log.Info("tunnel opened", "component", "proxy", "relay", rel.Port,
			"target", target, "network", network, "egress", le.EgressKey())
		splice(client, upstream, bufrw)
	})
}

// serveAbsoluteForm forwards normal proxy requests (absolute-form URI) through
// the same WARP lease. OVH's lane uses this shape instead of CONNECT; the old
// "CONNECT only" response was rejected by our own relay, not by OVH.
func serveAbsoluteForm(w http.ResponseWriter, r *http.Request, rel *relay, log *slog.Logger) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "absolute-form URI required for non-CONNECT requests", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	if !hostAllowed(host) {
		log.Warn("proxy host rejected", "component", "proxy", "relay", rel.Port, "host", host)
		http.Error(w, "host not allowlisted: "+host, http.StatusForbidden)
		return
	}
	le, err := rel.Pool.Acquire(r.Context())
	if err != nil {
		http.Error(w, "no WARP egress available: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer le.Release()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			network := le.DialNetwork()
			if network == "" {
				network = "tcp6"
			}
			return le.DialContext(ctx, netNetwork(network), addr)
		},
		DisableCompression: true,
	}
	defer transport.CloseIdleConnections()

	req := r.Clone(r.Context())
	req.RequestURI = ""
	req.Host = r.URL.Host
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		log.Warn("absolute-form upstream failed", "component", "proxy", "relay", rel.Port, "error", err)
		http.Error(w, "WARP upstream failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// netNetwork maps the WARP lease's explicit family to the TCP network name
// used by DialContext. DialNetwork already returns tcp4/tcp6 for dual stacks.
func netNetwork(network string) string {
	switch strings.ToLower(network) {
	case "tcp4", "tcp6":
		return strings.ToLower(network)
	default:
		return "tcp6"
	}
}

func splice(client, upstream net.Conn, pre io.Reader) {
	defer client.Close()
	defer upstream.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var src io.Reader = client
		if pre != nil {
			// bufrw may already hold the first TLS bytes from the client.
			src = io.MultiReader(pre, client)
		}
		_, _ = io.Copy(upstream, src)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// newControlHandler is the HTTP mux for /health and /rotate.
func newControlHandler(reg *registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(reg))
	mux.HandleFunc("/rotate", rotateHandler(reg))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"endpoints": []string{
				"GET /health",
				"GET /rotate?relay=<port>[&prefix=<egress-/64>]",
				"CONNECT <allowlisted-host>:443 on each relay port",
			},
		})
	})
	return mux
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type relayHealth struct {
	Port   int              `json:"port"`
	Active int              `json:"active"`
	States []warp.ExitState `json:"states"`
}

type healthResponse struct {
	OK     bool          `json:"ok"`
	Relays []relayHealth `json:"relays"`
}

func healthHandler(reg *registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		resp := healthResponse{OK: true, Relays: make([]relayHealth, 0)}
		for _, rel := range reg.List() {
			states := rel.Pool.States()
			if states == nil {
				states = []warp.ExitState{}
			}
			active := 0
			for _, st := range states {
				if !st.Cooling {
					active++
				}
			}
			if active == 0 {
				resp.OK = false
			}
			resp.Relays = append(resp.Relays, relayHealth{Port: rel.Port, Active: active, States: states})
		}
		if len(resp.Relays) == 0 {
			resp.OK = false
		}
		status := http.StatusOK
		if !resp.OK {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, resp)
	}
}

type rotateResponse struct {
	Relay   int              `json:"relay"`
	Rotated int              `json:"rotated"`
	Prefix  string           `json:"prefix,omitempty"`
	States  []warp.ExitState `json:"states"`
}

func rotateHandler(reg *registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rotate" {
			http.NotFound(w, r)
			return
		}
		port, err := strconv.Atoi(r.URL.Query().Get("relay"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "relay must be a listener port number"})
			return
		}
		rel, ok := reg.Get(port)
		if !ok {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("unknown relay %d", port)})
			return
		}
		prefix := r.URL.Query().Get("prefix")
		rotated := rel.Rotate(prefix)
		states := rel.Pool.States()
		if states == nil {
			states = []warp.ExitState{}
		}
		writeJSON(w, http.StatusOK, rotateResponse{Relay: port, Rotated: rotated, Prefix: prefix, States: states})
	}
}
