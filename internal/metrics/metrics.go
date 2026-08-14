// Package metrics holds the Prometheus registry and every metric
// definition.
package metrics

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/example/process_exporter/internal/aggregate"
)

// groupLabels is the label set on every process-group metric. Exactly
// two labels, and neither is a PID.
var groupLabels = []string{"name", "user"}

var (
	descNumProcs = prometheus.NewDesc("process_group_num_procs",
		"Live processes in the group.", groupLabels, nil)
	descNumThreads = prometheus.NewDesc("process_group_num_threads",
		"Total threads across the group.", groupLabels, nil)
	descRSS = prometheus.NewDesc("process_group_memory_rss_bytes",
		"Resident set size summed across the group. Shared pages are "+
			"counted once per member, so the sum exceeds the true footprint.",
		groupLabels, nil)
	descVSize = prometheus.NewDesc("process_group_memory_vsize_bytes",
		"Virtual size summed across the group.", groupLabels, nil)
	descShared = prometheus.NewDesc("process_group_memory_shared_bytes",
		"Resident shared pages summed across the group.", groupLabels, nil)
	descData = prometheus.NewDesc("process_group_memory_data_bytes",
		"Private writable pages summed across the group. A useful leak "+
			"indicator, but not an approximation of PSS: it excludes text "+
			"and shared segments.", groupLabels, nil)
	descPSS = prometheus.NewDesc("process_group_memory_pss_bytes",
		"Proportional set size summed across the group. Each shared page "+
			"is divided by the number of processes sharing it, so this sum "+
			"is the true physical footprint. Use this rather than RSS for "+
			"any comparison against total memory.", groupLabels, nil)
	descSwapPSS = prometheus.NewDesc("process_group_memory_swap_pss_bytes",
		"Proportional swap summed across the group.", groupLabels, nil)
	descSwap = prometheus.NewDesc("process_group_memory_swap_bytes",
		"Swapped memory summed across the group, from VmSwap.", groupLabels, nil)
	descPSSCoverage = prometheus.NewDesc("process_group_pss_coverage_ratio",
		"Fraction of members whose smaps was readable. Below one means the "+
			"PSS total is a lower bound.", groupLabels, nil)
	descWorstFD = prometheus.NewDesc("process_group_worst_fd_ratio",
		"Highest open descriptors over limit across the members, taken per "+
			"process before aggregation. A ratio of the group sums cannot "+
			"detect one member near its limit; alert on this instead.",
		groupLabels, nil)
	descOpenFDs = prometheus.NewDesc("process_group_open_fds",
		"Open file descriptors summed across the group.", groupLabels, nil)
	descMaxFDs = prometheus.NewDesc("process_group_max_fds",
		"File descriptor limits summed across the group.", groupLabels, nil)
	descOldestStart = prometheus.NewDesc("process_group_oldest_start_time_seconds",
		"Unix start time of the oldest member, which gives the group age.",
		groupLabels, nil)
	descStates = prometheus.NewDesc("process_group_states",
		"Processes in each state.", append(append([]string{}, groupLabels...), "state"), nil)
	descFDCoverage = prometheus.NewDesc("process_group_fd_coverage_ratio",
		"Fraction of members whose descriptor count was readable. Below "+
			"one means the descriptor total is a lower bound.", groupLabels, nil)
	descIOCoverage = prometheus.NewDesc("process_group_io_coverage_ratio",
		"Fraction of members whose io file was readable. Below one means "+
			"the byte totals are a lower bound.", groupLabels, nil)

	descCPUSeconds = prometheus.NewDesc("process_group_cpu_seconds_total",
		"CPU seconds consumed by the group, accumulated from per-process "+
			"deltas so that process churn does not reset the counter.",
		append(append([]string{}, groupLabels...), "mode"), nil)
	descMinorFaults = prometheus.NewDesc("process_group_minor_page_faults_total",
		"Minor page faults accumulated across the group.", groupLabels, nil)
	descMajorFaults = prometheus.NewDesc("process_group_major_page_faults_total",
		"Major page faults accumulated across the group.", groupLabels, nil)
	descCtxSwitches = prometheus.NewDesc("process_group_context_switches_total",
		"Context switches accumulated across the group.",
		append(append([]string{}, groupLabels...), "kind"), nil)
	descReadBytes = prometheus.NewDesc("process_group_read_bytes_total",
		"Bytes read from storage, accumulated across the group.", groupLabels, nil)
	descWriteBytes = prometheus.NewDesc("process_group_write_bytes_total",
		"Bytes written to storage, accumulated across the group.", groupLabels, nil)
)

// Registry holds the collectors and the current snapshot.
//
// The snapshot is published by an atomic pointer swap and read by
// scrapes without a lock. A scrape never triggers a scan, so scrape
// cost is constant and the exporter's own cost is a function of its
// configuration alone rather than of how many Prometheus servers are
// pointed at it.
type Registry struct {
	reg  *prometheus.Registry
	snap atomic.Pointer[aggregate.Snapshot]

	scanDuration prometheus.Histogram
	scanCPU      prometheus.Counter
	scansTotal   prometheus.Counter
	overruns     prometheus.Counter
	readErrs     *prometheus.CounterVec

	lastScan   prometheus.Gauge
	procsTotal prometheus.Gauge
	procsScan  prometheus.Gauge
	procsIgn   prometheus.Gauge
	procsVan   prometheus.Gauge
	procsDen   prometheus.Gauge
	groups     prometheus.Gauge
	namesSeen  prometheus.Gauge
	stateEnts  prometheus.Gauge
	buildInfo  *prometheus.GaugeVec

	cpuAccum float64
}

// New builds the registry and registers every collector.
func New() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}

	r.scanDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "process_exporter_scan_duration_seconds",
		Help:    "Wall time taken by one scan.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})
	r.scanCPU = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "process_exporter_scan_cpu_seconds_total",
		Help: "CPU seconds consumed by the exporter itself while scanning. " +
			"This is the direct check that the exporter is not expensive.",
	})
	r.scansTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "process_exporter_scans_total",
		Help: "Completed scans. Counter values do not exist until this reaches 2.",
	})
	r.overruns = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "process_exporter_scan_overruns_total",
		Help: "Scans that exceeded the configured interval, causing a skipped tick.",
	})
	r.readErrs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "process_exporter_read_errors_total",
		Help: "Read failures by file kind, excluding vanished processes and " +
			"permission denials, both of which are normal.",
	}, []string{"file"})

	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
		r.reg.MustRegister(g)
		return g
	}

	r.reg.MustRegister(r.scanDuration, r.scanCPU, r.scansTotal, r.overruns, r.readErrs)

	r.lastScan = gauge("process_exporter_last_scan_timestamp_seconds",
		"Unix time of the most recent completed scan, for freshness checks.")
	r.procsTotal = gauge("process_exporter_procs_total",
		"PIDs seen in /proc during the last scan.")
	r.procsScan = gauge("process_exporter_procs_scanned",
		"Processes successfully read during the last scan.")
	r.procsIgn = gauge("process_exporter_procs_ignored",
		"Processes that matched the ignore list during the last scan.")
	r.procsVan = gauge("process_exporter_procs_vanished",
		"Processes that exited mid-scan. Normal at any real turnover rate.")
	r.procsDen = gauge("process_exporter_procs_denied",
		"Processes whose required files could not be read for permission reasons.")
	r.groups = gauge("process_exporter_groups_total",
		"Distinct groups currently exported.")
	r.namesSeen = gauge("process_exporter_group_names_seen_total",
		"Distinct group names ever observed. A large gap above "+
			"process_exporter_groups_total means a naming rule is extracting "+
			"an unbounded value.")
	r.stateEnts = gauge("process_exporter_state_entries",
		"Per-PID state map size. Tracks the live process count, not history.")

	r.buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "process_exporter_build_info",
		Help: "Build information, always 1.",
	}, []string{"version", "goversion"})
	r.reg.MustRegister(r.buildInfo)

	r.reg.MustRegister(&groupCollector{r: r})
	r.reg.MustRegister(collectors.NewGoCollector())
	r.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return r
}

// Handler returns the Prometheus exposition handler.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// Publish installs a new snapshot and updates the self-metrics.
//
// The swap is atomic, so a scrape sees either the previous snapshot or
// the new one and never a partial one.
func (r *Registry) Publish(s *aggregate.Snapshot) {
	r.snap.Store(s)

	r.scansTotal.Inc()
	r.scanDuration.Observe(s.Duration.Seconds())
	if s.SelfCPU > 0 {
		r.scanCPU.Add(s.SelfCPU)
	}
	r.lastScan.Set(float64(s.ScanAt.Unix()))
	r.procsTotal.Set(float64(s.ProcsTotal))
	r.procsScan.Set(float64(s.ProcsScanned))
	r.procsIgn.Set(float64(s.ProcsIgnored))
	r.procsVan.Set(float64(s.ProcsVanished))
	r.procsDen.Set(float64(s.ProcsDenied))
	r.groups.Set(float64(len(s.Groups)))
	r.namesSeen.Set(float64(s.NamesSeen))
	r.stateEnts.Set(float64(s.StateEntries))

	for file, n := range s.ReadErrs {
		r.readErrs.WithLabelValues(file).Add(float64(n))
	}
}

// Snapshot returns the current snapshot for the API handlers.
func (r *Registry) Snapshot() *aggregate.Snapshot { return r.snap.Load() }

// AddOverrun records a scan that exceeded the interval.
func (r *Registry) AddOverrun() { r.overruns.Inc() }

// SetBuildInfo publishes the version.
func (r *Registry) SetBuildInfo(version, goVersion string) {
	r.buildInfo.WithLabelValues(version, goVersion).Set(1)
}

// groupCollector implements prometheus.Collector over the current
// snapshot.
//
// Implementing Collector rather than holding GaugeVec instances avoids
// the delete-on-disappear problem: a group that vanishes simply is not
// in the next snapshot, so it stops being collected, with no explicit
// cleanup and no risk of a stale series lingering.
type groupCollector struct{ r *Registry }

// Describe implements prometheus.Collector.
func (c *groupCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descNumProcs
	ch <- descNumThreads
	ch <- descRSS
	ch <- descVSize
	ch <- descShared
	ch <- descData
	ch <- descPSS
	ch <- descSwapPSS
	ch <- descSwap
	ch <- descPSSCoverage
	ch <- descOpenFDs
	ch <- descMaxFDs
	ch <- descWorstFD
	ch <- descOldestStart
	ch <- descStates
	ch <- descFDCoverage
	ch <- descIOCoverage
	ch <- descCPUSeconds
	ch <- descMinorFaults
	ch <- descMajorFaults
	ch <- descCtxSwitches
	ch <- descReadBytes
	ch <- descWriteBytes
}

// Collect implements prometheus.Collector, emitting every metric for
// every group in the current snapshot.
func (c *groupCollector) Collect(ch chan<- prometheus.Metric) {
	snap := c.r.snap.Load()
	if snap == nil {
		return
	}

	g := func(d *prometheus.Desc, v float64, lv ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, lv...)
	}
	cnt := func(d *prometheus.Desc, v float64, lv ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, lv...)
	}

	for _, s := range snap.List {
		n, u := s.Key.Name, s.Key.User

		g(descNumProcs, float64(s.NumProcs), n, u)
		g(descNumThreads, float64(s.Threads), n, u)
		g(descRSS, float64(s.RSSBytes), n, u)
		g(descVSize, float64(s.VSizeBytes), n, u)
		g(descShared, float64(s.SharedBytes), n, u)
		g(descData, float64(s.DataBytes), n, u)
		g(descSwap, float64(s.SwapBytes), n, u)
		g(descOldestStart, s.OldestStart, n, u)
		g(descPSSCoverage, s.PSSCoverage(), n, u)

		// Omitted rather than zeroed when nothing was readable, so
		// that "no proportional data" and "no memory" cannot be
		// mistaken for each other.
		if s.PSSRead > 0 {
			g(descPSS, float64(s.PSSBytes), n, u)
			g(descSwapPSS, float64(s.SwapPSSBytes), n, u)
		}
		g(descFDCoverage, s.FDCoverage(), n, u)
		g(descIOCoverage, s.IOCoverage(), n, u)

		// Descriptor gauges are emitted only when at least one member
		// was readable, so that an unprivileged exporter reports no
		// value rather than a misleading zero.
		if s.FDsRead > 0 {
			g(descOpenFDs, float64(s.OpenFDs), n, u)
			if s.MaxFDs > 0 {
				g(descMaxFDs, float64(s.MaxFDs), n, u)
				g(descWorstFD, s.WorstFDRatio, n, u)
			}
		}

		for state, count := range s.StateNames {
			g(descStates, float64(count), n, u, state)
		}

		cnt(descCPUSeconds, s.Accum.UTimeSeconds, n, u, "user")
		cnt(descCPUSeconds, s.Accum.STimeSeconds, n, u, "system")
		cnt(descMinorFaults, float64(s.Accum.MinorFaults), n, u)
		cnt(descMajorFaults, float64(s.Accum.MajorFaults), n, u)
		cnt(descCtxSwitches, float64(s.Accum.VolCtxSw), n, u, "voluntary")
		cnt(descCtxSwitches, float64(s.Accum.InvolCtxSw), n, u, "involuntary")

		if s.IORead > 0 {
			cnt(descReadBytes, float64(s.Accum.ReadBytes), n, u)
			cnt(descWriteBytes, float64(s.Accum.WriteBytes), n, u)
		}
	}
}

// keep time referenced for the duration observation above.
var _ = time.Second
