package zfs

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// PoolStatus holds parsed `zpool status` information for one pool.
type PoolStatus struct {
	Name     string
	State    string // ONLINE, DEGRADED, FAULTED, ...
	Topology string // raidz1, mirror, stripe, ...; empty when unknown
	Scrub    ScrubStatus
	Vdevs    []VdevStatus
}

// ScrubStatus holds the scrub (or resilver) status parsed from the scan line.
type ScrubStatus struct {
	State    string // NONE, SCANNING, FINISHED, CANCELED
	Progress string // e.g. "10.00%" while scanning
	Errors   uint64
}

// VdevStatus is a single vdev row (mirror, raidz, or leaf disk).
type VdevStatus struct {
	Name         string
	State        string
	ReadErrs     uint64
	WriteErrs    uint64
	ChecksumErrs uint64
}

var (
	progressRe = regexp.MustCompile(`(\d+\.\d+)%\s+done`)
	errorsRe   = regexp.MustCompile(`with\s+(\d+)\s+errors`)
)

// PoolStatuses runs `zpool status` and parses per-pool state, scrub, and vdev
// information. The human-readable format has been stable across OpenZFS
// releases; rows are matched by their tabular shape rather than position.
func PoolStatuses() ([]PoolStatus, error) {
	out, err := commandOutput("zpool", "status")
	if err != nil {
		return nil, fmt.Errorf("zpool status: %w", err)
	}
	return parseZpoolStatusOutput(out)
}

// parseZpoolStatusOutput parses the output of `zpool status`.
func parseZpoolStatusOutput(out []byte) ([]PoolStatus, error) {
	var pools []PoolStatus
	var current *PoolStatus
	inConfig := false
	scanContinuation := false // next non-blank line continues the scan line (progress)
	var tree configTree

	// The topology of the pool being parsed is only known once its config
	// table ends, so it is applied when the next pool starts and after the
	// final line.
	finishPool := func() {
		if current != nil {
			current.Topology = tree.topology()
		}
		tree = configTree{}
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "pool:"):
			finishPool()
			pools = append(pools, PoolStatus{Name: strings.TrimSpace(strings.TrimPrefix(trimmed, "pool:"))})
			current = &pools[len(pools)-1]
			inConfig = false
			scanContinuation = false
		case current == nil:
			continue
		case strings.HasPrefix(trimmed, "state:"):
			current.State = strings.TrimSpace(strings.TrimPrefix(trimmed, "state:"))
		case strings.HasPrefix(trimmed, "scan:"):
			current.Scrub = parseScanLine(trimmed)
			// zpool status prints the progress percentage on the line after scan.
			scanContinuation = true
		case trimmed == "config:":
			inConfig = true
		case scanContinuation:
			// The line after scan: may be an indented progress continuation.
			if m := progressRe.FindStringSubmatch(trimmed); m != nil {
				current.Scrub.Progress = m[1] + "%"
			}
			scanContinuation = false
		case inConfig && (line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			// Table rows are indented; blank lines separate sections. The
			// column header and the pool's own row are skipped.
			if trimmed != "" && !strings.HasPrefix(trimmed, "NAME") {
				tree.addRow(line, trimmed, current.Name)
				if vdev, ok := parseVdevLine(trimmed, current.Name); ok {
					current.Vdevs = append(current.Vdevs, vdev)
				}
			}
		case inConfig:
			// unindented line (errors:, status:, next pool:) ends the table
			inConfig = false
		}
	}
	finishPool()
	return pools, scanner.Err()
}

// configTree accumulates the data-section rows of one pool's config table so
// the top-level vdevs can be identified once the table is complete. Rows after
// a section header (logs, cache, spares, special, dedup) are dropped: those
// groups contribute capacity or caching but not the pool's data redundancy
// level, so a mirrored SLOG must not make the pool look mirrored.
type configTree struct {
	poolIndent  int
	poolSeen    bool
	inAuxiliary bool
	rows        []configRow
}

type configRow struct {
	indent int
	name   string
}

// addRow records one config table row. Rows are classified by indentation
// relative to the pool's own row rather than by a fixed width, so the parser
// does not depend on zpool's exact column layout.
func (t *configTree) addRow(line, trimmed, poolName string) {
	indent := len(line) - len(strings.TrimLeft(line, " \t"))
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return
	}
	name := fields[0]
	if !t.poolSeen {
		// The pool's own row establishes the base indent. Anything before it
		// (a stray header) is ignored.
		if name == poolName {
			t.poolIndent, t.poolSeen = indent, true
		}
		return
	}
	if indent <= t.poolIndent {
		// A row back at the pool's indent is a section header such as "logs"
		// or "cache". Everything after one belongs to an auxiliary group;
		// treating an unrecognized header the same way means an unfamiliar
		// group can never be mistaken for a data vdev.
		t.inAuxiliary = true
		return
	}
	if t.inAuxiliary {
		return
	}
	t.rows = append(t.rows, configRow{indent: indent, name: name})
}

// topology summarizes the pool's top-level data vdevs, e.g. "raidz1" or
// "mirror". Pools combining vdev types report them joined with "+".
func (t *configTree) topology() string {
	if len(t.rows) == 0 {
		return ""
	}
	// Top-level vdevs are the shallowest rows below the pool; everything
	// deeper is a leaf device belonging to one of them.
	minIndent := t.rows[0].indent
	for _, row := range t.rows[1:] {
		minIndent = min(minIndent, row.indent)
	}
	var types []string
	for _, row := range t.rows {
		if row.indent != minIndent {
			continue
		}
		if kind := vdevType(row.name); !slices.Contains(types, kind) {
			types = append(types, kind)
		}
	}
	slices.Sort(types)
	return strings.Join(types, "+")
}

// vdevType maps a top-level vdev name to its redundancy kind. ZFS names them
// "<kind>-N" (mirror-0, raidz2-1); dRAID appends its parameters to the kind
// (draid2:4d:12c:1s-0). A bare device name is a stripe member.
func vdevType(name string) string {
	kind, _, found := strings.Cut(name, "-")
	if !found {
		return "stripe"
	}
	// dRAID encodes its geometry after the kind; keep only the parity level.
	if base, _, ok := strings.Cut(kind, ":"); ok {
		kind = base
	}
	switch kind {
	case "mirror", "raidz1", "raidz2", "raidz3", "draid1", "draid2", "draid3":
		return kind
	case "raidz":
		// Pre-OpenZFS pools name single-parity raidz without the level.
		return "raidz1"
	}
	// A device name that merely contains a hyphen (by-id paths, partitions).
	return "stripe"
}

// parseScanLine maps a `scan:` line to a ScrubStatus.
func parseScanLine(line string) ScrubStatus {
	var scrub ScrubStatus
	switch {
	case strings.Contains(line, "in progress"):
		scrub.State = "SCANNING"
	case strings.Contains(line, "canceled"):
		scrub.State = "CANCELED"
	case strings.Contains(line, "repaired"), strings.Contains(line, "resilvered"):
		scrub.State = "FINISHED"
	default:
		scrub.State = "NONE"
	}
	if m := progressRe.FindStringSubmatch(line); m != nil {
		scrub.Progress = m[1] + "%"
	}
	if m := errorsRe.FindStringSubmatch(line); m != nil {
		if n, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			scrub.Errors = n
		}
	}
	return scrub
}

// parseVdevLine parses one row of the config table. Rows have the shape
// "NAME STATE READ WRITE CKSUM [extra...]". The first data row is the pool
// itself and is skipped since it duplicates pool-level info.
func parseVdevLine(line, poolName string) (VdevStatus, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return VdevStatus{}, false
	}
	if fields[0] == poolName {
		return VdevStatus{}, false
	}
	read, err1 := strconv.ParseUint(fields[2], 10, 64)
	write, err2 := strconv.ParseUint(fields[3], 10, 64)
	cksum, err3 := strconv.ParseUint(fields[4], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return VdevStatus{}, false
	}
	return VdevStatus{
		Name:         fields[0],
		State:        fields[1],
		ReadErrs:     read,
		WriteErrs:    write,
		ChecksumErrs: cksum,
	}, true
}
