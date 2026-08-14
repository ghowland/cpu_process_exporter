package config

import "time"

// DefaultIgnoreComm lists system process names that add noise without
// signal. These are kernel threads and driver helpers whose CPU is
// better observed through node_exporter's system-wide metrics and whose
// memory readings are meaningless.
var DefaultIgnoreComm = []string{
	"kthreadd",
	"rcu_gp",
	"rcu_par_gp",
	"rcu_sched",
	"rcu_bh",
	"rcu_tasks_kthre",
	"rcu_tasks_rude_",
	"rcu_tasks_trace",
	"kdevtmpfs",
	"kauditd",
	"khungtaskd",
	"oom_reaper",
	"writeback",
	"kcompactd0",
	"ksmd",
	"khugepaged",
	"kswapd0",
	"kblockd",
	"kintegrityd",
	"blkcg_punt_bio",
	"tpm_dev_wq",
	"ata_sff",
	"md",
	"edac-poller",
	"devfreq_wq",
	"watchdogd",
	"kthrotld",
	"ipv6_addrconf",
	"kstrp",
	"charger_manager",
	"acpi_thermal_pm",
	"mld",
	"jbd2",
	"ext4-rsv-conver",
	"netns",
	"kaluad",
	"kmpath_rdacd",
	"kmpathd",
	"kmpath_handlerd",
	"nvme-wq",
	"nvme-reset-wq",
	"nvme-delete-wq",
	"cryptd",
	"raid5wq",
	"dm_bufio_cache",
	"vfio-irqfd-clea",
	"zswap-shrink",
	"kworker",
}

// DefaultIgnoreCommPrefix covers the numbered per-CPU kernel threads,
// which are the largest single source of unbounded process names. A
// machine with 64 cores produces 64 distinct ksoftirqd names and an
// unbounded number of kworker names.
var DefaultIgnoreCommPrefix = []string{
	"kworker/",
	"ksoftirqd/",
	"migration/",
	"watchdog/",
	"irq/",
	"cpuhp/",
	"idle_inject/",
	"scsi_eh_",
	"scsi_tmf_",
	"nfsd",
	"loop",
	"xfsalloc",
	"xfs-",
	"jbd2/",
	"ext4-",
	"card0-crtc",
	"ttm_swap",
	"kdmflush",
}

// DefaultIgnoreCommRegex catches the kworker naming form directly, as a
// second line of defence behind the prefix list.
var DefaultIgnoreCommRegex = []string{
	`^kworker/u?[0-9]+:[0-9]+`,
}

// DefaultNamingRules covers the interpreter cases where comm is the
// interpreter rather than the service. Grouping on comm alone would put
// every JVM on the machine into one bucket, which destroys the
// distinction that matters most.
var DefaultNamingRules = []NamingRule{
	{
		MatchComm: "java",
		From:      "cmdline",
		Regex:     `-jar\s+\S*?([^/\s]+)\.jar`,
		Name:      "java/$1",
	},
	{
		MatchComm: "java",
		From:      "cmdline",
		Regex:     `\s([a-zA-Z][a-zA-Z0-9_]*(?:\.[a-zA-Z0-9_]+)*\.[A-Z][a-zA-Z0-9_]*)(?:\s|$)`,
		Name:      "java/$1",
	},
	{
		MatchCommPrefix: "python",
		From:            "cmdline",
		Regex:           `([^/\s]+)\.py`,
		Name:            "python/$1",
	},
	{
		MatchComm: "node",
		From:      "cmdline",
		Regex:     `([^/\s]+)\.js`,
		Name:      "node/$1",
	},
	{
		MatchComm: "ruby",
		From:      "cmdline",
		Regex:     `([^/\s]+)\.rb`,
		Name:      "ruby/$1",
	},
	{
		MatchComm: "systemd",
		From:      "cmdline",
		Regex:     `(--user)`,
		Name:      "systemd-user",
	},
	{
		MatchCommPrefix: "containerd-shim",
		Name:            "containerd-shim",
	},
	{
		MatchCommPrefix: "runc",
		Name:            "runc",
	},
}

// Default returns a Config with every default value applied. The
// shipped defaults are intended to be adequate on a stock Linux server
// without modification.
func Default() Config {
	return Config{
		Scan: ScanConfig{
			Interval:       Duration(15 * time.Second),
			BatchSize:      50,
			BatchSleep:     Duration(5 * time.Millisecond),
			FDScanEvery:    4,
			ReadIO:         true,
			ReadStatus:     true,
			CacheCmdline:   true,
			GroupRetention: Duration(time.Hour),
			ProcPath:       "/proc",
		},
		Filter: FilterConfig{
			IgnoreKernelThreads: true,
			IgnoreComm:          nil, // filled by normalise
			IgnoreCommPrefix:    nil,
			IgnoreCommRegex:     nil,
			IgnoreCmdlineRegex:  []string{},
			IgnoreUsers:         []string{},
			IncludeOnly:         []string{},
		},
		Naming: NamingConfig{
			Fallback:     "comm",
			ResolveUsers: true,
			Rules:        nil, // filled by normalise
		},
		Server: ServerConfig{
			Listen:      "0.0.0.0:9256",
			MetricsPath: "/metrics",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

