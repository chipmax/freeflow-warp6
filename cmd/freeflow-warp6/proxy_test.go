package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/chipmax/freeflow-warp6/internal/warp"
)

// fakeLease/fakePool exercise the proxy without any WARP registration.
type fakeLease struct{ conn net.Conn }

func (f *fakeLease) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return f.conn, nil
}
func (f *fakeLease) Release()            {}
func (f *fakeLease) EgressKey() string   { return "2606:4700:d0::/64" }
func (f *fakeLease) DialNetwork() string { return "tcp6" }

type fakePool struct {
	mu      sync.Mutex
	states  []warp.ExitState
	rotated []string
	closed  bool
}

func (p *fakePool) Acquire(_ context.Context) (lease, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_, _ = io.Copy(server, server) // echo anything the client sends
	}()
	return &fakeLease{conn: client}, nil
}
func (p *fakePool) States() []warp.ExitState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]warp.ExitState(nil), p.states...)
}
func (p *fakePool) Rotate(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rotated = append(p.rotated, prefix)
	return 1
}
func (p *fakePool) Close() { p.mu.Lock(); p.closed = true; p.mu.Unlock() }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHostAllowed(t *testing.T) {
	allowed := []string{
		"opencode.ai", "www.opencode.ai", "API.KILO.AI",
		"oai.endpoints.kepler.ai.cloud.ovh.net",
	}
	for _, host := range allowed {
		if !hostAllowed(host) {
			t.Errorf("expected %q to be allowed", host)
		}
	}
	for _, host := range []string{"example.com", "evilopencode.ai", "opencode.ai.evil.com", ""} {
		if hostAllowed(host) {
			t.Errorf("expected %q to be rejected", host)
		}
	}
}

func TestSplitAuthority(t *testing.T) {
	for _, tc := range []struct{ in, host, port string }{
		{"opencode.ai", "opencode.ai", "443"},
		{"api.kilo.ai:443", "api.kilo.ai", "443"},
		{"[2606:4700::1]:8443", "2606:4700::1", "8443"},
	} {
		host, port, err := splitAuthority(tc.in)
		if err != nil || host != tc.host || port != tc.port {
			t.Errorf("splitAuthority(%q) = %q, %q, %v", tc.in, host, port, err)
		}
	}
	if _, _, err := splitAuthority("opencode.ai:notaport"); err == nil {
		t.Error("expected invalid port error")
	}
}

func TestConnectTunnel(t *testing.T) {
	rel := &relay{Port: 18110, Pool: &fakePool{}}
	srv := httptest.NewServer(newConnectHandler(rel, testLogger()))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "CONNECT opencode.ai:443 HTTP/1.1\r\nHost: opencode.ai:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "200") {
		t.Fatalf("expected 200 CONNECT response, got %q", line)
	}
	if _, err := br.ReadString('\n'); err != nil { // blank line
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
}

func TestConnectRejectsNonAllowlistedHost(t *testing.T) {
	rel := &relay{Port: 18110, Pool: &fakePool{}}
	srv := httptest.NewServer(newConnectHandler(rel, testLogger()))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	c, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "403") {
		t.Fatalf("expected 403, got %q", line)
	}
}

func TestHealthAndRotate(t *testing.T) {
	fp := &fakePool{states: []warp.ExitState{
		{ID: "dev-1", Egress: "2606:4700:d0::1", EgressKey: "2606:4700:d0::/64"},
	}}
	reg := &registry{relays: map[int]*relay{18110: {Port: 18110, Pool: fp}}}

	rec := httptest.NewRecorder()
	healthHandler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "2606:4700:d0::1") {
		t.Fatalf("health = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	rotateHandler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rotate?relay=18110", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rotated":1`) {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body.String())
	}
	fp.mu.Lock()
	rotated := append([]string(nil), fp.rotated...)
	fp.mu.Unlock()
	if len(rotated) != 1 || rotated[0] != "2606:4700:d0::/64" {
		t.Fatalf("rotated prefixes = %v", rotated)
	}

	rec = httptest.NewRecorder()
	rotateHandler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rotate?relay=999", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown relay = %d", rec.Code)
	}
}

func TestControlHTTPOverNetwork(t *testing.T) {
	fp := &fakePool{states: []warp.ExitState{
		{ID: "dev-1", Egress: "2606:4700:d0::1", EgressKey: "2606:4700:d0::/64"},
	}}
	reg := &registry{relays: map[int]*relay{18110: {Port: 18110, Pool: fp}}}
	srv := httptest.NewServer(newControlHandler(reg))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "2606:4700:d0::1") {
		t.Fatalf("GET /health = %d %s", resp.StatusCode, body)
	}

	resp, err = http.Get(srv.URL + "/rotate?relay=18110")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"rotated":1`) {
		t.Fatalf("GET /rotate = %d %s", resp.StatusCode, body)
	}
}

type dialLease struct{ addr string }

func (d *dialLease) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.addr)
}
func (d *dialLease) Release()            {}
func (d *dialLease) EgressKey() string   { return "2606:4700:d0::/64" }
func (d *dialLease) DialNetwork() string { return "tcp6" }

type dialPool struct {
	fakePool
	addr string
}

func (p *dialPool) Acquire(context.Context) (lease, error) {
	return &dialLease{addr: p.addr}, nil
}

func TestAbsoluteFormProxyForwardsOvh(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "pong:"+r.URL.Path)
	}))
	defer up.Close()
	proxy := httptest.NewServer(newConnectHandler(
		&relay{Port: 18110, Pool: &dialPool{addr: up.Listener.Addr().String()}}, testLogger()))
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	req, err := http.NewRequest(http.MethodPost,
		"http://oai.endpoints.kepler.ai.cloud.ovh.net/v1/chat", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "pong:/v1/chat" {
		t.Fatalf("absolute-form proxy = %d %q", resp.StatusCode, body)
	}
}
