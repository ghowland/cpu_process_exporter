# process_exporter — Technical Specification

**Version:** 1.0 (as implemented)
**Binary:** `process_exporter`
**Language:** Go 1.22
**Platform:** Linux only

---

# Part 1 — What this system is

## 1.1 Purpose

A Prometheus exporter that reads `/proc` on a fixed interval, groups every running process by name and owning user, sums each metric across the members of a group, and publishes the group totals.

It answers questions of this form:

- How much resident memory is `nginx` using on this box, in total across all its workers?
- Is the `postgres` group approaching its file descriptor limit?
- Which group's CPU consumption grew after last night's deploy?
- How many `python3` processes does `deploy` run, and how many does `root` run?

## 1.2 The unit of measurement

The unit is the **process group**, not the process. A group is every process sharing a name and an owning user. Forty `nginx` workers owned by `www-data` produce one series per metric, carrying the sum across all forty, plus a count of how many there were.

Individual processes are read, aggregated, and discarded within one scan. **No PID appears anywhere in any label.**

Live output from a running instance:

```
process_group_cpu_seconds_total{mode="user",name="python/app",user="geoff"} 0.42
process_group_cpu_seconds_total{mode="system",name="python/app",user="geoff"} 0.09
process_group_memory_rss_bytes{name="python/app",user="geoff"} 44036096
process_group_num_procs{name="python/app",user="geoff"} 3
```

Note `python/app`: the naming rules extracted the script name from the command line, because `comm` for that process is `python3` and grouping on `comm` alone would merge every Python service on the machine into one bucket.

## 1.3 Two constraints that shape everything

### Constraint 1: bounded cardinality

A Prometheus time series is identified by its label set. If PID were a label, every process start would create a new series. A machine forking a short-lived process every second creates 86,400 series per day, each living in memory until it ages out. The series count becomes unbounded and driven by workload behaviour rather than by anything the operator chose.

The process name is bounded. A machine runs some finite set of programs, and that set changes when software is installed, not when a program runs. Keying on name makes the series count a property of the machine's software inventory.

Adding the owning user multiplies this by a small, also-bounded factor, and it earns its keep: one `nginx` owned by `root` and forty owned by `www-data` is the master-and-workers structure made visible.

### Constraint 2: negligible CPU cost

The exporter competes for the resource it measures. A scan that reads two thousand `/proc` entries as fast as it can appears in `top` as a significant consumer, which is unacceptable for a monitoring agent.

The exporter must not appear near the top of `top`. This is achieved by breaking the scan into batches with sleeps between them, by separating cheap reads from expensive ones and running them at different frequencies, by caching values that cannot change, and by running as a long-lived daemon so no startup cost is paid per scrape.

The exporter measures its own cost and publishes it as `process_exporter_scan_cpu_seconds_total`, so the constraint is verifiable rather than assumed.

---

# Part 2 — Core mechanics

Read this section before any other. Everything else follows from it.

## 2.1 Gauges are read; counters are accumulated

`/proc` exposes two kinds of value, and they need completely different handling.

**Gauges** are instantaneous. Resident memory is a page count right now. Open file descriptors is a directory entry count right now. Read it, sum it across the group, publish it. One read suffices.

**Counters** are monotonic totals since process start. CPU is ticks consumed since the process began; page faults, context switches, and I/O bytes have the same shape. A single read gives a lifetime total, which is not what anyone wants to see.

## 2.2 Why counters need per-PID state

The useful value from a counter is the rate, which requires two reads separated by a known interval:

```
delta = ticks_now − ticks_previous
```

The exporter must therefore remember the previous value for each process. Three consequences follow:

**State is per-PID, even though output is per-group.** A counter belongs to one process. Two processes named `nginx` have separate counters that both increase independently.

**The delta must be computed per PID, then summed.** Summing raw lifetime counters across a group and differencing the sum produces a wrong answer the moment one member exits: the sum drops discontinuously and the difference goes negative. Per-PID first, sum second.

**The first scan produces no counter deltas.** There is no previous reading. The first scan establishes the baseline; the second produces the first usable delta. This is why the sample output above shows zeros immediately after start.

## 2.3 Why the published counter is an accumulator

A Prometheus counter must increase monotonically within a series. Prometheus treats any decrease as a counter reset and discards the interval.

If the exporter published a group's CPU counter as the sum of its live members' lifetime totals, the value would drop every time a worker exited. Prometheus would see a reset and the data would be wrong.

The correct construction is an **accumulator per group**:

```
accumulator[group] += Σ (ticks_now[pid] − ticks_prev[pid])   over pids in group
```

The accumulator only ever increases, regardless of churn. A process exiting simply stops contributing new deltas. The accumulator is published; the per-PID counters are internal working values that never leave the scan.

## 2.4 PID reuse

The kernel wraps PIDs. An old state entry can be matched against a new process holding the same number, whose counters start near zero, producing a large negative delta.

The defence is the process start time. `/proc/<pid>/stat` field 22 gives the start time in clock ticks since boot; it is fixed for the life of a process and effectively unique in combination with the PID.

Each state entry stores it. On lookup:

- Start time matches → same process, compute the delta.
- Start time differs → PID reuse. The stale entry is deleted, the process is treated as new, and no delta is produced this scan.

A second line of defence clamps every delta at zero, so a negative value from any source never reaches a counter.

## 2.5 State pruning

The per-PID state map grows every time a new process appears. Without pruning it grows without bound.

Each scan increments a **generation counter** and stamps every PID it observes. At the end of the scan, entries whose stamp is older than the current generation belong to processes that no longer exist and are deleted.

The map size therefore tracks live processes, not historical ones. This is what makes the exporter safe to run for months without a restart. It is published as `process_exporter_state_entries`.

## 2.6 Group accumulator lifetime

Accumulators must not be pruned on the same schedule as PID state. A group whose processes all exit and then restart minutes later must resume its counter where it left off, or Prometheus sees a reset.

Accumulators are kept for `group_retention` (default 1h) after the group was last seen. During that window the group emits no gauge series — it has no live members — but its accumulator survives. Beyond the window the group is forgotten, on the assumption that the program was uninstalled or permanently stopped.

## 2.7 Caching what cannot change

Several values are fixed for the life of a process:

| Value | Source |
|---|---|
| Command line | `/proc/<pid>/cmdline` |
| Executable path | `/proc/<pid>/exe` |
| UID | `/proc/<pid>/status` |
| Start time | `/proc/<pid>/stat` field 22 |
| **Derived group name** | Namer output over the above |

The derived name and user are cached in the PID state entry. On subsequent scans, when the start time matches, the cached values are reused and `cmdline` is not read at all. This removes one read and one namer evaluation per process per scan for every long-lived process, which is the large majority.

The trade: a process that rewrites its own argv, which some servers do to display status, keeps its original name until it restarts. Re-reading the command line every scan for every process to catch a rare in-place rename is not worth the cost.

Controlled by `scan.cache_cmdline`, default true.

---

# Part 3 — Architecture

## 3.1 Component diagram

```
                    ┌──────────────────┐
                    │  Config (YAML)   │
                    │  reload on SIGHUP│
                    └────────┬─────────┘
                             │
         ┌───────────────────┼───────────────────┐
         v                   v                   v
  ┌─────────────┐    ┌──────────────┐    ┌──────────────┐
  │   Filter    │    │    Namer     │    │  Scan loop   │
  │ ignore list │    │ naming rules │    │ own timer    │
  └──────┬──────┘    └──────┬───────┘    └──────┬───────┘
         │                  │                   │
         └──────────┬───────┴───────────────────┘
                    v
            ┌───────────────┐        ┌──────────────────┐
            │   Scanner     │<──────>│   StateMap       │
            │ batched walk  │        │ prev counters,   │
            │ of /proc      │        │ start times,     │
            │               │        │ cached names,    │
            │               │        │ generation       │
            └───────┬───────┘        └──────────────────┘
                    │ []Process (deltas already computed)
                    v
            ┌───────────────┐        ┌──────────────────┐
            │  Aggregator   │<──────>│  Accumulators    │
            │ sum gauges,   │        │ monotonic totals │
            │ add deltas    │        │ per group key    │
            └───────┬───────┘        └──────────────────┘
                    │ *Snapshot
                    v
            ┌───────────────┐
            │   Registry    │  atomic.Pointer swap
            │ groupCollector│
            └───────┬───────┘
                    v
            ┌───────────────┐
            │  HTTP server  │  reads the snapshot, never scans
            └───────────────┘
```

## 3.2 The decoupling rule

**A scrape never triggers a scan.** The scanner runs on its own timer. A scrape reads the most recent completed snapshot through an atomic pointer load.

This is deliberate:

- Scrape cost is constant and tiny: serialising an in-memory structure.
- Scan cost is independent of scrape frequency. Ten Prometheus servers scraping does not multiply the cost by ten.
- A slow scan cannot cause a scrape timeout.
- The exporter's own CPU use is a function of its configuration alone, which is what makes it predictable.

The snapshot carries the timestamp of the scan that produced it, published as `process_exporter_last_scan_timestamp_seconds`, so staleness is visible.

## 3.3 Package layout

```
build.sh
go.mod
config.example.yaml

cmd/process_exporter/main.go     wiring, lifecycle, scan loop

internal/config/config.go        structs and the Duration wrapper
internal/config/defaults.go      defaults, ignore lists, naming rules
internal/config/load.go          parse, normalise, validate
internal/config/watcher.go       SIGHUP and file-change reload

internal/procfs/procfs.go        ListPIDs, IsVanished, IsDenied
internal/procfs/system.go        clock ticks, page size, boot time, UID cache
internal/procfs/stat.go          /proc/<pid>/stat parser
internal/procfs/statm.go         /proc/<pid>/statm parser
internal/procfs/status.go        /proc/<pid>/status parser
internal/procfs/io.go            /proc/<pid>/io parser
internal/procfs/fd.go            fd count and limit
internal/procfs/cmdline.go       cmdline and exe readers

internal/filter/filter.go        ignore list matching
internal/namer/namer.go          group name derivation

internal/scan/state.go           per-PID state map, generation pruning
internal/scan/scan.go            batched walk, yields, delta computation

internal/aggregate/aggregate.go  gauge sums, counter accumulators, snapshot
internal/metrics/metrics.go      registry and prometheus.Collector
internal/api/api.go              HTTP handlers
```

Twenty-two Go files.

## 3.4 Dependencies

| Module | Purpose |
|---|---|
| `github.com/prometheus/client_golang` | Registry and exposition |
| `gopkg.in/yaml.v3` | Configuration parsing |

All `/proc` reading is standard library.

---

# Part 4 — Data model

## 4.1 Group key

```go
type Key struct {
    Name string
    User string
}
```

Two fields. Nothing else. Every metric carries exactly this pair as labels.

## 4.2 Per-process reading

Assembled fresh each scan, consumed by the aggregator, discarded.

```go
type Process struct {
    PID, PPID  int
    Comm       string   // kernel short name, max 15 chars
    Name       string   // derived group name
    User       string
    UID        uint32
    State      byte     // R, S, D, Z, T, I, ...
    StartTicks uint64   // identity anchor
    StartUnix  float64

    // Gauges
    RSSBytes, VSizeBytes, SharedBytes, DataBytes uint64
    Threads, OpenFDs, MaxFDs                     int

    // Counter deltas, already differenced against state
    DUTimeTicks, DSTimeTicks   uint64
    DMinorFaults, DMajorFaults uint64
    DVolCtxSw, DInvolCtxSw     uint64
    DReadBytes, DWriteBytes    uint64

    // Read status
    HaveDeltas, HaveIO, HaveFDs, HaveStatus bool
}
```

The four `Have*` fields matter. An unprivileged exporter cannot read `/proc/<pid>/io` for processes it does not own. Reporting zero would be a lie. The aggregator skips the metric for that process and reports coverage instead.

## 4.3 Per-PID state

```go
type PIDState struct {
    StartTicks uint64  // must match, or the PID was reused
    Generation uint64  // scan number when last seen

    UTimeTicks, STimeTicks   uint64
    MinorFaults, MajorFaults uint64
    VolCtxSw, InvolCtxSw     uint64
    ReadBytes, WriteBytes    uint64

    // Cached, immutable for the process lifetime
    Name string
    User string
    UID  uint32

    // Cached descriptor values, refreshed only on an fd scan
    OpenFDs, MaxFDs int
    HaveFDs         bool
}
```

Roughly 120 bytes per process. Two thousand processes costs 240 KB.

## 4.4 Group accumulator and sample

```go
type Accum struct {
    UTimeSeconds, STimeSeconds float64
    MinorFaults, MajorFaults   uint64
    VolCtxSw, InvolCtxSw       uint64
    ReadBytes, WriteBytes      uint64
    LastSeen                   time.Time
}

type Sample struct {
    Key Key

    NumProcs, Threads                            int
    RSSBytes, VSizeBytes, SharedBytes, DataBytes uint64
    OpenFDs, MaxFDs                              int
    OldestStart                                  float64
    StateNames                                   map[string]int

    Accum Accum

    FDsRead, IORead int   // coverage
}
```

---

# Part 5 — Scanning

## 5.1 The scan cycle

```
1.  gen++, scanNo++
2.  ListPIDs from /proc
3.  for each batch of batch_size PIDs:
      for each PID:
        read stat                          (always)
        lookup state, validate start time
        read cmdline                       (if filter or namer needs it)
        read status                        (if configured or user unknown)
        apply filter → skip and Touch if ignored
        resolve name                       (cached, or namer)
        read statm                         (always)
        read io                            (if configured and permitted)
        read fd/ and limits                (if this is an fd scan)
        compute deltas against state
        store new state
      sleep batch_sleep
4.  Prune state entries older than gen
5.  measure self CPU delta
6.  Aggregate into snapshot
7.  Publish by atomic pointer swap
```

## 5.2 Read costs and frequencies

| File | Contains | Cost | Frequency |
|---|---|---|---|
| `stat` | CPU ticks, state, threads, start time, faults, PPID, VSize, RSS | One small read | Every scan |
| `statm` | RSS, VSize, shared, data, in pages | One small read | Every scan |
| `status` | UID, context switches | One read, ~50 lines | Every scan if `read_status` |
| `cmdline` | argv | One small read | Only when uncached or the filter needs it |
| `exe` | Executable path | One readlink | Only when deriving a new name |
| `io` | Bytes read and written | One read, needs privilege | Every scan if `read_io` |
| `fd/` | Open descriptors | **Directory walk** | Every `fd_scan_every` scans |
| `limits` | Descriptor limit | One read, ~16 lines | Same as `fd/` |

The cost difference is not marginal. `stat` and `statm` are generated by the kernel from in-memory structures in microseconds. Enumerating `/proc/<pid>/fd/` requires walking the file descriptor table and producing a directory entry for each one; a process holding ten thousand descriptors costs more than a hundred `stat` reads.

**Descriptor counts are cached between fd scans.** Without this the gauge would drop to zero on the three scans out of four that skip the walk, which would look like every process closing all its files. The cached value is reported instead.

## 5.3 Batching and yielding

```yaml
scan:
  interval: 15s
  batch_size: 50
  batch_sleep: 5ms
```

Scan duration is approximately:

```
duration ≈ (procs / batch_size) × batch_sleep + procs × read_cost
```

With 2,000 processes, batch 50, sleep 5ms:

- Sleeping: `(2000 / 50) × 5ms = 200ms`
- Reading: roughly 40 to 100ms at 20 to 50 microseconds per process

Total roughly 250 to 300ms out of a 15-second interval, of which two thirds is sleep. The resulting CPU share is about `100ms / 15s ≈ 0.7%` of one core.

The sleep is not wasted time. It is what stops the scheduler from treating the exporter as a CPU-bound task, which is precisely what keeps it off the top of `top`.

### Tuning relationships

| Change | CPU use | Scan duration | Snapshot coherence |
|---|---|---|---|
| Larger `batch_size` | Higher | Shorter | Better |
| Longer `batch_sleep` | Lower | Longer | Worse |
| Shorter `interval` | Higher | Unchanged | Unchanged |

**Snapshot coherence** is the concern that the first and last processes read are separated by the full scan duration, so the gauge snapshot is smeared across that window. For a 300ms scan this is irrelevant.

Counter accuracy is **not** affected. Each PID's delta uses its own two readings and its own elapsed time. The smear affects the simultaneity of gauges, not the correctness of any rate.

### Overrun protection

If a scan exceeds `interval`, the next scan does not start early or run concurrently. The missed tick is skipped, `process_exporter_scan_overruns_total` increments, and a warning is logged. Two scans never run at once, because both would mutate the state map.

## 5.4 Race tolerance

A process can exit between the directory listing and any file read. This is the normal case at any real turnover rate, not an error.

| Condition | Response |
|---|---|
| `ENOENT` or `ESRCH` on any file | Vanished. Skip silently, count it |
| `EACCES`/`EPERM` on `io` | Set `HaveIO = false`, continue |
| `EACCES` on `fd/` | Set `HaveFDs = false`, continue |
| `EACCES` on `stat` | Cannot proceed. Skip, count as denied |
| Malformed content | Skip that field, continue with the rest |

Nothing here is logged above debug level. An agent that logs on every process exit is worse than useless.

## 5.5 Privilege

The exporter runs at whatever privilege it was given and does not complain.

| Running as | `stat`, `statm`, `status` | `io` | `fd/` |
|---|---|---|---|
| root | All processes | All processes | All processes |
| Unprivileged | All processes | Own only | Own only |

The kernel exposes `stat` and `statm` for every process regardless of ownership, so **CPU, memory, and thread counts are always complete**.

At start, an unprivileged instance logs once:

```
running unprivileged; CPU and memory will be complete, while io and fd
data will cover only processes owned by this user
```

The coverage ratios make it visible in the metrics thereafter.

## 5.6 Parsing detail: the comm field

`/proc/<pid>/stat` wraps the process name in parentheses, and the name may itself contain spaces and parentheses. A naive split on spaces produces wrong field offsets for any such process — and therefore silently wrong CPU numbers.

The parser locates the **final** closing parenthesis and splits only the remainder. Field numbering then follows proc(5) with field 3 at index 0 of the remainder.

Note in the sample output that `process_exporte` appears without its final `r`. `comm` is truncated to fifteen characters by the kernel. This is expected and is why the naming rules exist for cases where the truncated name is insufficient.

---

# Part 6 — Filtering

## 6.1 Purpose

A default Linux machine runs many kernel threads and driver helpers that carry no useful signal:

```
kworker/0:1, kworker/u16:3, ksoftirqd/0, migration/0, rcu_sched,
kthreadd, watchdog/0, kdevtmpfs, kcompactd0, khugepaged, ...
```

Kernel threads have no address space of their own, so their memory readings are meaningless. Their CPU is real but is better observed through `node_exporter`'s system-wide metrics. Their names are numerous and machine-specific — `kworker/u16:3` and `kworker/u16:4` are distinct names — so they are a cardinality problem as well as a noise problem.

## 6.2 Rules and evaluation order

1. **`include_only`** — when non-empty, a process must match one of these patterns to survive. Everything below still applies to what does.
2. **`ignore_kernel_threads`** — empty cmdline, or PPID of 2.
3. **`ignore_comm`** — exact match.
4. **`ignore_comm_prefix`** — prefix match.
5. **`ignore_comm_regex`** — regex match.
6. **`ignore_cmdline_regex`** — regex against the joined command line.
7. **`ignore_users`** — exact match on the resolved user.

An ignored process is counted and its state entry is **stamped rather than dropped**, so it is not rebuilt from scratch on every scan.

## 6.3 The read-order optimisation

`FilterConfig.NeedsCmdline()` reports whether any rule requires the command line. When false, the filter is applied after `stat` and before every other read, so an **ignored process costs exactly one small read**.

A configuration using `ignore_cmdline_regex`, `ignore_kernel_threads`, or `include_only` pays for `cmdline` on every process. This is documented so an operator can choose to avoid it. Note that `ignore_kernel_threads` is true by default, so the default configuration does read cmdline for uncached processes — but the name cache means this is only for processes not seen on the previous scan.

## 6.4 Kernel thread detection

Two signals, either sufficient:

- Empty `/proc/<pid>/cmdline`. Kernel threads have no argv.
- `PPID == 2`, which is `kthreadd`.

Full ancestry walking is not performed: the direct children are the overwhelming majority, and the walk would cost one read per generation for every process on the machine.

## 6.5 Defaults

The shipped configuration includes roughly fifty exact names, eighteen prefixes, and one regex, covering the standard kernel threads on a stock server.

A **nil** list in the configuration means "not mentioned" and gets the defaults. An **explicitly empty** list means "I want nothing", disabling the default for that rule type.

Per the design decision, there is **no cardinality limit and no overflow bucket**. The ignore list is the control.

---

# Part 7 — Group naming

## 7.1 The problem

A process name is not one unambiguous value:

| Source | Java service | Python service |
|---|---|---|
| `comm` | `java` | `python3` |
| `cmdline[0]` | `/usr/lib/jvm/java-17/bin/java` | `/usr/bin/python3` |
| basename | `java` | `python3` |

None identifies *which* service. That information is in argv, several arguments in. Grouping on `comm` alone puts every JVM on the machine into one bucket, which destroys the distinction that matters most.

## 7.2 Rule specification

An ordered list. Each rule has a match condition, an optional extraction, and a name template.

```yaml
naming:
  fallback: comm
  resolve_users: true
  rules:
    - match_comm: java
      from: cmdline
      regex: '-jar\s+\S*?([^/\s]+)\.jar'
      name: 'java/$1'

    - match_comm: java
      from: cmdline
      regex: '\s([a-zA-Z][a-zA-Z0-9_]*(?:\.[a-zA-Z0-9_]+)*\.[A-Z][a-zA-Z0-9_]*)(?:\s|$)'
      name: 'java/$1'

    - match_comm_prefix: python
      from: cmdline
      regex: '([^/\s]+)\.py'
      name: 'python/$1'
```

| Field | Meaning |
|---|---|
| `match_comm` | Exact match against `comm` |
| `match_comm_prefix` | Prefix match |
| `match_comm_regex` | Regex match |
| `match_exe_regex` | Regex against the resolved executable path |
| `from` | Text the extraction runs against: `comm`, `exe`, or `cmdline` |
| `regex` | Extraction pattern with capture groups |
| `name` | Template; `$1`, `$2` reference groups |

## 7.3 The cascade

**A rule that matches but whose extraction fails falls through to the next rule.** This is what makes the two Java rules work as a pair: the first catches `-jar app.jar`, the second catches a bare main class. A JVM invoked either way gets a useful name.

A rule with no `regex` emits its template literally. This is how the container shims collapse: match the prefix `containerd-shim`, discard the container ID that follows, emit a constant. Without it, every container would produce its own series.

## 7.4 Fallback

When no rule matches:

| `fallback` | Result |
|---|---|
| `comm` | Kernel short name, truncated to 15 characters |
| `exe_basename` | Basename of the resolved executable |
| `cmdline_basename` | Basename of `cmdline[0]` |

The observed `process_exporte` in the sample output is `comm` truncation in action.

## 7.5 Naming rules are the cardinality control

You declined a hard limit, which is a reasonable position, but the mechanism should be understood.

A rule extracting an unbounded value reintroduces the growth that grouping exists to prevent.

**Anti-pattern:**
```yaml
- match_comm: java
  regex: '-Dinstance=(\S+)'     # instance IDs are per-process
  name: 'java/$1'
```

**Correct:**
```yaml
- match_comm: java
  regex: '-Dservice=(\S+)'      # service names are bounded
  name: 'java/$1'
```

Two metrics form the diagnostic:

- `process_exporter_groups_total` — currently exported groups
- `process_exporter_group_names_seen_total` — distinct names ever observed

A **widening gap** between them means names appear briefly and disappear, which is the signature of an unbounded extraction.

## 7.6 Sanitisation

Derived names are sanitised before use as a label value:

- Truncated to 128 characters.
- Characters outside `[a-zA-Z0-9_./-]` replaced with `_`.
- Leading and trailing underscores trimmed.
- Empty result becomes `unknown`, because a label value must not be empty.

This bounds the memory cost of a rule that accidentally captures a whole command line, and stops a process embedding arbitrary bytes in argv from producing an unqueryable label.

## 7.7 User resolution

The UID comes from `/proc/<pid>/status`, field `Uid:`, first value (the real UID).

Resolution uses a cache built from `/etc/passwd` at start and refreshed on configuration reload. A UID with no entry falls back to `user.LookupId`, then to `uid:NNNN`. Container-created UIDs with no host passwd entry therefore render as `uid:1001` rather than failing.

`resolve_users: false` emits numeric UIDs throughout, saving the lookup.

---

# Part 8 — Aggregation

## 8.1 Gauge aggregation

Summed across live members in the current scan:

| Metric | Aggregation | Note |
|---|---|---|
| `NumProcs` | Count | |
| `Threads` | Sum | |
| `RSSBytes` | Sum | Over-counts shared pages |
| `VSizeBytes` | Sum | Nearly meaningless summed; kept for completeness |
| `SharedBytes` | Sum | |
| `DataBytes` | Sum | Private writable; closest to unshared memory |
| `OpenFDs` | Sum | Only members where `HaveFDs` |
| `MaxFDs` | Sum | Matched to OpenFDs so the ratio is meaningful |
| `OldestStart` | **Minimum** | Group age; changes on restart |
| `States` | Count per state | |

### The RSS caveat

Summing RSS across processes that share pages over-counts. Forty `nginx` workers forked from one master share most of their text and much of their data; the sum exceeds the group's true footprint.

This is documented rather than corrected. Computing proportional set size requires `/proc/<pid>/smaps` or `smaps_rollup`, which is expensive enough to defeat the low-CPU requirement. `DataBytes` is published alongside as the closer approximation.

**A group's RSS total can exceed physical memory.** This is an artefact of the summation, not a bug.

### MaxFDs summation

`MaxFDs` is summed rather than minimised, so that `open_fds / max_fds` is a meaningful ratio for the group. A limit of `unlimited` reads as −1 and is excluded from the sum.

## 8.2 Counter aggregation

Per scan, per group:

```
group_delta = Σ over pids of max(0, now[pid] − prev[pid])
accumulator[group] += group_delta
```

CPU ticks convert to seconds by dividing by `sysconf(_SC_CLK_TCK)`, which is read at start rather than assumed.

A PID with no previous state contributes nothing this scan. Its state is recorded and it contributes from the next scan onward.

## 8.3 Coverage reporting

`FDsRead` and `IORead` count members whose respective files were readable:

```
process_group_fd_coverage_ratio{name, user}
process_group_io_coverage_ratio{name, user}
```

A ratio of 1.0 means the total is complete. A ratio of 0.3 means only 30% of members contributed and the total is a lower bound.

This is the difference between "this group does no I/O" and "I cannot see this group's I/O", which are entirely different facts.

Correspondingly, **descriptor and I/O metrics are omitted rather than zeroed** when nothing was readable. An unprivileged exporter emits no `process_group_open_fds` for a group it cannot see into, and the coverage ratio says why.

---

# Part 9 — Metrics

## 9.1 Labels

Every process-group metric carries exactly two labels: `name` and `user`. No PID, no command line, no container ID, no PPID.

## 9.2 Gauges

| Metric | Unit |
|---|---|
| `process_group_num_procs` | count |
| `process_group_num_threads` | count |
| `process_group_memory_rss_bytes` | bytes |
| `process_group_memory_vsize_bytes` | bytes |
| `process_group_memory_shared_bytes` | bytes |
| `process_group_memory_data_bytes` | bytes |
| `process_group_open_fds` | count |
| `process_group_max_fds` | count |
| `process_group_oldest_start_time_seconds` | unix seconds |
| `process_group_states{state}` | count |
| `process_group_fd_coverage_ratio` | ratio |
| `process_group_io_coverage_ratio` | ratio |

State values are readable names: `running`, `sleeping`, `disk_sleep`, `zombie`, `stopped`, `idle`, and so on.

## 9.3 Counters

| Metric | Unit |
|---|---|
| `process_group_cpu_seconds_total{mode="user"\|"system"}` | seconds |
| `process_group_minor_page_faults_total` | count |
| `process_group_major_page_faults_total` | count |
| `process_group_context_switches_total{kind="voluntary"\|"involuntary"}` | count |
| `process_group_read_bytes_total` | bytes |
| `process_group_write_bytes_total` | bytes |

## 9.4 Exporter self-metrics

| Metric | Type | Meaning |
|---|---|---|
| `process_exporter_scan_duration_seconds` | Histogram | Wall time per scan |
| `process_exporter_scan_cpu_seconds_total` | Counter | **The exporter's own CPU cost** |
| `process_exporter_scans_total` | Counter | Completed scans |
| `process_exporter_scan_overruns_total` | Counter | Scans exceeding the interval |
| `process_exporter_last_scan_timestamp_seconds` | Gauge | Snapshot freshness |
| `process_exporter_procs_total` | Gauge | PIDs seen |
| `process_exporter_procs_scanned` | Gauge | Successfully read |
| `process_exporter_procs_ignored` | Gauge | Matched the ignore list |
| `process_exporter_procs_vanished` | Gauge | Exited mid-scan |
| `process_exporter_procs_denied` | Gauge | Permission denied |
| `process_exporter_groups_total` | Gauge | Distinct groups exported |
| `process_exporter_group_names_seen_total` | Gauge | Distinct names ever seen |
| `process_exporter_state_entries` | Gauge | Per-PID state map size |
| `process_exporter_read_errors_total{file}` | Counter | Genuine read failures |
| `process_exporter_build_info{version,goversion}` | Gauge | Constant 1 |

`process_exporter_scan_cpu_seconds_total` is measured with `getrusage(RUSAGE_SELF)` around each scan. It lets an operator verify the low-cost requirement directly:

```promql
rate(process_exporter_scan_cpu_seconds_total[5m]) < 0.02
```

`process_exporter_read_errors_total` excludes vanished processes and permission denials, both of which are normal. A non-zero value indicates a genuine parsing or filesystem problem.

## 9.5 Collector implementation

The metrics package implements `prometheus.Collector` over the snapshot rather than holding `GaugeVec` instances.

The reason: a group that vanishes is simply absent from the next snapshot and stops being collected. There is no `Delete` call to forget and no stale series to leak. With `GaugeVec` instances, every disappearing group would require an explicit deletion, and a missed one would leave a frozen series forever.

## 9.6 Cardinality

```
series ≈ groups × (12 gauges + 6 counters + state variants)
```

| Machine type | Groups | Approximate series |
|---|---|---|
| Small server | 20 | 400 |
| Typical server | 60 | 1,200 |
| Busy application server | 200 | 4,000 |

Compare against the PID-keyed alternative: 2,000 live processes with complete turnover of short-lived ones produces tens of thousands of series per day and never stops growing.

---

# Part 10 — HTTP API

| Path | Content |
|---|---|
| `/metrics` | Prometheus exposition |
| `/groups` | Current snapshot as JSON, sortable |
| `/stats` | Scan counters and self CPU |
| `/config` | Effective configuration |
| `/livez` | Process is running |
| `/readyz` | At least two scans have completed |
| `/` | Version, uptime, endpoint list |

`/groups` accepts `?sort=` with values `cpu` (default), `rss`, `procs`, `fds`, or `name`. It is the debugging equivalent of `top` and answers "what is this exporter actually seeing" without going through Prometheus.

`/readyz` requires **two** scans, not one, because counter values do not exist until the second scan produces the first deltas. Reporting ready after one scan would expose all-zero counters to a scraper that had just started.

All reads come from the atomic snapshot pointer. No handler ever triggers a scan.

---

# Part 11 — Configuration

```yaml
scan:
  interval: 15s
  batch_size: 50
  batch_sleep: 5ms
  fd_scan_every: 4
  read_io: true
  read_status: true
  cache_cmdline: true
  group_retention: 1h
  proc_path: /proc

filter:
  ignore_kernel_threads: true
  # ignore_comm, ignore_comm_prefix, ignore_comm_regex:
  #   omit for the shipped defaults; set to [] to disable them
  ignore_cmdline_regex: []
  ignore_users: []
  include_only: []

naming:
  fallback: comm            # comm | exe_basename | cmdline_basename
  resolve_users: true
  # rules: omit for the shipped defaults

server:
  listen: "0.0.0.0:9256"
  metrics_path: /metrics

log:
  level: info               # debug | info | warn | error
  format: json              # json | text
```

## 11.1 Tuning table

| Machine | interval | batch_size | batch_sleep | fd_scan_every |
|---|---|---|---|---|
| Small, under 200 procs | 15s | 100 | 2ms | 2 |
| Typical, under 1,000 | 15s | 50 | 5ms | 4 |
| Busy, 1,000 to 5,000 | 30s | 50 | 5ms | 8 |
| Very busy, over 5,000 | 60s | 100 | 10ms | 10 |
| Latency-sensitive host | 60s | 25 | 20ms | 20 |

The last row trades freshness for the smallest possible footprint, for a machine where the exporter must be invisible.

## 11.2 Validation

`Validate()` reports every problem at once, not just the first. It includes a scan-duration estimate:

```
scan.batch_sleep 50ms at batch_size 10 would take about 10s for 2000
processes, which exceeds scan.interval 5s
```

`process_exporter -check -config <path>` validates without starting.

## 11.3 Reload

`SIGHUP` or file modification. A configuration failing validation is rejected and the previous one stays active.

| Change | Effect |
|---|---|
| `scan.*` | Applies from the next scan |
| `filter.*` | Applies from the next scan; newly ignored groups age out via `group_retention` |
| `naming.*` | **Resets accumulators and per-PID name cache.** Every series restarts from zero |
| `server.*` | Requires restart |

A naming change resets **both** the aggregator and the scanner state. The accumulators are keyed on names that no longer exist, and the per-PID cache holds names derived under the old rules. Keeping either would produce wrong output. The warning says so plainly:

```
naming changed; accumulators and cached names discarded, every time
series restarts from zero
```

---

# Part 12 — Operational behaviour

## 12.1 Startup

1. Parse and validate configuration. Failure exits with code 2.
2. Read system constants: clock ticks via `getconf CLK_TCK` (fallback 100), page size, boot time from `/proc/stat` `btime`.
3. Build the UID cache from `/etc/passwd`.
4. Compile the filter and namer. Failure exits with code 3.
5. Log the privilege level and its consequences.
6. Run a **priming scan**. It publishes gauges but no counter deltas.
7. Start the scan loop, the config watcher, and the HTTP server.

Between the priming scan and the second scan, `/metrics` shows gauges with zero counters. `process_exporter_scans_total` reading 1 tells an operator why, and `/readyz` returns 503.

## 12.2 Shutdown

`SIGINT` or `SIGTERM`. The HTTP server stops accepting, the scanner finishes its current batch and exits, all goroutines are awaited.

**No state is persisted.** A restart resets every counter and Prometheus sees a reset. This is correct and expected for a process exporter, matching `node_exporter` and every other agent of this class. `rate()` handles it.

## 12.3 Failure modes

| Symptom | Cause | Diagnostic |
|---|---|---|
| All counters zero | Only one scan has run | `process_exporter_scans_total` |
| I/O metrics absent | Not root; `io` unreadable | `process_group_io_coverage_ratio` |
| fd metrics absent | Not root, or `fd_scan_every` not yet reached | `process_group_fd_coverage_ratio` |
| Group count growing steadily | A naming rule extracts an unbounded value | Gap between `groups_total` and `names_seen_total` |
| High exporter CPU | `batch_sleep` too short or `interval` too short | `process_exporter_scan_cpu_seconds_total` |
| Stale metrics | Scans overrunning | `process_exporter_scan_overruns_total` |
| Many processes ignored | Filter too broad | `process_exporter_procs_ignored` |
| RSS exceeds physical memory | Shared pages counted per member | Expected; see 8.1 |
| Truncated names like `process_exporte` | `comm` is 15 characters | Expected; add a naming rule if it matters |

---

# Part 13 — Prometheus usage

## 13.1 Scrape

```yaml
scrape_configs:
  - job_name: process
    scrape_interval: 30s
    static_configs:
      - targets: ['host:9256']
```

Scrape interval should be at or above `scan.interval`. Scraping faster returns the same snapshot repeatedly, which is harmless but pointless.

## 13.2 Queries

```promql
# CPU by group
topk(10, sum by (name, user) (rate(process_group_cpu_seconds_total[5m])))

# Memory by group
topk(10, process_group_memory_rss_bytes)

# Descriptor pressure
process_group_open_fds / process_group_max_fds > 0.8

# Process count change: leaks or crash loops
delta(process_group_num_procs[1h]) != 0

# Group restarted
changes(process_group_oldest_start_time_seconds[1h]) > 0

# Blocked on I/O
process_group_states{state="disk_sleep"} > 0

# Zombies
process_group_states{state="zombie"} > 5

# The self-check
rate(process_exporter_scan_cpu_seconds_total[5m])
```

## 13.3 Alerting

```yaml
groups:
  - name: process_exporter
    rules:
      - alert: ProcessGroupFDExhaustion
        expr: process_group_open_fds / process_group_max_fds > 0.9
        for: 5m

      - alert: ProcessGroupGone
        expr: process_group_num_procs == 0
        for: 5m

      - alert: ProcessExporterExpensive
        expr: rate(process_exporter_scan_cpu_seconds_total[5m]) > 0.05
        for: 15m
        annotations:
          summary: "over 5% of a core; check scan tuning"

      - alert: ProcessExporterStale
        expr: time() - process_exporter_last_scan_timestamp_seconds > 120
        for: 5m

      - alert: ProcessExporterCardinality
        expr: |
          process_exporter_group_names_seen_total
            - process_exporter_groups_total > 200
        for: 30m
        annotations:
          summary: "name churn; a naming rule is extracting an unbounded value"
```

---

# Part 14 — Design decisions recorded

| Decision | Choice | Reason |
|---|---|---|
| Key | Name plus user, no PID | PID is unbounded; name is bounded by software inventory; user separates masters from workers |
| Cardinality cap | None | The ignore list and naming rules are the control |
| Counter model | Per-group accumulator fed by per-PID deltas | Summing lifetime totals drops on exit and reads as a reset |
| PID reuse defence | Start time validation plus zero clamp | Two independent checks |
| State pruning | Generation stamp | Map size tracks live processes, not history |
| Accumulator pruning | `group_retention`, default 1h | A restarting service resumes its counter |
| Scan model | Own timer; scrapes read a snapshot | Cost independent of scrape count and frequency |
| Yield model | Batch plus sleep | Keeps the scheduler from treating the exporter as CPU-bound |
| fd frequency | Every Nth scan, cached between | Directory walk dominates; caching stops the gauge dropping |
| Name caching | Per PID, keyed on start time | Removes a read and an evaluation per process per scan |
| Kernel threads | Ignored by default | No address space, meaningless memory, unbounded names |
| Privilege | Whatever it was given | CPU and memory always complete; io and fd coverage reported as a ratio |
| Absent data | Metric omitted, not zeroed | Zero and unknown are different facts |
| Threads | Count only | Per-thread would multiply cardinality for little gain |
| RSS sharing | Not corrected | `smaps` defeats the low-CPU requirement; `DataBytes` published alongside |
| Metrics implementation | `prometheus.Collector` | A vanished group stops being collected with no explicit cleanup |
| Persistence | None | Restart resets counters; correct for this class of agent |
| Readiness | Two scans | Counters do not exist until the second scan |

---

# Part 15 — Reading the code

Suggested order:

1. **`internal/scan/state.go`** — `PIDState` and `StateMap`. The generation stamp and the start-time check are the whole basis of correct counters. Short file.
2. **`internal/scan/scan.go`** — `readProcess`. Read the order of operations: which files are read, when the filter is applied, and where the name cache short-circuits.
3. **`internal/aggregate/aggregate.go`** — `Apply` and `accumulate`. The accumulator is what keeps counters monotonic.
4. **`internal/namer/namer.go`** — `Name` and the fall-through cascade.
5. **`internal/metrics/metrics.go`** — `groupCollector.Collect`. Note which metrics are conditionally emitted.
6. **`cmd/process_exporter/main.go`** — the scan loop and overrun handling.

The `internal/procfs` package is a set of independent parsers and can be read in any order or skipped.

---

# Part 16 — Known limitations

| Item | Status |
|---|---|
| RSS over-counting | Shared pages counted per member. `smaps` would fix it at a cost this design cannot pay |
| argv rewriting | A process changing its own argv keeps its cached name until restart. Set `cache_cmdline: false` to disable |
| `comm` truncation | Fifteen characters, kernel-imposed. Naming rules are the workaround |
| Container awareness | None. A containerised process is named by its binary like any other |
| Per-thread data | Not collected. Thread count only |
| Counter persistence | None. Restart resets every counter |
| `getconf` dependency | `clockTicks()` shells out once at start; falls back to 100 |
| Cgroup metrics | Not collected. `cadvisor` is the right tool |
