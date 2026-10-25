// Package memlimit sets the Go runtime's soft memory limit from the
// container's memory limit.
//
// Without a limit the garbage collector only runs when the heap doubles, so
// a process holding a large live heap, such as a big metadata document
// being rewritten, can grow past a container limit and be OOM-killed while
// most of that memory is garbage. A soft limit just below the container's
// makes the collector work harder as it gets close instead.
package memlimit

import (
	"bufio"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// ratio is the share of the container limit given to the Go runtime. The
// rest is left for memory the runtime does not account for, such as the git
// processes the proxy runs, which share the container's cgroup.
const ratio = 0.8

// noLimit is the value cgroup v1 reports, rounded to a page, when no limit
// is set. Anything this large is treated as unlimited.
const noLimit = 1 << 62

// Apply sets the runtime memory limit to ratio of the cgroup memory limit,
// unless GOMEMLIMIT is set, which the runtime has applied already, or no
// limit can be found. It only looks for one on Linux.
func Apply(logger *slog.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" || runtime.GOOS != "linux" {
		return
	}
	limit, ok := cgroupLimit(os.DirFS("/"))
	if !ok {
		return
	}
	set := int64(float64(limit) * ratio)
	debug.SetMemoryLimit(set)
	logger.Info("set Go memory limit from cgroup", "memory_limit", set, "cgroup_limit", limit)
}

// cgroupLimit returns the memory limit of the cgroup the process is in,
// read from fsys, the root filesystem. It reads cgroup v2's memory.max, or
// v1's memory.limit_in_bytes, first in the process's own cgroup and then at
// the hierarchy root, which is what a container sees as its own.
func cgroupLimit(fsys fs.FS) (int64, bool) {
	v2, v1 := ownCgroups(fsys)
	candidates := []string{
		path.Join("sys/fs/cgroup", v2, "memory.max"),
		"sys/fs/cgroup/memory.max",
		path.Join("sys/fs/cgroup/memory", v1, "memory.limit_in_bytes"),
		"sys/fs/cgroup/memory/memory.limit_in_bytes",
	}
	for _, name := range candidates {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			continue
		}
		// "max" means no limit at this level; a later candidate may still
		// have one.
		limit, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err == nil && limit > 0 && limit < noLimit {
			return limit, true
		}
	}
	return 0, false
}

// ownCgroups returns the process's cgroup v2 path and its cgroup v1 memory
// controller path from /proc/self/cgroup, each "" when not listed.
func ownCgroups(fsys fs.FS) (v2, v1 string) {
	f, err := fsys.Open("proc/self/cgroup")
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Lines are "hierarchy-ID:controller-list:cgroup-path".
		id, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		controllers, cgroup, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		switch {
		case id == "0" && controllers == "":
			v2 = cgroup
		case hasController(controllers, "memory"):
			v1 = cgroup
		}
	}
	return v2, v1
}

func hasController(list, name string) bool {
	for controller := range strings.SplitSeq(list, ",") {
		if controller == name {
			return true
		}
	}
	return false
}
