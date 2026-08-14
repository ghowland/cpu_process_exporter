# process_exporter — Technical Specification

**Version:** 1.0 (design)
**Binary:** `process_exporter`
**Language:** Go 1.22
**Target:** Linux only

---

# Part 1 — What this system is

## 1.1 Purpose

A Prometheus exporter that reads `/proc` on a fixed interval, groups every running process by name and owning user, sums each metric across the members of a group, and publishes the group totals.

It answers questions of this form:

- How much resident memory is `nginx` using on this box, in total across all its workers?
- Is the `postgres` process group approaching its file descriptor limit?
- Which process group's CPU consumption grew after last night's deploy?
- How many `python3` processes is the `deploy` user running, and how many is `root` running?

## 1.2 The unit of measurement

The unit is the **process group**, not the process. A group is every process sharing a name and an owning user. Forty `nginx` workers owned by `www-data` produce one series per metric, carrying the sum across all forty, plus a count of how many there were.

Individual processes are never visible in the output. They are read, aggregated, and discarded within one scan. **No PID appears anywhere in any label.**

## 1.3 Two constraints that shape everything

### Constraint 1: bounded cardinality

A Prometheus time series is identified by its label set. If PID were a label, every process start would create a new series. A machine that forks a short-lived process every second creates 86,400 series per day, each living in memory until it ages out. The series count becomes unbounded and driven by workload behaviour rather than by anything the operator chose.

The process name is bounded. A machine runs some finite set of programs. That set changes when software is installed, not when a program runs. Keying on name makes the series count a property of the machine's software inventory, which is stable and small.

Adding the owning user multiplies this by a small, also-bounded factor, and it earns its keep: knowing that one `nginx` is owned by `root` and forty by `www-data` is the master-and-workers structure made visible.

### Constraint 2: negligible CPU cost

The exporter competes for the resource it measures. A scan that reads two thousand `/proc` entries as fast as it can appears in `top` as a significant consumer, which is unacceptable for a monitoring agent.

The exporter must not appear near the top of `top`. This is achieved by breaking the scan into batches with sleeps between them, by separating cheap reads from expensive ones and running them at different frequencies, and by running as a long-lived daemon so that no process startup cost is paid per scrape.

---

# Part 2 — Core mechanics

Read this section before any other.

## 2.1 Gauges are read; counters are accumulated

`/proc` exposes two kinds of value, and they need completely different handling.

**Gauges** are instantaneous. Resident memory is a page count right now. Open file descriptors is a directory entry count right now. Thread count is a number in `stat` right now. Read it, sum it across the group, publish it. One read is sufficient.

**Counters** are monotonic totals since process start. CPU is ticks consumed since the process began. Page faults, context switches, and I/O bytes are the same shape. A single read gives a lifetime total, which is not what anyone wants to see.

## 2.2 Why counters need per-PID state

The useful value from a counter is the rate, which needs two reads separated by a known interval:

```
delta = ticks_now - ticks_previous
```

This requires the exporter to remember the previous value for each process. Three consequences follow, and they are the heart of the design:

**State is per-PID, even though output is per-group.** A counter belongs to one process. Two processes named `nginx` have separate counters that both increase independently.

**The delta must be computed per PID, then summed.** Summing the raw lifetime counters across a group and differencing the sum produces a wrong answer the moment one process exits: the sum drops discontinuously, and the difference goes negative. Per-PID first, sum second.

**The first scan produces no counter deltas.** There is no previous reading. The first scan establishes the baseline; the second produces the first usable delta.

## 2.3 Why the published counter is an accumulator

A Prometheus counter must increase monotonically within a series. Prometheus treats any decrease as a counter reset and discards the interval.

If the exporter published a group's CPU counter as the sum of its live members' lifetime totals, the value would drop every time a worker exited. Prometheus would see a reset. The data would be wrong.

The correct construction is an **accumulator per group**. Each scan adds the sum of that group's per-PID deltas to the accumulator:

```
accumulator[group] += Σ (ticks_now[pid] - ticks_prev[pid])   over pids in group
```

The accumulator only ever increases, regardless of process churn. A process exiting simply stops contributing new deltas. The accumulator is what is published; the per-PID counters are internal working values that never leave the scan.

## 2.4 PID reuse

The kernel wraps PIDs. An old state entry can be matched against a new process holding the same number. The new process's counter starts near zero, so the delta computes as a large negative number.

The defence is the process start time. `/proc/<pid>/stat` field 22 gives the start time in clock ticks since boot. It is fixed for the life of a process and effectively unique in combination with the PID.

Each state entry stores the start time. On each scan:

- Start time matches → same process, compute the delta.
- Start time differs → different process reusing the PID. Discard the old entry, treat this as a new process, produce no delta this scan.

Additionally, any negative delta from any source is discarded rather than published. This is a second line of defence against a start-time read that failed.

## 2.5 State pruning

The per-PID state map grows every time a new process appears. Without pruning it grows without bound.

Each scan increments a **generation counter** and stamps every PID it observes with the current value. At the end of the scan, entries whose stamp is older than the current generation belong to processes that no longer exist and are deleted.

The map size therefore tracks the number of live processes, not the number that have ever existed. This is what makes the exporter safe to run for months without restart.

## 2.6 Group accumulator lifetime

Group accumulators must not be pruned on the same schedule as PID state. A group whose processes all exit and then restart minutes later must resume its counter from where it left off, or Prometheus sees a reset.

Accumulators are kept for `group_retention` (default 1h) after the group was last seen. During that window the group emits no gauge series — it has no live members — but its accumulator survives. Beyond the window the group is forgotten entirely, on the assumption that the program has been uninstalled or permanently stopped.

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
  │   Filter    │    │  Namer       │    │  Scheduler   │
  │ ignore list │    │ group naming │    │ interval,    │
  │             │    │ rules        │    │ batch, sleep │
  └──────┬──────┘    └──────┬───────┘    └──────┬───────┘
         │                  │                   │
         └──────────┬───────┴───────────────────┘
                    v
            ┌───────────────┐        ┌──────────────────┐
            │   Scanner     │<──────>│  PID State Map   │
            │ walks /proc   │        │ prev counters,   │
            │ in batches    │        │ start times, gen │
            └───────┬───────┘        └──────────────────┘
                    │ per-process readings
                    v
            ┌───────────────┐        ┌──────────────────┐
            │  Aggregator   │<──────>│ Group Accumulator│
            │ sum gauges,   │        │ monotonic totals │
            │ add deltas    │        │ per group key    │
            └───────┬───────┘        └──────────────────┘
                    │ group snapshot
                    v
            ┌───────────────┐
            │   Registry    │  atomic pointer swap
            └───────┬───────┘
                    v
            ┌───────────────┐
            │  HTTP /metrics│  reads last snapshot, never scans
            └───────────────┘
```

## 3.2 The decoupling rule

**A scrape never triggers a scan.** The scanner runs on its own timer. A scrape reads the most recent completed snapshot through an atomic pointer load.

This is deliberate:

- Scrape cost is constant and tiny — a serialisation of an in-memory structure.
- Scan cost is independent of scrape frequency. Ten Prometheus servers scraping does not multiply the cost by ten.
- A slow scan cannot cause a scrape timeout.
- The exporter's own CPU use is a function of its configuration alone, which is what makes it predictable.

The published snapshot carries the timestamp of the scan that produced it, exposed as `process_exporter_last_scan_timestamp_seconds` so that staleness is visible.

## 3.3 Package layout

```
build.sh
go.mod

cmd/process_exporter/main.go     wiring, lifecycle, flags

internal/config/config.go        structs, defaults, validation
internal/config/watcher.go       SIGHUP and file-change reload

internal/procfs/procfs.go        low-level /proc readers, one per file
internal/procfs/stat.go          /proc/<pid>/stat parser
internal/procfs/statm.go         /proc/<pid>/statm parser
internal/procfs/status.go        /proc/<pid>/status parser
internal/procfs/io.go            /proc/<pid>/io parser
internal/procfs/fd.go            fd count and limit
internal/procfs/cmdline.go       cmdline reader
internal/procfs/system.go        boot time, clock ticks, page size, uid to name

internal/filter/filter.go        ignore list matching
internal/namer/namer.go          group name derivation rules

internal/scan/scan.go            batched walk, yields, per-process assembly
internal/scan/state.go           per-PID state map, generation pruning

internal/aggregate/aggregate.go  gauge sums, counter accumulators, group snapshot

internal/metrics/metrics.go      Prometheus registry and definitions
internal/api/api.go              HTTP handlers
```

---

# Part 4 — Data model

## 4.1 Group key

```go
// GroupKey identifies one exported series set. It contains no PID, and
// its cardinality is bounded by the machine's software inventory rather
// than by process churn.
type GroupKey struct {
    Name string // derived by the namer
    User string // owning user name, or the numeric UID if unresolvable
}
```

Two labels. Nothing else. Every metric in the exporter carries exactly this pair.

## 4.2 Per-process reading

Assembled fresh on every scan, consumed by the aggregator, discarded.

```go
// Process is one process as read in one scan. It is a working value and
// never leaves the scan.
type Process struct {
    PID       int
    PPID      int
    Comm      string   // kernel short name, max 15 chars
    Exe       string   // resolved executable path, may be empty
    Cmdline   []string // argv, may be empty for kernel threads
    UID       uint32
    User      string
    State     byte     // R, S, D, Z, T, ...
    StartTicks uint64  // stat field 22, identity anchor for PID reuse

    // Gauges — instantaneous, read fresh each scan
    RSSBytes    uint64
    VSizeBytes  uint64
    SharedBytes uint64
    DataBytes   uint64
    Threads     int
    OpenFDs     int    // -1 when not read this scan or not permitted
    MaxFDs      int    // -1 when unknown
    OldestStart float64 // unix seconds

    // Counters — lifetime totals, differenced against state
    UTimeTicks  uint64
    STimeTicks  uint64
    MinorFaults uint64
    MajorFaults uint64
    VolCtxSw    uint64
    InvolCtxSw  uint64
    ReadBytes   uint64
    WriteBytes  uint64

    // Read status, so that "unknown" is distinguishable from "zero"
    HaveIO     bool
    HaveStatus bool
    HaveFDs    bool
}
```

The three `Have*` fields matter. An unprivileged exporter cannot read `/proc/<pid>/io` for processes it does not own. Reporting zero would be a lie. The aggregator skips the metric entirely for that process, and the group's I/O total reflects only the processes it could read.

## 4.3 Per-PID state

```go
// PIDState is the memory between scans that makes counter deltas
// possible. It is keyed by PID and validated by StartTicks.
type PIDState struct {
    StartTicks uint64 // must match, or the PID was reused
    Generation uint64 // scan number when last seen; older means gone

    UTimeTicks  uint64
    STimeTicks  uint64
    MinorFaults uint64
    MajorFaults uint64
    VolCtxSw    uint64
    InvolCtxSw  uint64
    ReadBytes   uint64
    WriteBytes  uint64
}
```

Eight counters plus two identity fields. Roughly 80 bytes per process. Two thousand processes costs 160 KB, which is negligible.

## 4.4 Group accumulator

```go
// GroupAccum holds the monotonic totals for one group. It survives the
// disappearance of every member process, so that a restarted service
// does not reset its counters.
type GroupAccum struct {
    UTimeSeconds float64
    STimeSeconds float64
    MinorFaults  uint64
    MajorFaults  uint64
    VolCtxSw     uint64
    InvolCtxSw   uint64
    ReadBytes    uint64
    WriteBytes   uint64

    LastSeen time.Time // for group_retention pruning
}
```

## 4.5 Snapshot

```go
// Snapshot is one complete scan result, published atomically.
type Snapshot struct {
    Groups    map[GroupKey]*GroupSample
    ScanAt    time.Time
    Duration  time.Duration
    Generation uint64

    ProcsTotal    int // seen in /proc
    ProcsScanned  int // successfully read
    ProcsIgnored  int // matched the ignore list
    ProcsVanished int // exited mid-scan
    ProcsDenied   int // permission denied on a required file
}

// GroupSample is the exported state of one group at one instant.
type GroupSample struct {
    Key GroupKey

    // Gauges, summed across live members
    NumProcs    int
    Threads     int
    RSSBytes    uint64
    VSizeBytes  uint64
    SharedBytes uint64
    DataBytes   uint64
    OpenFDs     int
    MaxFDs      int
    OldestStart float64
    States      map[byte]int

    // Counters, copied from the accumulator
    Accum GroupAccum

    // Coverage, so partial data is visible rather than silent
    FDsRead int // members whose fd count was read this scan
    IORead  int // members whose io file was readable
}
```

---

# Part 5 — Scanning

## 5.1 The scan cycle

```
1.  gen++
2.  list /proc, collect numeric directory names as PIDs
3.  for each batch of `batch_size` PIDs:
      a.  for each PID in batch:
            read cheap files
            apply ignore filter
            derive group name and user
            read expensive files if this scan is an fd-scan
            look up state, validate start time, compute deltas
            accumulate into group
            update state, stamp generation
      b.  sleep `batch_sleep`
4.  prune state entries with generation < gen
5.  prune group accumulators older than `group_retention`
6.  build snapshot, publish by atomic pointer swap
```

## 5.2 Cheap and expensive reads

| File | Contains | Cost | Frequency |
|---|---|---|---|
| `/proc/<pid>/stat` | CPU ticks, state, threads, start time, faults, PPID | One small read, one parse | Every scan |
| `/proc/<pid>/statm` | RSS, VSize, shared, data, in pages | One small read | Every scan |
| `/proc/<pid>/cmdline` | argv, NUL-separated | One small read | Every scan (see 5.3) |
| `/proc/<pid>/status` | UID, context switches | One read, ~50 lines | Every scan |
| `/proc/<pid>/io` | Bytes read and written | One read, needs privilege | Every scan, if permitted |
| `/proc/<pid>/fd/` | Open descriptors | **Directory walk** | Every `fd_scan_every` scans |
| `/proc/<pid>/limits` | Descriptor limit | One read, ~16 lines | Same as fd |

The cost difference is not marginal. `stat` and `statm` are generated by the kernel from in-memory structures in microseconds. Enumerating `/proc/<pid>/fd/` requires walking the file descriptor table and producing a directory entry for each one. A process holding ten thousand descriptors makes that one read cost more than a hundred `stat` reads.

This is why fd counting has its own frequency. Open file counts change slowly; a sixty-second refresh is adequate. CPU needs a fifteen-second refresh to be useful.

## 5.3 Caching immutable values

Several values never change for the life of a process:

| Value | Source | Cacheable |
|---|---|---|
| `cmdline` | `/proc/<pid>/cmdline` | Yes, almost always |
| `exe` | `/proc/<pid>/exe` symlink | Yes |
| UID | `/proc/<pid>/status` | Practically yes |
| Start time | `/proc/<pid>/stat` | Yes, by definition |
| Derived group name | Namer output | Yes, follows from the above |

The derived group name is cached in the PID state entry. On subsequent scans, if the start time matches, the cached name and user are reused and `cmdline` is not read at all. This removes one read and one namer evaluation per process per scan for every long-lived process, which is the large majority.

A process can rewrite its own argv, which some servers do to show status. The cache means such changes are not picked up until the process restarts. This is an accepted trade: re-reading cmdline every scan for every process to catch a rare in-place rename is not worth the cost.

## 5.4 Batching and yielding

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

```
(2000 / 50) × 5ms = 200ms of sleeping
```

Plus the actual read time, which for cheap reads is on the order of 20 to 50 microseconds per process, so 40 to 100ms. Total scan time roughly 250 to 300ms out of a 15-second interval, of which two thirds is sleep.

The resulting CPU share is approximately `100ms / 15s ≈ 0.7%` of one core.

### The tuning relationship

| Parameter change | CPU use | Scan duration | Snapshot coherence |
|---|---|---|---|
| Larger `batch_size` | Higher | Shorter | Better |
| Longer `batch_sleep` | Lower | Longer | Worse |
| Shorter `interval` | Higher | Unchanged | Unchanged |

**Snapshot coherence** is the concern that the first process read and the last are separated by the full scan duration, so the gauge snapshot is smeared across that window. For a 300ms scan this is irrelevant.

Note that counter accuracy is **not** affected by the smear. Each PID's delta is computed against its own previous reading with its own elapsed time. The smear affects the simultaneity of the gauges, not the correctness of any rate.

### Overrun protection

If a scan takes longer than `interval`, the next scan does not start early or run concurrently. The scheduler skips the missed tick, increments `process_exporter_scan_overruns_total`, and logs a warning. Two scans never run at once, because they would both mutate the state map.

## 5.5 Race tolerance

A process can exit between the directory listing and the file read. This is the normal case at any real process turnover rate, not an error.

| Condition | Response |
|---|---|
| `ENOENT` opening any file | Process vanished. Skip silently, increment `ProcsVanished` |
| `ESRCH` | Same |
| `EACCES` or `EPERM` on `io` | Not permitted. Set `HaveIO = false`, continue |
| `EACCES` on `fd/` | Set `HaveFDs = false`, continue |
| `EACCES` on `stat` or `statm` | Cannot proceed. Skip, increment `ProcsDenied` |
| Malformed content | Skip the field, continue with the rest |

Nothing here is logged at anything above debug level. A monitoring agent that logs on every process exit is worse than useless.

## 5.6 Privilege

The exporter runs at whatever privilege it was given and does not complain.

| Running as | `stat`, `statm`, `status` | `io` | `fd/` |
|---|---|---|---|
| root | All processes | All processes | All processes |
| Unprivileged | All processes | Own processes only | Own processes only |

The kernel exposes `stat` and `statm` for every process regardless of ownership, so CPU, memory, and thread counts are always complete.

`io` and `fd/` require matching UID or `CAP_SYS_PTRACE`. An unprivileged exporter gets them for its own processes only.

The coverage counters `FDsRead` and `IORead` make this visible in the metrics, so an operator can tell partial data from zero data.

Running as root gives complete data. Running unprivileged gives complete CPU and memory data and partial I/O and descriptor data. Both are supported and neither produces an error.

---

# Part 6 — Filtering

## 6.1 Purpose

A default Linux machine runs a large number of kernel threads and system processes that carry no useful signal:

```
kworker/0:1, kworker/u16:3, ksoftirqd/0, migration/0, rcu_sched,
kthreadd, watchdog/0, kdevtmpfs, kcompactd0, khugepaged, ...
```

Kernel threads have no address space of their own, so their memory readings are meaningless. Their CPU is real but is better observed through `node_exporter`'s system-wide metrics. Their names are numerous and machine-specific — `kworker/u16:3` is a distinct name from `kworker/u16:4` — so they are a cardinality problem as well as a noise problem.

The ignore list removes them.

## 6.2 Rule types

```yaml
filter:
  ignore_kernel_threads: true

  ignore_comm:
    - kthreadd
    - ksoftirqd
    - migration
    - rcu_sched
    - rcu_bh
    - watchdog
    - kdevtmpfs
    - kcompactd0
    - khugepaged
    - kswapd0
    - kauditd
    - oom_reaper
    - writeback
    - kblockd
    - kintegrityd
    - md
    - edac-poller
    - devfreq_wq
    - watchdogd
    - kthrotld
    - ipv6_addrconf
    - kstrp
    - charger_manager
    - scsi_eh
    - scsi_tmf
    - jbd2
    - ext4-rsv-conver

  ignore_comm_prefix:
    - "kworker/"
    - "ksoftirqd/"
    - "migration/"
    - "watchdog/"
    - "irq/"
    - "cpuhp/"
    - "idle_inject/"
    - "scsi_eh_"
    - "scsi_tmf_"
    - "nfsd"
    - "loop"

  ignore_comm_regex:
    - "^kworker/u?[0-9]+:[0-9]+"

  ignore_cmdline_regex: []

  ignore_users: []

  include_only: []
```

### Evaluation order

1. If `include_only` is non-empty, the process must match at least one of its patterns or it is dropped. Everything below still applies to what survives.
2. `ignore_kernel_threads` — dropped if a kernel thread (see 6.3).
3. `ignore_comm` — exact match against `comm`.
4. `ignore_comm_prefix` — prefix match against `comm`.
5. `ignore_comm_regex` — regex match against `comm`.
6. `ignore_cmdline_regex` — regex match against the joined command line.
7. `ignore_users` — exact match against the resolved user name.

A dropped process is counted in `ProcsIgnored` and is not read further, which saves the expensive reads.

The filter is applied after `stat` and before `cmdline`, `status`, `io`, and `fd/`, so that an ignored process costs one small read rather than five reads and a directory walk. `ignore_cmdline_regex` is the exception: matching against the command line requires reading it, so a configuration using that rule pays for `cmdline` on every process. This is documented so an operator can choose to avoid it.

## 6.3 Kernel thread detection

Two signals, either sufficient:

- `/proc/<pid>/cmdline` is empty. Kernel threads have no argv.
- The process's ancestry reaches PID 2 (`kthreadd`). Checking `PPID == 2` catches the direct children, which is the majority; full ancestry walking is not worth the cost.

With `ignore_kernel_threads: true`, the empty-cmdline test alone removes essentially all of them. The explicit `ignore_comm` and prefix lists are belt and braces, and are also useful when kernel thread filtering is turned off for a specific investigation.

## 6.4 Defaults

The shipped default configuration includes the full list above. It is intended to be adequate on a stock Linux server without modification, and to be extended by the operator for site-specific noise.

Per your decision, there is **no cardinality limit and no overflow bucket**. The ignore list is the control. If it is misconfigured, the output is noisy — which is visible, diagnosable, and the operator's choice.

---

# Part 7 — Group naming

## 7.1 The problem

A process name is not one unambiguous value. Three candidates exist and they disagree:

| Source | Value for a Java service | Value for a Python service |
|---|---|---|
| `comm` from `stat` | `java` | `python3` |
| `cmdline[0]` | `/usr/lib/jvm/java-17/bin/java` | `/usr/bin/python3` |
| basename of `cmdline[0]` | `java` | `python3` |

None of these identifies *which* Java service. That information is in argv, several arguments in.

Grouping on `comm` alone puts every JVM on the machine into one bucket, which destroys the distinction that matters most.

## 7.2 Rule specification

An ordered list of rules. The first match wins. If none matches, the fallback applies.

```yaml
naming:
  fallback: comm          # comm | exe_basename | cmdline_basename

  rules:
    # Java: name by the main class or the jar file
    - match_comm: java
      from: cmdline
      regex: "-jar\\s+\\S*/([^/\\s]+)\\.jar"
      name: "java/$1"

    - match_comm: java
      from: cmdline
      regex: "\\s([a-zA-Z][a-zA-Z0-9_.]*\\.[A-Z][a-zA-Z0-9_]*)(?:\\s|$)"
      name: "java/$1"

    # Python: name by the script
    - match_comm_prefix: python
      from: cmdline
      regex: "([^/\\s]+)\\.py"
      name: "python/$1"

    # Node: name by the script
    - match_comm: node
      from: cmdline
      regex: "([^/\\s]+)\\.js"
      name: "node/$1"

    # systemd user sessions collapse to one group
    - match_comm: systemd
      from: cmdline
      regex: "--user"
      name: "systemd-user"

    # Container runtime shims: strip the container ID
    - match_comm_prefix: "containerd-shim"
      name: "containerd-shim"
```

### Rule fields

| Field | Meaning |
|---|---|
| `match_comm` | Exact match against `comm` |
| `match_comm_prefix` | Prefix match against `comm` |
| `match_comm_regex` | Regex match against `comm` |
| `match_exe_regex` | Regex match against the resolved executable path |
| `from` | Which text the extraction regex runs against: `comm`, `exe`, or `cmdline` |
| `regex` | Extraction pattern with capture groups |
| `name` | Output template; `$1`, `$2` reference capture groups |

If a rule matches but its extraction regex does not, evaluation continues to the next rule. This lets the two Java rules above act as a jar-first, then main-class, cascade.

If `regex` is absent, `name` is used literally. This is the `containerd-shim` case: match the prefix, discard everything else, emit a constant.

## 7.3 The naming rule is the cardinality control

You declined a hard limit, and that is a reasonable position, but the mechanism should be understood:

A rule that extracts too much detail reintroduces the unbounded growth that grouping was meant to prevent. A rule producing a name containing a PID, a UUID, a timestamp, a port number, or a container ID creates one series per process instance.

**Anti-pattern:**
```yaml
- match_comm: java
  from: cmdline
  regex: "-Dinstance=(\\S+)"     # instance IDs are per-process
  name: "java/$1"
```

**Correct:**
```yaml
- match_comm: java
  from: cmdline
  regex: "-Dservice=(\\S+)"      # service names are bounded
  name: "java/$1"
```

The diagnostic is `process_exporter_groups_total`. If it grows steadily rather than sitting flat, a naming rule is extracting something unbounded.

The exporter also exposes `process_exporter_group_names_seen_total`, a counter of distinct names ever observed. A large gap between the two indicates churn: many names appear briefly and disappear.

## 7.4 Name sanitisation

Derived names are sanitised before use as a label value:

- Truncate to 128 characters.
- Replace any character outside `[a-zA-Z0-9_./-]` with `_`.
- If the result is empty, fall back to `comm`; if `comm` is also empty, use `unknown`.

## 7.5 User resolution

The UID comes from `/proc/<pid>/status`, field `Uid:`, first value (the real UID).

Resolution to a name uses a cache built at start from `/etc/passwd` and refreshed on the same interval as configuration reload. A UID with no entry is rendered as its number in a fixed form: `uid:1001`. This keeps container-created UIDs, which have no passwd entry on the host, from becoming a resolution failure.

```yaml
naming:
  resolve_users: true    # false emits numeric UIDs, saving the lookup
```

---

# Part 8 — Aggregation

## 8.1 Gauge aggregation

Summed across live members of the group in the current scan:

| Metric | Aggregation | Note |
|---|---|---|
| `NumProcs` | Count | Group membership |
| `Threads` | Sum | |
| `RSSBytes` | Sum | Over-counts shared pages between members |
| `VSizeBytes` | Sum | Nearly meaningless summed; kept for completeness |
| `SharedBytes` | Sum | |
| `DataBytes` | Sum | Private writable, the closest to "real" memory |
| `OpenFDs` | Sum | Only over members where `HaveFDs` |
| `MaxFDs` | Sum | Sum matches OpenFDs so a ratio is meaningful |
| `OldestStart` | **Minimum** | Group age; when the oldest member started |
| `States` | Count per state letter | |

### The RSS caveat

Summing RSS across processes that share pages over-counts. Forty `nginx` workers forked from one master share most of their text and much of their data. The sum is larger than the group's true footprint.

This is documented rather than corrected. Computing proportional set size requires reading `/proc/<pid>/smaps` or `smaps_rollup`, which is expensive enough to defeat the low-CPU requirement. `DataBytes` (private writable pages) is exported alongside RSS as the closer approximation to unshared memory.

An operator comparing `process_group_memory_rss_bytes` against `node_memory_MemTotal_bytes` must understand that group totals can exceed physical memory. This is an artefact of the summation, not a bug.

## 8.2 Counter aggregation

Per scan, per group:

```
group_delta = Σ over pids in group of max(0, now[pid] - prev[pid])
accumulator[group] += group_delta
```

CPU ticks convert to seconds by dividing by `sysconf(_SC_CLK_TCK)`, which is 100 on essentially all Linux systems but is read at start rather than assumed.

The `max(0, ...)` clamp discards negative deltas from PID reuse that the start-time check missed.

A PID with no previous state contributes nothing this scan. Its state is recorded and it contributes from the next scan onward.

## 8.3 Coverage reporting

`FDsRead` and `IORead` count how many members of the group had those files successfully read. Exported as:

```
process_group_fd_coverage_ratio{name, user}
process_group_io_coverage_ratio{name, user}
```

A ratio of 1.0 means the group's descriptor or I/O totals are complete. A ratio of 0.3 means only 30% of members contributed and the total is a lower bound. This is the difference between "this group does no I/O" and "I cannot see this group's I/O", which are entirely different facts.

---

# Part 9 — Metrics

## 9.1 Labels

Every process-group metric carries exactly two labels:

```
name, user
```

No PID. No command line. No container ID. No PPID.

## 9.2 Gauges

| Metric | Unit | Description |
|---|---|---|
| `process_group_num_procs` | count | Live processes in the group |
| `process_group_num_threads` | count | Total threads |
| `process_group_memory_rss_bytes` | bytes | Resident set, summed |
| `process_group_memory_vsize_bytes` | bytes | Virtual size, summed |
| `process_group_memory_shared_bytes` | bytes | Shared pages, summed |
| `process_group_memory_data_bytes` | bytes | Private writable, summed |
| `process_group_open_fds` | count | Open descriptors, summed |
| `process_group_max_fds` | count | Descriptor limits, summed |
| `process_group_oldest_start_time_seconds` | unix seconds | Oldest member's start |
| `process_group_states{state}` | count | Processes in each state, extra `state` label |
| `process_group_fd_coverage_ratio` | ratio | Members whose fd count was read |
| `process_group_io_coverage_ratio` | ratio | Members whose io was readable |

## 9.3 Counters

| Metric | Unit | Description |
|---|---|---|
| `process_group_cpu_seconds_total{mode}` | seconds | `mode` is `user` or `system` |
| `process_group_minor_page_faults_total` | count | |
| `process_group_major_page_faults_total` | count | |
| `process_group_context_switches_total{kind}` | count | `kind` is `voluntary` or `involuntary` |
| `process_group_read_bytes_total` | bytes | From `io`, `read_bytes` |
| `process_group_write_bytes_total` | bytes | From `io`, `write_bytes` |

## 9.4 Exporter self-metrics

| Metric | Type | Description |
|---|---|---|
| `process_exporter_scan_duration_seconds` | Histogram | Wall time of one scan |
| `process_exporter_scan_cpu_seconds_total` | Counter | CPU the exporter itself consumed |
| `process_exporter_scans_total` | Counter | Completed scans |
| `process_exporter_scan_overruns_total` | Counter | Scans that exceeded the interval |
| `process_exporter_last_scan_timestamp_seconds` | Gauge | Freshness of the published snapshot |
| `process_exporter_procs_total` | Gauge | PIDs seen in `/proc` |
| `process_exporter_procs_scanned` | Gauge | Successfully read |
| `process_exporter_procs_ignored` | Gauge | Matched the ignore list |
| `process_exporter_procs_vanished` | Gauge | Exited mid-scan |
| `process_exporter_procs_denied` | Gauge | Permission denied on a required file |
| `process_exporter_groups_total` | Gauge | Distinct groups currently exported |
| `process_exporter_group_names_seen_total` | Counter | Distinct names ever observed |
| `process_exporter_state_entries` | Gauge | Size of the per-PID state map |
| `process_exporter_read_errors_total{file}` | Counter | Read failures by file kind |
| `process_exporter_build_info{version,goversion}` | Gauge | Constant 1 |

`process_exporter_scan_cpu_seconds_total` deserves emphasis. The exporter measures its own CPU cost and publishes it. An operator can verify directly that the tuning goal is being met:

```promql
rate(process_exporter_scan_cpu_seconds_total[5m]) < 0.02
```

## 9.5 Cardinality

```
series ≈ groups × (12 gauges + 6 counters + state_variants)
```

For a typical server with 60 distinct groups, roughly 1,200 series. For a busy application server with 200 groups, roughly 4,000.

Compare against the PID-keyed alternative: 2,000 live processes at any instant, with complete turnover of short-lived ones, produces tens of thousands of series per day and never stops growing.

---

# Part 10 — Configuration

**File: `/etc/process_exporter/config.yaml`**

```yaml
scan:
  interval: 15s          # time between scan starts
  batch_size: 50         # processes read between yields
  batch_sleep: 5ms       # yield duration
  fd_scan_every: 4       # read fd/ and limits every Nth scan
  read_io: true          # attempt /proc/<pid>/io
  read_status: true      # attempt /proc/<pid>/status for ctx switches
  cache_cmdline: true    # cache cmdline and derived name per PID
  group_retention: 1h    # keep accumulators after the group vanishes

filter:
  ignore_kernel_threads: true
  ignore_comm: [ ... ]           # see Part 6
  ignore_comm_prefix: [ ... ]
  ignore_comm_regex: [ ... ]
  ignore_cmdline_regex: []
  ignore_users: []
  include_only: []

naming:
  fallback: comm                 # comm | exe_basename | cmdline_basename
  resolve_users: true
  rules: [ ... ]                 # see Part 7

server:
  listen: "0.0.0.0:9256"
  metrics_path: /metrics

log:
  level: info                    # debug | info | warn | error
  format: json                   # json | text
```

## 10.1 Tuning table

| Machine | interval | batch_size | batch_sleep | fd_scan_every |
|---|---|---|---|---|
| Small, under 200 procs | 15s | 100 | 2ms | 2 |
| Typical server, under 1,000 | 15s | 50 | 5ms | 4 |
| Busy, 1,000 to 5,000 | 30s | 50 | 5ms | 8 |
| Very busy, over 5,000 | 60s | 100 | 10ms | 10 |
| Latency-sensitive host | 60s | 25 | 20ms | 20 |

The last row trades data freshness for the smallest possible footprint. It is the right choice on a machine where the exporter must be invisible.

## 10.2 Reload

`SIGHUP` or file modification. A configuration that fails validation is rejected and the previous one stays active. Validation reports every problem at once.

| Change | Effect |
|---|---|
| `scan.*` | Applies from the next scan |
| `filter.*` | Applies from the next scan; newly ignored groups age out via `group_retention` |
| `naming.*` | **Rebuilds every group name.** Series with old names age out; new ones start from zero |
| `server.*` | Requires restart |

A naming change is logged as a warning, because it will break every time series.

`process_exporter -check -config <path>` validates without starting.

---

# Part 11 — Operational behaviour

## 11.1 Startup

1. Parse and validate configuration. Failure exits with code 2.
2. Read system constants: clock ticks per second, page size, boot time.
3. Build the UID-to-name cache.
4. Compile filter patterns and naming rules. Failure exits with code 3.
5. Run one **priming scan**. It publishes gauges but no counter deltas, because there is no previous state.
6. Start the HTTP server.
7. Start the scan timer.

Between the priming scan and the second scan, `/metrics` returns gauges with zero counters. `process_exporter_scans_total` reading 1 tells an operator this is why.

## 11.2 Steady state

Every `interval`, one scan runs to completion and swaps the snapshot pointer. Scrapes read the current pointer with no lock contention against the scanner.

## 11.3 Shutdown

`SIGINT` or `SIGTERM`. HTTP server stops accepting, in-flight scrapes complete, the scanner finishes its current batch and exits. No state is persisted — the exporter rebuilds from a priming scan on the next start.

The consequence of not persisting: a restart resets every counter, and Prometheus sees a counter reset across the restart. This is correct and expected behaviour for a process exporter, matching `node_exporter` and every other agent of this kind. `rate()` handles it.

## 11.4 Failure modes

| Symptom | Cause | Diagnostic |
|---|---|---|
| No counter values | Only one scan has run | `process_exporter_scans_total` |
| I/O metrics all zero | Not root; `io` unreadable | `process_group_io_coverage_ratio` |
| fd metrics missing | Not root, or `fd_scan_every` not yet reached | `process_group_fd_coverage_ratio` |
| Group count growing without bound | A naming rule extracts an unbounded value | `process_exporter_groups_total` trend |
| High exporter CPU | `batch_sleep` too short or `interval` too short | `process_exporter_scan_cpu_seconds_total` |
| Stale metrics | Scans overrunning the interval | `process_exporter_scan_overruns_total` |
| Many processes ignored | Filter too broad | `process_exporter_procs_ignored` |
| RSS sum exceeds physical memory | Shared pages counted per member | Expected; see 8.1 |

---

# Part 12 — Prometheus usage

## 12.1 Scrape

```yaml
scrape_configs:
  - job_name: process
    scrape_interval: 30s
    static_configs:
      - targets: ['host:9256']
```

Scrape interval should be at or above `scan.interval`. Scraping faster than scanning returns the same snapshot repeatedly, which is harmless but pointless.

## 12.2 Queries

**CPU by group:**
```promql
topk(10,
  sum by (name, user) (rate(process_group_cpu_seconds_total[5m]))
)
```

**Memory by group:**
```promql
topk(10, process_group_memory_rss_bytes)
```

**Descriptor pressure:**
```promql
process_group_open_fds / process_group_max_fds > 0.8
```

**Process count change, catching leaks or crash loops:**
```promql
delta(process_group_num_procs[1h]) != 0
```

**Group restarted:**
```promql
changes(process_group_oldest_start_time_seconds[1h]) > 0
```

**Blocked on I/O:**
```promql
process_group_states{state="D"} > 0
```

**Zombies:**
```promql
process_group_states{state="Z"} > 5
```

**Exporter cost, the self-check:**
```promql
rate(process_exporter_scan_cpu_seconds_total[5m])
```

## 12.3 Alerting

```yaml
groups:
  - name: process_exporter
    rules:
      - alert: ProcessGroupFDExhaustion
        expr: process_group_open_fds / process_group_max_fds > 0.9
        for: 5m
        annotations:
          summary: "{{ $labels.name }} ({{ $labels.user }}) near its fd limit"

      - alert: ProcessGroupGone
        expr: process_group_num_procs == 0
        for: 5m
        annotations:
          summary: "{{ $labels.name }} has no running processes"

      - alert: ProcessGroupZombies
        expr: process_group_states{state="Z"} > 10
        for: 10m

      - alert: ProcessExporterExpensive
        expr: rate(process_exporter_scan_cpu_seconds_total[5m]) > 0.05
        for: 15m
        annotations:
          summary: "process_exporter using over 5% of a core; check scan tuning"

      - alert: ProcessExporterStale
        expr: time() - process_exporter_last_scan_timestamp_seconds > 120
        for: 5m

      - alert: ProcessExporterCardinality
        expr: process_exporter_groups_total > 500
        for: 30m
        annotations:
          summary: "{{ $value }} groups; check naming rules for unbounded extraction"
```

---

# Part 13 — Design decisions recorded

| Decision | Choice | Reason |
|---|---|---|
| Key | Name plus user, no PID | PID is unbounded; name is bounded by software inventory; user separates masters from workers |
| Cardinality cap | None | Per your instruction. The ignore list is the control |
| Counter model | Per-group accumulator fed by per-PID deltas | Summing lifetime totals drops on process exit and Prometheus sees a reset |
| PID reuse defence | Start time validation plus negative-delta clamp | Two independent checks |
| State pruning | Generation stamp | Map size tracks live processes, not historical ones |
| Accumulator pruning | `group_retention`, default 1h | A restarting service resumes its counter instead of resetting |
| Scan model | Own timer; scrapes read a snapshot | Cost independent of scrape count and frequency |
| Yield model | Batch plus sleep | Keeps the scheduler from treating the exporter as CPU-bound |
| fd frequency | Every Nth scan | Directory walk is the dominant cost and fd counts change slowly |
| cmdline caching | Cached per PID | Immutable in practice; removes one read per process per scan |
| Kernel threads | Ignored by default | No address space, meaningless memory, unbounded names |
| Privilege | Whatever it was given | Complete CPU and memory regardless; I/O and fd coverage reported as a ratio |
| Threads | Count only, no per-thread | Per-thread would multiply cardinality for little gain |
| RSS sharing | Not corrected | `smaps` is too expensive; `DataBytes` published alongside as the closer figure |
| Persistence | None | Restart resets counters; correct and expected for this class of agent |

---

# Part 14 — Open items for your confirmation

1. **Metric prefix.** `process_group_*` for the data, `process_exporter_*` for self-metrics. Confirm or rename.
2. **Default port.** 9256 is the Prometheus registry allocation for the existing `process-exporter`. Reusing it eases migration but collides if both run. Confirm 9256, or pick another.
3. **`OldestStart` aggregation.** Minimum gives group age. An alternative is maximum, giving "most recently restarted". I chose minimum; say if you want both.
4. **`MaxFDs` aggregation.** Summed, so `open/max` is a meaningful group ratio. The alternative is minimum, giving the tightest individual limit. Summed is more useful for a group; confirm.
5. **`include_only`.** Included as an inverse of the ignore list, for the case where you want a small named set only. Say if you want it removed.
6. **Container awareness.** The current design has none — a containerised process is named by its binary like any other. Adding cgroup or container-name labels is possible but multiplies cardinality by the container count and needs a decision.

Confirm these six and correct anything above, and I will produce the implementation turn plan and the complete function and struct surface, as with the mesh system.
