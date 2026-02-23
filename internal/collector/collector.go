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

// Package collector implements a Prometheus Collector that queries KubeVirt
// VMI block-device latency histograms via QMP on the local node.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kubevirt/kuevirt-io-latency-debug/internal/crio"
	"github.com/kubevirt/kuevirt-io-latency-debug/internal/qmp"
)

// Config holds all tunable parameters for the Collector.
type Config struct {
	NodeName     string
	Namespaces   []string          // empty → all namespaces
	LabelFilter  map[string]string // matched against pod labels via CRI
	CRIOClient   *crio.Client
	PollInterval time.Duration
	// QMPTimeout is the per-call deadline for QueryBlockStats and
	// SetHistogramBoundaries.  Defaults to 5s if zero.
	QMPTimeout time.Duration
	// Bucket boundaries in nanoseconds, passed to block-latency-histogram-set.
	Boundaries []int64
}

// domain represents a single running VMI with an open QMP connection.
type domain struct {
	mu          sync.Mutex // protects qmpClient and closed
	closed      bool
	containerID string
	vmiName     string
	namespace   string
	pid         int
	qmpClient   *qmp.Client
	// tracks devices for which block-latency-histogram-set has been called;
	// keyed by the device field from query-blockstats.
	armed map[string]bool
}

// close terminates the QMP connection idempotently.
func (d *domain) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.qmpClient.Close()
	}
}

// Collector implements prometheus.Collector.
type Collector struct {
	cfg Config

	mu      sync.RWMutex
	domains map[string]*domain // containerID → domain

	// Metric descriptors.  The node label is constant per DaemonSet pod.
	bucketDesc *prometheus.Desc
	countDesc  *prometheus.Desc
	sumDesc    *prometheus.Desc
}

// New creates a Collector.  Call Start to begin background reconciliation.
func New(cfg Config) *Collector {
	if cfg.QMPTimeout == 0 {
		cfg.QMPTimeout = 5 * time.Second
	}
	constLabels := prometheus.Labels{"node": cfg.NodeName}
	varLabels := []string{"namespace", "vmi", "drive", "operation"}

	return &Collector{
		cfg:     cfg,
		domains: make(map[string]*domain),
		bucketDesc: prometheus.NewDesc(
			"kubevirt_vmi_storage_latency_ns_bucket",
			"Cumulative count of VMI block-device I/O operations with latency ≤ le (nanoseconds).",
			append(varLabels, "le"),
			constLabels,
		),
		countDesc: prometheus.NewDesc(
			"kubevirt_vmi_storage_latency_ns_count",
			"Total number of VMI block-device I/O operations observed.",
			varLabels,
			constLabels,
		),
		sumDesc: prometheus.NewDesc(
			"kubevirt_vmi_storage_latency_ns_sum",
			"Total cumulative VMI block-device I/O latency in nanoseconds.",
			varLabels,
			constLabels,
		),
	}
}

// Start launches the background loop that keeps the active domain set in sync
// with what CRI-O reports.
func (c *Collector) Start(ctx context.Context) {
	go c.reconcileLoop(ctx)
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bucketDesc
	ch <- c.countDesc
	ch <- c.sumDesc
}

// Collect implements prometheus.Collector.
// It snapshots the current domain set and queries each VMI concurrently.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	doms := make([]*domain, 0, len(c.domains))
	for _, d := range c.domains {
		doms = append(doms, d)
	}
	c.mu.RUnlock()

	var wg sync.WaitGroup
	for _, d := range doms {
		d := d
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.collectDomain(d, ch); err != nil {
				slog.Warn("collect failed",
					"vmi", d.vmiName,
					"namespace", d.namespace,
					"error", err)
			}
		}()
	}
	wg.Wait()
}

func (c *Collector) collectDomain(d *domain, ch chan<- prometheus.Metric) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.QMPTimeout)
	defer cancel()

	stats, err := d.qmpClient.QueryBlockStats(ctx)
	if err != nil {
		return fmt.Errorf("query-blockstats: %w", err)
	}

	// Arm histograms for any new or previously-failed devices (idempotent).
	c.armHistograms(ctx, d, stats)

	for _, bs := range stats {
		alias, ok := extractDiskAlias(bs.Device, bs.QDev)
		if !ok {
			continue // firmware flash, CDROM, internal QEMU nodes, etc.
		}
		c.emitOperation(ch, d, alias, "read",
			bs.Stats.RdOperations, bs.Stats.RdTotalTimeNs, bs.Stats.ReadHistogram())
		c.emitOperation(ch, d, alias, "write",
			bs.Stats.WrOperations, bs.Stats.WrTotalTimeNs, bs.Stats.WriteHistogram())
		c.emitOperation(ch, d, alias, "flush",
			bs.Stats.FlushOperations, bs.Stats.FlushTotalTimeNs, bs.Stats.FlushHistogram())
	}
	return nil
}

func (c *Collector) emitOperation(
	ch chan<- prometheus.Metric,
	d *domain,
	drive, operation string,
	count, sum int64,
	hist *qmp.LatencyHistogram,
) {
	lv := []string{d.namespace, d.vmiName, drive, operation}

	if hist != nil && len(hist.Bins) > 0 {
		cumulative := hist.CumulativeBins()
		// The histogram accumulates only from when block-latency-histogram-set
		// was called, whereas RdOperations/WrOperations/FlushOperations count
		// from QEMU start.  Using the raw operation counters as _count would
		// make +Inf < count, violating the Prometheus histogram invariant and
		// breaking histogram_quantile.  Use the histogram total instead so
		// that _count == +Inf at all times.
		histTotal := cumulative[len(cumulative)-1]
		ch <- prometheus.MustNewConstMetric(c.countDesc, prometheus.CounterValue, float64(histTotal), lv...)
		ch <- prometheus.MustNewConstMetric(c.sumDesc, prometheus.CounterValue, float64(sum), lv...)

		// Emit one bucket per boundary; QEMU gives N boundaries and N+1 bins.
		// cumulative[i] covers observations ≤ hist.Boundaries[i].
		for i, boundary := range hist.Boundaries {
			if i >= len(cumulative) {
				break
			}
			ch <- prometheus.MustNewConstMetric(
				c.bucketDesc, prometheus.CounterValue, float64(cumulative[i]),
				append(lv, strconv.FormatInt(boundary, 10))...,
			)
		}
		// +Inf bucket must equal _count.
		ch <- prometheus.MustNewConstMetric(
			c.bucketDesc, prometheus.CounterValue, float64(histTotal),
			append(lv, "+Inf")...,
		)
		return
	}

	// No histogram yet (arming pending or not yet reflected in this scrape):
	// emit raw counters only; no bucket metrics until arming succeeds.
	ch <- prometheus.MustNewConstMetric(c.countDesc, prometheus.CounterValue, float64(count), lv...)
	ch <- prometheus.MustNewConstMetric(c.sumDesc, prometheus.CounterValue, float64(sum), lv...)
}

// --- reconciliation loop ---

func (c *Collector) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	c.reconcile(ctx) // run immediately on startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcile(ctx)
		}
	}
}

func (c *Collector) reconcile(ctx context.Context) {
	containers, err := c.cfg.CRIOClient.ListComputeContainers(
		ctx, c.cfg.Namespaces, c.cfg.LabelFilter,
	)
	if err != nil {
		slog.Error("CRI-O ListContainers failed", "error", err)
		return
	}

	current := make(map[string]crio.ContainerMeta, len(containers))
	for _, cm := range containers {
		current[cm.ContainerID] = cm
	}

	// --- remove domains that are no longer running ---
	c.mu.Lock()
	for id, d := range c.domains {
		if _, ok := current[id]; !ok {
			slog.Info("VMI left node, closing QMP connection",
				"vmi", d.vmiName, "namespace", d.namespace)
			d.close()
			delete(c.domains, id)
		}
	}
	// Snapshot which IDs are already connected so we don't hold the lock
	// during the slow QMP dial + histogram-set below.
	existing := make(map[string]bool, len(c.domains))
	for id := range c.domains {
		existing[id] = true
	}
	c.mu.Unlock()

	// --- connect to new domains (no lock held) ---
	for id, cm := range current {
		if existing[id] {
			continue
		}

		d, err := c.connectDomain(cm)
		if err != nil {
			slog.Warn("Could not connect to VMI",
				"vmi", cm.VMIName, "namespace", cm.Namespace, "error", err)
			continue
		}

		c.mu.Lock()
		// Guard against a concurrent reconcile that already inserted this id.
		if _, dup := c.domains[id]; !dup {
			c.domains[id] = d
			slog.Info("Connected to VMI",
				"vmi", cm.VMIName, "namespace", cm.Namespace, "pid", cm.PID)
		} else {
			d.close()
		}
		c.mu.Unlock()
	}
}

// connectDomain connects to virtqemud and attempts to arm latency histograms
// for all KubeVirt disks.  If the initial blockstats fetch fails, arming is
// deferred to the first collectDomain call.
func (c *Collector) connectDomain(cm crio.ContainerMeta) (*domain, error) {
	sockPath, err := findVirtqemudSocket(cm.PID)
	if err != nil {
		return nil, fmt.Errorf("locating virtqemud socket: %w", err)
	}

	// libvirt domain names follow the pattern <namespace>_<vmiName>.
	libvirtDomainName := cm.Namespace + "_" + cm.VMIName

	qmpClient, err := qmp.DialViaLibvirt(sockPath, libvirtDomainName)
	if err != nil {
		return nil, fmt.Errorf("libvirt dial %s domain %s: %w", sockPath, libvirtDomainName, err)
	}

	d := &domain{
		containerID: cm.ContainerID,
		vmiName:     cm.VMIName,
		namespace:   cm.Namespace,
		pid:         cm.PID,
		qmpClient:   qmpClient,
		armed:       make(map[string]bool),
	}

	// Best-effort: arm histograms immediately if possible.  collectDomain
	// calls armHistograms on every scrape (idempotent via d.armed), so any
	// devices that fail here — or disks hot-plugged later — are picked up
	// automatically on the next scrape.
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.QMPTimeout)
	defer cancel()

	stats, err := d.qmpClient.QueryBlockStats(ctx)
	if err != nil {
		slog.Warn("Initial blockstats fetch failed; histograms will be armed on first scrape",
			"vmi", cm.VMIName, "error", err)
	} else {
		c.armHistograms(ctx, d, stats)
	}
	return d, nil
}

// armHistograms calls block-latency-histogram-set for every KubeVirt disk that
// has not yet been armed.  stats must come from a QueryBlockStats call made
// by the caller.  The method is idempotent: already-armed devices (tracked in
// d.armed) are skipped, so it is safe to call on every scrape and handles
// hot-plugged disks automatically.
func (c *Collector) armHistograms(ctx context.Context, d *domain, stats qmp.BlockStatsList) {
	for _, bs := range stats {
		if _, ok := extractDiskAlias(bs.Device, bs.QDev); !ok {
			continue
		}
		// In modern KubeVirt the "device" field is always empty; use the
		// qdev path as the QMP id instead (QEMU accepts the qdev path as
		// the id parameter for block-latency-histogram-set).
		id := bs.Device
		if id == "" {
			id = bs.QDev
		}
		if id == "" {
			continue
		}
		if d.armed[id] {
			continue
		}
		if err := d.qmpClient.SetHistogramBoundaries(ctx, id, c.cfg.Boundaries); err != nil {
			slog.Warn("block-latency-histogram-set failed",
				"device", id, "vmi", d.vmiName, "error", err)
			continue
		}
		d.armed[id] = true
		slog.Info("Histogram armed", "device", id, "vmi", d.vmiName)
	}
}

// --- helpers ---

// findVirtqemudSocket returns the path to the virtqemud Unix socket for the
// container whose init process has the given host PID.
//
// In modern KubeVirt, QEMU is managed by virtqemud running inside the
// virt-launcher pod.  External QMP access must go through virtqemud via the
// libvirt remote protocol; connecting to QEMU's monitor socket directly is not
// possible because virtqemud holds the sole QMP connection.
//
// The socket is reachable via /proc/<pid>/root without nsenter because
// /run/libvirt is on the container's own tmpfs and the inode is accessible
// from the host mount namespace when the process is privileged.
func findVirtqemudSocket(pid int) (string, error) {
	path := fmt.Sprintf("/proc/%d/root/run/libvirt/virtqemud-sock", pid)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("virtqemud socket not found at %s: %w", path, err)
	}
	return path, nil
}

// extractDiskAlias derives a human-readable disk alias from QEMU device fields.
// Returns ("", false) for non-KubeVirt devices (firmware flash, CDROM, …).
//
// KubeVirt names all user-defined disks with the "ua-" (user-alias) prefix:
//
//   - qdev: /machine/peripheral/ua-<alias>/<backend>  → alias = parts[3][3:]
//   - device: drive-ua-<alias>  or  ua-<alias>        → alias = TrimPrefix(…)
//
// The qdev path is preferred because the alias component is unambiguously
// delimited by '/' on both sides, making it safe for aliases that contain
// hyphens (e.g. "data-disk" → "ua-data-disk" → parts[3] = "ua-data-disk").
func extractDiskAlias(device, qdev string) (string, bool) {
	if parts := strings.Split(qdev, "/"); len(parts) >= 5 {
		// ["", "machine", "peripheral", "ua-<alias>", "<backend>"]
		if component := parts[3]; strings.HasPrefix(component, "ua-") {
			return strings.TrimPrefix(component, "ua-"), true
		}
	}
	for _, prefix := range []string{"drive-ua-", "ua-"} {
		if strings.HasPrefix(device, prefix) {
			return strings.TrimPrefix(device, prefix), true
		}
	}
	return "", false
}
