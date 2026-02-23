/*
Copyright 2026 The KubeVirt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kubevirt/kuevirt-io-latency-debug/internal/collector"
	"github.com/kubevirt/kuevirt-io-latency-debug/internal/crio"
)

func main() {
	var (
		crioSocket     = flag.String("crio-socket", "/run/crio/crio.sock", "Path to the CRI-O UNIX socket")
		port           = flag.Int("port", 9100, "TCP port to expose /metrics on")
		namespacesRaw  = flag.String("namespaces", "", "Comma-separated namespace list to monitor (empty = all)")
		labelFilterRaw = flag.String("label-filter", "", `Pod label selector, e.g. "environment=prod,tier=db"`)
		pollInterval   = flag.Duration("poll-interval", 30*time.Second, "How often to reconcile the running VMI set")
		qmpTimeout     = flag.Duration("qmp-timeout", 5*time.Second, "Per-call deadline for QMP operations (QueryBlockStats, block-latency-histogram-set)")
		// Bucket boundaries can be overridden for environments with different
		// latency profiles.  Values are in nanoseconds.
		boundariesRaw = flag.String("boundaries",
			"1000000,10000000,100000000,1000000000,10000000000,30000000000,60000000000",
			"Comma-separated histogram bucket boundaries in nanoseconds")
	)
	flag.Parse()

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		slog.Warn("NODE_NAME env var not set; node label will be empty. " +
			"Set it via the Downward API in the DaemonSet pod spec.")
	}

	namespaces := splitTrimmed(*namespacesRaw, ",")
	labelFilter := parseLabelFilter(*labelFilterRaw)
	boundaries, err := parseBoundaries(*boundariesRaw)
	if err != nil {
		slog.Error("Invalid --boundaries", "error", err)
		os.Exit(1)
	}

	crioClient, err := crio.NewClient(*crioSocket)
	if err != nil {
		slog.Error("Failed to connect to CRI-O", "socket", *crioSocket, "error", err)
		os.Exit(1)
	}

	col := collector.New(collector.Config{
		NodeName:     nodeName,
		Namespaces:   namespaces,
		LabelFilter:  labelFilter,
		CRIOClient:   crioClient,
		PollInterval: *pollInterval,
		QMPTimeout:   *qmpTimeout,
		Boundaries:   boundaries,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	col.Start(ctx)

	reg := prometheus.NewRegistry()
	reg.MustRegister(col)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		slog.Info("Shutting down")
		srv.Close()
	}()

	slog.Info("Starting metrics server", "addr", srv.Addr,
		"namespaces", namespaces, "poll_interval", *pollInterval)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server error", "error", err)
		os.Exit(1)
	}
}

func splitTrimmed(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, sep) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseLabelFilter parses "key1=val1,key2=val2" into a map.
func parseLabelFilter(raw string) map[string]string {
	m := make(map[string]string)
	for _, pair := range splitTrimmed(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return m
}

func parseBoundaries(raw string) ([]int64, error) {
	parts := splitTrimmed(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v, err := time.ParseDuration(p)
		if err == nil {
			// Accept human-readable durations like "1ms", "100ms", "1s".
			out = append(out, v.Nanoseconds())
			continue
		}
		// Fall back to raw integer nanoseconds.
		var n int64
		if _, err := fmt.Sscan(p, &n); err != nil {
			return nil, fmt.Errorf("invalid boundary %q (use nanoseconds or Go duration syntax)", p)
		}
		out = append(out, n)
	}
	return out, nil
}
