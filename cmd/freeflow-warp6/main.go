package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func parsePorts(raw string) ([]int, error) {
	seen := make(map[int]bool)
	var ports []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid relay port %q", part)
		}
		if !seen[n] {
			seen[n] = true
			ports = append(ports, n)
		}
	}
	if len(ports) == 0 {
		return nil, errors.New("no relay ports configured")
	}
	sort.Ints(ports)
	return ports, nil
}

type fileConfig struct {
	ListenBase int    `json:"listen_base"`
	RelayCount int    `json:"relay_count"`
	Control    int    `json:"control_port"`
	Bind       string `json:"bind"`
	StateFile  string `json:"state_file"`
}

func loadFileConfig(path string) (fileConfig, error) {
	var cfg fileConfig
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "", "JSON config file (config.example.json)")
	relayFlag := flag.String("relays", "18110", "comma-separated CONNECT ports; each port gets its own Size=1 WARP pool")
	bind := flag.String("bind", "0.0.0.0", "bind address for CONNECT listeners")
	control := flag.String("control", "127.0.0.1:18080", "control address serving /health and /rotate")
	stateDir := flag.String("state-dir", "", "directory for WARP registration state (empty = in-memory only)")
	flag.Parse()

	cfg, err := loadFileConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	visited := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if cfg.ListenBase > 0 && cfg.RelayCount > 0 && !visited["relays"] {
		parts := make([]string, 0, cfg.RelayCount)
		for i := 0; i < cfg.RelayCount; i++ {
			parts = append(parts, strconv.Itoa(cfg.ListenBase+i))
		}
		*relayFlag = strings.Join(parts, ",")
	}
	if cfg.Bind != "" && !visited["bind"] {
		*bind = cfg.Bind
	}
	if cfg.Control > 0 && !visited["control"] {
		*control = net.JoinHostPort(*bind, strconv.Itoa(cfg.Control))
	}
	if cfg.StateFile != "" && !visited["state-dir"] {
		*stateDir = filepath.Dir(cfg.StateFile)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ports, err := parsePorts(*relayFlag)
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg, err := newRegistry(ctx, ports, *stateDir, log)
	if err != nil {
		// NewWithOptions registers the first exit synchronously: most failures
		// here are WARP registration/network failures, not code errors.
		log.Error("WARP registration failed; no listeners started", "error", err)
		os.Exit(1)
	}
	defer reg.Close()

	mux := newControlHandler(reg)

	errCh := make(chan error, len(ports)+1)
	servers := make([]*http.Server, 0, len(ports)+1)
	controlSrv := &http.Server{Addr: *control, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	servers = append(servers, controlSrv)
	go func() { errCh <- controlSrv.ListenAndServe() }()
	log.Info("control listener started", "component", "control", "addr", *control)

	for _, rel := range reg.List() {
		srv := &http.Server{
			Addr:              net.JoinHostPort(*bind, strconv.Itoa(rel.Port)),
			Handler:           newConnectHandler(rel, log),
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, srv)
		go func(s *http.Server, port int) {
			errCh <- s.ListenAndServe()
			_ = port
		}(srv, rel.Port)
		log.Info("CONNECT listener started", "component", "relay", "port", rel.Port, "addr", srv.Addr)
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener stopped unexpectedly", "error", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	log.Info("server stopped")
}
