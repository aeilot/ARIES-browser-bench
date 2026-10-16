package docker

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hyscale-lab/aries/pkg/core"
)

// cgroupReader reads the measurements Docker's stats API lacks, memory.peak
// and pressure stall information, from a runtime's cgroup v2 directory. It
// finds the directory through the runtime's init process in the host's
// /proc, so it applies only when the daemon shares the host with ARIES, which
// ARIES's local-socket deployment requires. A capability that is absent (no
// such process or file, cgroup v1, kernels without PSI) leaves the field
// unmeasured; other read and parse errors are measurement failures.
type cgroupReader struct {
	procRoot   string
	cgroupRoot string
}

var hostCgroups = cgroupReader{procRoot: "/proc", cgroupRoot: "/sys/fs/cgroup"}

// absent reports an error that means the capability is not there.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ESRCH)
}

// directory resolves the cgroup v2 directory of process pid, and confirms it
// belongs to the runtime by its ID. ok is false when it cannot be resolved.
func (reader cgroupReader) directory(pid int, runtimeID string) (string, bool, error) {
	if pid <= 0 || runtimeID == "" {
		return "", false, nil
	}
	data, err := os.ReadFile(filepath.Join(reader.procRoot, strconv.Itoa(pid), "cgroup"))
	if absent(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read cgroup of process %d: %w", pid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		path, unified := strings.CutPrefix(line, "0::")
		if !unified {
			continue
		}
		// The process may have exited and its PID been reused; only a path
		// naming the runtime (docker-<id>.scope or docker/<id>) is its own.
		if !strings.Contains(path, runtimeID) || strings.Contains(path, "..") {
			return "", false, nil
		}
		return filepath.Join(reader.cgroupRoot, filepath.FromSlash(path)), true, nil
	}
	return "", false, nil
}

// read adds memory.peak and PSI totals for the runtime to reading.
func (reader cgroupReader) read(pid int, runtimeID string, reading *core.ResourceReading) error {
	directory, ok, err := reader.directory(pid, runtimeID)
	if err != nil || !ok {
		return err
	}
	peak, err := readCgroupUint(filepath.Join(directory, "memory.peak"))
	if err != nil {
		return err
	}
	reading.MemoryPeakBytes = peak
	var pressure core.PressureReading
	for _, resource := range []struct {
		file   string
		target **core.PressureTotals
	}{{"cpu.pressure", &pressure.CPU}, {"memory.pressure", &pressure.Memory}, {"io.pressure", &pressure.IO}} {
		totals, err := readPressure(filepath.Join(directory, resource.file))
		if err != nil {
			return err
		}
		*resource.target = totals
	}
	if pressure.CPU != nil || pressure.Memory != nil || pressure.IO != nil {
		reading.Pressure = &pressure
	}
	return nil
}

func readCgroupUint(path string) (*uint64, error) {
	data, err := os.ReadFile(path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &value, nil
}

// readPressure parses a PSI file such as
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=1234
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=567
func readPressure(path string) (*core.PressureTotals, error) {
	data, err := os.ReadFile(path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var totals core.PressureTotals
	sawSome := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		var total *uint64
		for _, field := range fields[1:] {
			if text, ok := strings.CutPrefix(field, "total="); ok {
				value, err := strconv.ParseUint(text, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("parse %s: %w", path, err)
				}
				total = &value
			}
		}
		if total == nil {
			return nil, fmt.Errorf("parse %s: line %q has no total", path, scanner.Text())
		}
		switch fields[0] {
		case "some":
			totals.SomeMicroseconds, sawSome = *total, true
		case "full":
			totals.FullMicroseconds = total
		default:
			return nil, fmt.Errorf("parse %s: unexpected line %q", path, scanner.Text())
		}
	}
	if !sawSome {
		return nil, fmt.Errorf("parse %s: no \"some\" line", path)
	}
	return &totals, nil
}
