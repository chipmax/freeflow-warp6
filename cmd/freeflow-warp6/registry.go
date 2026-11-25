package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sort"
	"sync"

	"github.com/chipmax/freeflow-warp6/internal/warp"
)

// lease is the subset of *warp.Lease the proxy uses. Keeping it as an
// interface lets the CONNECT tests run without registering Cloudflare devices.
type lease interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	Release()
	EgressKey() string
	DialNetwork() string
}

// pool is the subset of *warp.Pool the proxy uses.
type pool interface {
	Acquire(ctx context.Context) (lease, error)
	States() []warp.ExitState
	Rotate(prefix string) int
	Close()
}

type warpPool struct{ p *warp.Pool }

func (w *warpPool) Acquire(ctx context.Context) (lease, error) { return w.p.Acquire(ctx) }
func (w *warpPool) States() []warp.ExitState                   { return w.p.States() }
func (w *warpPool) Rotate(prefix string) int                   { return w.p.Rotate(prefix) }
func (w *warpPool) Close()                                     { w.p.Close() }

// relay ties one CONNECT listener to exactly one single-exit WARP pool, so
// every listening port owns one independent egress/relay.
type relay struct {
	Port int
	Pool pool
}

// Rotate swaps this relay's exit. With no explicit prefix it rotates every
// active /64 reported by the pool; with a prefix it rotates only that one.
func (r *relay) Rotate(prefix string) int {
	if prefix != "" {
		return r.Pool.Rotate(prefix)
	}
	total := 0
	for _, st := range r.Pool.States() {
		if st.Cooling || st.EgressKey == "" {
			continue
		}
		total += r.Pool.Rotate(st.EgressKey)
	}
	return total
}

type registry struct {
	mu     sync.RWMutex
	relays map[int]*relay
}

// newRegistry builds one Size=1 pool per relay port. Pool construction does a
// synchronous WARP registration, so a failure here means no listener starts.
func newRegistry(ctx context.Context, ports []int, stateDir string, log *slog.Logger) (*registry, error) {
	reg := &registry{relays: make(map[int]*relay, len(ports))}
	for _, port := range ports {
		statePath := ""
		if stateDir != "" {
			statePath = filepath.Join(stateDir, fmt.Sprintf("warp-%d.json", port))
		}
		p, err := warp.NewWithOptions(ctx, log, warp.Options{
			Size:              1,
			Preload:           1,
			Stack:             warp.StackDual,
			RequireIPv6Egress: true,
			StatePath:         statePath,
		})
		if err != nil {
			reg.Close()
			return nil, fmt.Errorf("relay %d: create WARP pool: %w", port, err)
		}
		reg.relays[port] = &relay{Port: port, Pool: &warpPool{p: p}}
		log.Info("WARP pool ready", "component", "relay", "port", port,
			"size", 1, "stack", string(warp.StackDual), "state_path", statePath)
	}
	return reg, nil
}

func (reg *registry) Get(port int) (*relay, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	r, ok := reg.relays[port]
	return r, ok
}

func (reg *registry) List() []*relay {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	out := make([]*relay, 0, len(reg.relays))
	for _, r := range reg.relays {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func (reg *registry) Close() {
	for _, r := range reg.List() {
		if r.Pool != nil {
			r.Pool.Close()
		}
	}
}
