# Process Exporter

A Prometheus exporter that reads `/proc` on an interval, groups every running process by **name and owning user**, and publishes the group totals. No PID appears in any label.

```
process_group_cpu_seconds_total{mode="user",name="python/app",user="geoff"} 0.42
process_group_memory_rss_bytes{name="python/app",user="geoff"} 44036096
process_group_num_procs{name="nginx",user="www-data"} 40
process_group_num_procs{name="nginx",user="root"} 1
```

Linux only. Two dependencies. Under 3,000 lines of Go.

## Why It's Fast

![Diagram](docs/process_exporter.png)

## Why

Two constraints shape the entire design.

### Cardinality must be bounded

A Prometheus series is identified by its label set. If PID were a label, every process start would create a new series. A machine forking once a second produces 86,400 series a day, each living in memory until it ages out. The series count becomes a function of workload churn — something no operator chose and nobody can predict.

The process name is bounded. A machine runs some finite set of programs, and that set changes when software is installed, not when a program runs. Keying on name makes the series count a property of the machine's **software inventory**.

The owning user is added as a second label because it is also bounded and it earns its keep: one `nginx` owned by `root` and forty owned by `www-data` is the master-and-workers structure made visible.

### CPU cost must be negligible, at a useful interval

The exporter competes for the resource it measures. A scan that reads two thousand `/proc` entries as fast as it can appears in `top` as a significant consumer, which is unacceptable for a monitoring agent.

The obvious way to make an exporter cheap is to run it rarely, but that trades away the thing it exists to capture. At a 15-second interval, a process that burns a core for half that time contributes one sample and the rate calculation averages it away. **Five seconds is the slowest interval that captures anything useful**, and one second resolves the shape of a short event. The default is 5s, and the cost is controlled by how the scan is done rather than by how seldom it runs.

The exporter must not appear near the top of `top`. This is achieved by breaking the scan into batches with sleeps between them, by separating cheap reads from expensive ones and running them at different frequencies, by caching values that cannot change, and by running as a daemon so no startup cost is paid per scrape.

It measures its own cost and publishes it, so the constraint is verifiable rather than assumed:

```promql
rate(process_exporter_scan_cpu_seconds_total[5m])   # typically 0.01 at a 5s interval
```

---

## How it works

### Gauges are read; counters are accumulated

`/proc` exposes two kinds of value, and they need completely different handling.

**Gauges** are instantaneous. Resident memory is a page count right now. Read it, sum it across the group, publish it. One read suffices.

**Counters** are monotonic totals since process start. A single read of CPU ticks gives a lifetime total, which is not what anyone wants to see. The useful value is the rate, which needs two readings separated by a known interval.

### Why counters need per-PID state

Three consequences follow from needing two readings:

**State is per-PID, even though output is per-group.** A counter belongs to one process. Two processes named `nginx` have separate counters that increase independently.

**The delta is computed per PID, then summed.** Summing raw lifetime counters across a group and differencing the sum produces a wrong answer the moment one member exits: the sum drops discontinuously and the difference goes negative.

**The first scan produces no counter values.** There is no previous reading. The first scan establishes the baseline; the second produces the first delta. `/readyz` returns 503 until two scans have completed.

### Why the published counter is an accumulator

A Prometheus counter must increase monotonically; any decrease is read as a reset and the interval is discarded.

If the exported value were the sum of the live members' lifetime totals, it would drop every time a worker exited. Instead there is an accumulator per group:

```
accumulator[group] += Σ (ticks_now[pid] − ticks_prev[pid])
```

It only ever increases, regardless of process churn. A worker exiting simply stops contributing new deltas. This is what makes the counter survive a rolling restart of forty nginx workers without a single reset.

### PID reuse

The kernel wraps PIDs, so an old state entry can be matched against a new process holding the same number — whose counters start near zero, producing a large negative delta.

The defence is the process start time from `stat` field 22, which is fixed for the life of a process. A mismatch means PID reuse: the stale entry is deleted and the process is treated as new. A second line of defence clamps every delta at zero.

### State pruning

Each scan increments a generation counter and stamps every PID it observes. Entries stamped with an older generation belong to processes that no longer exist and are deleted.

The state map size therefore tracks live processes, not historical ones. This is what makes the exporter safe to run for months without a restart.

### Group accumulators outlive their processes

Accumulators are kept for `group_retention` (default 1h) after the group's last process exits. A service stopped and restarted three minutes later resumes its counter where it left off, instead of showing Prometheus a reset.

---

## Quick start

```bash
git clone <repo> process_exporter && cd process_exporter
./build.sh

sudo ./process_exporter -config config.example.yaml
```

In another terminal:

```bash
curl -s localhost:9256/stats | jq
curl -s localhost:9256/groups | jq '.groups[:10]'
curl -s localhost:9256/metrics | grep process_group_cpu
```

Counters will be zero until the second scan. `process_exporter_scans_total` reading 1 tells you that is why.

---

## Configuration

```yaml
scan:
  interval: 5s           # scan period, and the counter sampling resolution
  batch_size: 50         # processes read between yields
  batch_sleep: 4ms       # yield duration
  fd_scan_every: 12      # read /proc/<pid>/fd every Nth scan
  read_smaps: true       # proportional set size, the honest memory figure
  smaps_scan_every: 12   # read /proc/<pid>/smaps_rollup every Nth scan
  read_io: true
  read_status: true
  cache_cmdline: true    # cache the derived name per PID
  group_retention: 1h    # accumulator survival after the group vanishes

filter:
  ignore_kernel_threads: true
  # ignore_comm, ignore_comm_prefix, ignore_comm_regex:
  #   omit for the shipped defaults, or set to [] to disable them

naming:
  fallback: comm         # comm | exe_basename | cmdline_basename
  resolve_users: true
  # rules: omit for the shipped defaults

server:
  listen: "0.0.0.0:9256"
```

Validate before applying:

```bash
process_exporter -check -config /etc/process_exporter/config.yaml
```

It reports every problem at once, including a scan-duration estimate that catches the most common tuning mistake:

```
scan.batch_sleep 50ms at batch_size 10 would take about 10s for 2000 processes, which exceeds scan.interval 5s
```

### The batch and sleep

Three scan shapes, not one. The expensive reads are an order of magnitude
above the base cost, so a single average is the wrong figure to size against:

```
cheap scan ≈ (procs ÷ batch_size) × batch_sleep + procs × read_cost
fd scan ≈ cheap + procs × fd_cost
smaps scan ≈ cheap + procs × smaps_cost
```

Size against the largest, not the average. `-check` reports all three.

With 2,000 processes, batch 50, sleep 4ms: 160ms sleeping plus roughly 100ms reading, out of a 5-second interval. About 2% of one core on a busy machine, and well under 1% on a machine of a few hundred processes.

**The sleep is not wasted time.** It is what stops the scheduler from classifying the exporter as a CPU-bound task. A process that yields voluntarily and often is scheduled differently from one that runs to the end of its quantum, and that difference is why the exporter can scan two thousand processes without appearing in a `top` listing.

### Read costs are not equal

| File | Cost | Frequency |
|---|---|---|
| `stat`, `statm` | One small read each | Every scan |
| `status`, `io` | One read each | Every scan, if enabled |
| `cmdline`, `exe` | One read each | Only when the name is uncached |
| **`fd/`** | **Directory walk** | Every `fd_scan_every` scans |
| **`smaps_rollup`** | **Page table walk** | Every `smaps_scan_every` scans |

Two reads are an order of magnitude more expensive than the rest, and both refresh data that changes slowly, so both run on their own schedule.

Enumerating `/proc/<pid>/fd/` requires the kernel to walk the file descriptor table and produce a directory entry for each one. A process holding ten thousand descriptors costs more than a hundred `stat` reads.

Reading `/proc/<pid>/smaps_rollup` requires the kernel to walk the page tables. It is what makes proportional set size available, and it is worth the cost — see the memory section below.

At the defaults both refresh once a minute, and they are offset onto different scans rather than stacking on the same one. Between refreshes the cached value is reported. Without that, both gauges would drop to zero on eleven scans out of twelve, which would look like every process closing all its files and releasing all its memory.

---

## Filtering

A stock Linux server runs many kernel threads that carry no useful signal:

```
kworker/0:1  kworker/u16:3  ksoftirqd/0  migration/0  rcu_sched  kthreadd
```

Three problems: they have no address space, so their memory readings are meaningless; their CPU is better observed through `node_exporter`; and their names are numerous, machine-specific, and dynamically created, which makes them a cardinality problem as well as a noise problem.

The shipped ignore list covers roughly fifty exact names, eighteen prefixes, and one regex. On a typical server `procs_ignored` will exceed `procs_scanned` by a factor of three or more. That is correct.

Omitting a list gets the defaults. Setting it to `[]` disables them. Setting your own replaces them — there is no implicit merge, because that would make a default impossible to remove.

---

## Naming rules

For most programs `comm` is the right name. For interpreters it is not:

| Process | `comm` | What you want |
|---|---|---|
| A Spring service | `java` | `java/OrderService` |
| A Django app | `python3` | `python/manage` |
| A container shim | `containerd-shim` | `containerd-shim` |

Without rules, every JVM on the machine merges into one `java` group.

```yaml
naming:
  rules:
    - match_comm: java
      from: cmdline
      regex: '-jar\s+\S*?([^/\s]+)\.jar'
      name: 'java/$1'

    - match_comm: java
      from: cmdline
      regex: '\s([a-zA-Z][\w.]*\.[A-Z]\w*)(?:\s|$)'
      name: 'java/$1'
```

**A rule that matches but whose extraction fails falls through to the next rule.** That is what makes the pair above work as a cascade: the first catches `-jar app.jar`, the second catches a bare main class, and a JVM invoked either way gets a useful name.

A rule with no `regex` emits its name literally. This is how container shims collapse to one series instead of one per container.

### Naming rules are the cardinality control

There is no hard cap on group count. That was deliberate — a cap that folds groups into an `other` bucket hides the problem rather than surfacing it. The naming rules are the control, and the one way to get this wrong is a rule that extracts an unbounded value:

```yaml
# Wrong — instance IDs are per-process
regex: '-Dinstance=(\S+)'

# Right — service names come from a finite list
regex: '-Dservice=(\S+)'
```

Two metrics form the diagnostic:

```promql
process_exporter_group_names_seen_total - process_exporter_groups_total
```

Distinct names ever seen, minus groups currently exported. A healthy machine has these equal and both flat. A **widening gap** means names appear briefly and disappear, which is the signature of an unbounded extraction.

Verify a naming rule on one machine before deploying it fleet-wide.

---

## Privilege

The exporter runs at whatever privilege it was given and does not complain.

| Running as | CPU, RSS, threads | I/O bytes | Open descriptors | PSS |
|---|---|---|---|---|
| root | All processes | All processes | All processes | All processes |
| Unprivileged | All processes | Own only | Own only | Own only |

The kernel exposes `stat` and `statm` for every process regardless of ownership, so **CPU, RSS, and thread data are always complete**. `io`, `fd/`, and `smaps_rollup` need a matching UID or `CAP_SYS_PTRACE`.

Note what the unprivileged path loses: without PSS you have RSS only, which over-counts shared pages with no correction available. On any machine running a forked master-and-workers service that is a real limitation, and it is the strongest argument for granting the capability.

When data cannot be read, the metric is **omitted rather than zeroed**, and a coverage ratio says why:

```
process_group_io_coverage_ratio{name="postgres",user="postgres"} 0
```

Zero and unknown are different facts. A group that does no I/O and a group whose I/O cannot be seen must not look identical in a graph.

---

## Metrics

Every process-group metric carries exactly two labels: `name` and `user`.

**Gauges**

| Metric | Unit |
|---|---|
| `process_group_num_procs` | count |
| `process_group_num_threads` | count |
| `process_group_memory_rss_bytes` | bytes |
| `process_group_memory_vsize_bytes` | bytes |
| `process_group_memory_shared_bytes` | bytes |
| `process_group_memory_data_bytes` | bytes |
| `process_group_memory_pss_bytes` | bytes |
| `process_group_memory_swap_bytes` | bytes |
| `process_group_memory_swap_pss_bytes` | bytes |
| `process_group_open_fds` | count |
| `process_group_max_fds` | count |
| `process_group_worst_fd_ratio` | ratio |
| `process_group_oldest_start_time_seconds` | unix seconds |
| `process_group_states{state}` | count |
| `process_group_fd_coverage_ratio` | ratio |
| `process_group_io_coverage_ratio` | ratio |
| `process_group_pss_coverage_ratio` | ratio |

**Counters**

| Metric | Unit |
|---|---|
| `process_group_cpu_seconds_total{mode}` | seconds |
| `process_group_minor_page_faults_total` | count |
| `process_group_major_page_faults_total` | count |
| `process_group_context_switches_total{kind}` | count |
| `process_group_read_bytes_total` | bytes |
| `process_group_write_bytes_total` | bytes |

**Self-metrics** — `process_exporter_scan_cpu_seconds_total`,
`_scan_duration_seconds`, `_scans_total`, `_scan_overruns_total`,
`_groups_total`, `_group_names_seen_total`, `_state_entries`, `_procs_total`,
`_procs_ignored`, `_procs_vanished`, `_procs_denied`, `_read_errors_total`,
`_last_scan_timestamp_seconds`.

### Which memory metric to use

Summing RSS across processes that share pages **over-counts**. Forty nginx workers forked from one master share most of their text and much of their data, so the sum exceeds the group's true footprint. A group's RSS total can exceed physical memory.

Four memory metrics are published, and they are different quantities:

| Metric | What it measures | Group sum is |
|---|---|---|
| `memory_pss_bytes` | Every resident page, each shared page divided by the number of processes sharing it | **Correct** |
| `memory_rss_bytes` | Every resident page, counted in full for each process | Over-counts |
| `memory_data_bytes` | Private writable pages only | Under-counts |
| `memory_shared_bytes` | Resident shared pages | Diagnostic only |

**Use `process_group_memory_pss_bytes`** for anything that needs a real number, including any comparison against `node_memory_MemTotal_bytes`:

```promql
sum(process_group_memory_pss_bytes) / node_memory_MemTotal_bytes
```

`memory_data_bytes` is **not** an approximation of PSS. It is private writable memory: it excludes text and shared segments entirely, where RSS double-counts them. Neither reliably brackets the true figure. It remains a good heap leak indicator, because a leak lands in private writable memory, but it is not a substitute.

PSS is on by default. Set `read_smaps: false` on a kernel older than 4.14, where the cheap `smaps_rollup` does not exist and the fallback reads one entry per memory mapping — hundreds for a large JVM.

### Alert on the worst FD ratio, not the ratio of sums

`process_group_open_fds / process_group_max_fds` is a ratio of group sums and **cannot detect the case it exists for**.

Ten processes, soft limit 4096 each. One holds 4000 descriptors, nine hold 40:

| Quantity | Value |
|---|---|
| `open_fds` sum | 4360 |
| `max_fds` sum | 40960 |
| Ratio of sums | **0.11** |
| `worst_fd_ratio` | **0.98** |

An alert on the ratio of sums reads 0.11 and stays silent while one process is 96 descriptors from failing. The dilution scales with group size — forty nginx workers dilute the signal by a factor of forty.

`process_group_worst_fd_ratio` is the maximum of the per-process ratios, taken before aggregation. It costs nothing extra, because both values are already read per process. Keep `open_fds` for capacity planning and alert on `worst_fd_ratio`.

### Cardinality in practice

```
series ≈ groups × (17 gauges + 6 counters + state variants)
```

| Machine | Groups | Approximate series |
|---|---|---|
| Small VM | 20 | 400 |
| Typical server | 60 | 1,200 |
| Busy application server | 200 | 4,000 |

Compare against the PID-keyed alternative: tens of thousands of series per day, growing without bound.

---

## HTTP endpoints

| Path | Content |
|---|---|
| `/metrics` | Prometheus exposition |
| `/groups` | Current snapshot as JSON, `?sort=cpu\|rss\|procs\|fds\|name` |
| `/stats` | Scan counters and the exporter's own CPU use |
| `/config` | Effective configuration |
| `/livez` | Process is running |
| `/readyz` | At least two scans have completed |

`/groups` is the debugging equivalent of `top` and answers "what is this exporter actually seeing" without going through Prometheus.

**A scrape never triggers a scan.** The scanner runs on its own timer and scrapes read the last completed snapshot through an atomic pointer load. Scrape cost is constant, and the exporter's own cost is a function of its configuration alone rather than of how many Prometheus servers point at it.

---

## Tuning

| Machine | interval | batch_size | batch_sleep | fd_scan_every | Approx CPU |
|---|---|---|---|---|---|
| Under 200 procs | 15s | 100 | 2ms | 2 | 0.1% |
| Under 1,000 | 15s | 50 | 5ms | 4 | 0.5% |
| 1,000–5,000 | 30s | 50 | 5ms | 8 | 0.5% |
| Over 5,000 | 60s | 100 | 10ms | 10 | 0.4% |
| Latency-sensitive | 60s | 25 | 20ms | 20 | 0.1% |

Do not tune blind. `rate(process_exporter_scan_cpu_seconds_total[5m])` gives
the answer directly. In order of effectiveness: double `interval`, then raise
`fd_scan_every`, then raise `batch_sleep`.

Watch for overruns. A scan exceeding the interval skips the next tick rather
than running two scans concurrently, since both would mutate the state map:

```promql
rate(process_exporter_scan_overruns_total[1h]) > 0
```

---

## Alerting

```yaml
# The worst individual member, not a ratio of the group sums, which
# averages away the single process that is about to fail.
- alert: ProcessGroupFDExhaustion
  expr: process_group_worst_fd_ratio > 0.9
  for: 5m

- alert: ProcessGroupCrashLooping
  expr: changes(process_group_oldest_start_time_seconds[15m]) > 3
  for: 5m

- alert: ProcessExporterExpensive
  expr: rate(process_exporter_scan_cpu_seconds_total[5m]) > 0.05
  for: 15m

- alert: ProcessExporterNameChurn
  expr: |
    process_exporter_group_names_seen_total
      - process_exporter_groups_total > 200
  for: 30m
```

---

## Repository layout

```
build.sh
config.example.yaml

cmd/process_exporter/main.go     wiring, lifecycle, scan loop

internal/config/                 structs, defaults, validation, reload
internal/procfs/                 one stateless reader per /proc file, including smaps for proportional memory
internal/filter/                 ignore list matching
internal/namer/                  group name derivation
internal/scan/state.go           per-PID state, generation pruning
internal/scan/scan.go            batched walk, delta computation
internal/aggregate/              gauge sums, counter accumulators
internal/metrics/                registry, prometheus.Collector
internal/api/                    HTTP handlers

docs/tech_spec.md                full technical specification
docs/user_manual.md              installation, tuning, scenarios, queries
```

### Reading order for a new contributor

1. **`internal/scan/state.go`** — `PIDState` and `StateMap`. The generation stamp and the start-time check are the whole basis of correct counters.  Short file.
2. **`internal/scan/scan.go`** — `readProcess`. Note the order of operations: which files are read, when the filter is applied, where the name cache short-circuits.
3. **`internal/aggregate/aggregate.go`** — `Apply` and `accumulate`. The accumulator is what keeps counters monotonic.
4. **`internal/namer/namer.go`** — `Name` and the fall-through cascade.
5. **`internal/metrics/metrics.go`** — `groupCollector.Collect`. Note which metrics are conditionally emitted.
6. **`cmd/process_exporter/main.go`** — the scan loop and overrun handling.

`internal/procfs` is a set of independent parsers and can be read in any order or skipped.

### One implementation note

The metrics package implements `prometheus.Collector` over the snapshot rather than holding `GaugeVec` instances. A group that vanishes is simply absent from the next snapshot and stops being collected — no `Delete` call to forget, and no stale series to leak.

---

## Documentation

| Document | Contents |
|---|---|
| [`docs/tech_spec.md`](docs/tech_spec.md) | Architecture, data model, every algorithm, every metric, design decisions |
| [`docs/user_manual.md`](docs/user_manual.md) | Installation, configuration reference, cardinality management, tuning, scaling scenarios, Prometheus queries, troubleshooting |

---

## Requirements

- Go 1.22 or later
- Linux (`/proc` layout and `getrusage` are assumed)
- Prometheus, for collection

Dependencies: `prometheus/client_golang`, `gopkg.in/yaml.v3`. Everything that
reads `/proc` is standard library.

---

## Known limitations

| Item | Status |
|---|---|
| RSS over-counting | Corrected by `memory_pss_bytes`, on by default via `read_smaps`, refreshed every 12th scan. Set false on kernels older than 4.14 with large-mapping processes |
| Old kernels | Without `smaps_rollup` (pre-4.14) the PSS fallback reads `smaps`, one entry per memory mapping. Consider `read_smaps: false` there |
| argv rewriting | A process changing its own argv keeps its cached name until restart. Set `cache_cmdline: false` to disable |
| `comm` truncation | Fifteen characters, kernel-imposed. Naming rules or `fallback: exe_basename` are the workaround |
| Container awareness | None. A containerised process is named by its binary like any other |
| Per-thread data | Thread count only. Per-thread would multiply cardinality for little gain |
| Counter persistence | None. A restart resets every counter, which `rate()` handles correctly |
| Cgroup metrics | Not collected. `cadvisor` is the right tool |
| Port 9256 | Collides with the community `process-exporter` if both run |

---

## License

MIT
