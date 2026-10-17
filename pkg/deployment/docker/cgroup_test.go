package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	containertypes "github.com/moby/moby/api/types/container"
)

// writeCgroupTree lays out a fake /proc/<pid>/cgroup and the cgroup v2
// directory it names, with the given files in that directory.
func writeCgroupTree(t *testing.T, pid, path string, files map[string]string) cgroupReader {
	t.Helper()
	root := t.TempDir()
	reader := cgroupReader{procRoot: filepath.Join(root, "proc"), cgroupRoot: filepath.Join(root, "cgroup")}
	if err := os.MkdirAll(filepath.Join(reader.procRoot, pid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reader.procRoot, pid, "cgroup"), []byte("0::"+path+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(reader.cgroupRoot, filepath.FromSlash(path))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return reader
}

func TestCgroupReaderReadsPeakAndPressure(t *testing.T) {
	reader := writeCgroupTree(t, "42", "/system.slice/docker-"+resourceSandboxID+".scope", map[string]string{
		"memory.peak":     "4800512\n",
		"cpu.pressure":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=1500\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=700\n",
		"memory.pressure": "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
		"io.pressure":     "some avg10=1.20 avg60=0.40 avg300=0.10 total=99\n",
	})
	var reading core.ResourceReading
	if err := reader.read(42, resourceSandboxID, &reading); err != nil {
		t.Fatal(err)
	}
	if reading.MemoryPeakBytes == nil || *reading.MemoryPeakBytes != 4800512 {
		t.Fatalf("memory peak = %v", reading.MemoryPeakBytes)
	}
	pressure := reading.Pressure
	if pressure == nil || pressure.CPU.SomeMicroseconds != 1500 || *pressure.CPU.FullMicroseconds != 700 {
		t.Fatalf("cpu pressure = %+v", pressure)
	}
	if pressure.Memory.SomeMicroseconds != 0 || pressure.Memory.FullMicroseconds == nil || *pressure.Memory.FullMicroseconds != 0 {
		t.Fatalf("measured zero memory pressure lost: %+v", pressure.Memory)
	}
	if pressure.IO.SomeMicroseconds != 99 || pressure.IO.FullMicroseconds != nil {
		t.Fatalf("io pressure = %+v", pressure.IO)
	}
}

func TestCgroupReaderLeavesAbsentCapabilitiesUnmeasured(t *testing.T) {
	// No PSI files, as on kernels built without PSI.
	reader := writeCgroupTree(t, "42", "/docker/"+resourceSandboxID, map[string]string{"memory.peak": "10\n"})
	var reading core.ResourceReading
	if err := reader.read(42, resourceSandboxID, &reading); err != nil {
		t.Fatal(err)
	}
	if reading.MemoryPeakBytes == nil || reading.Pressure != nil {
		t.Fatalf("reading = %+v", reading)
	}
	// An exited process, an unknown PID, or a cgroup of another runtime.
	for name, check := range map[string]struct {
		pid int
		id  string
	}{"gone": {43, resourceSandboxID}, "no pid": {0, resourceSandboxID}, "reused pid": {42, resourceHarnessID}} {
		var unmeasured core.ResourceReading
		if err := reader.read(check.pid, check.id, &unmeasured); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if unmeasured.MemoryPeakBytes != nil || unmeasured.Pressure != nil {
			t.Fatalf("%s: reading = %+v", name, unmeasured)
		}
	}
}

func TestCgroupReaderRejectsMalformedFiles(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"peak":        {"memory.peak": "lots\n"},
		"no total":    {"cpu.pressure": "some avg10=0.00\n"},
		"no some":     {"cpu.pressure": "full avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"},
		"bad total":   {"io.pressure": "some total=x\n"},
		"unknown key": {"io.pressure": "some total=1\nboth total=2\n"},
	} {
		reader := writeCgroupTree(t, "42", "/docker/"+resourceSandboxID, files)
		var reading core.ResourceReading
		if err := reader.read(42, resourceSandboxID, &reading); err == nil {
			t.Fatalf("%s: malformed file accepted", name)
		}
	}
}

func TestDockerResourceSourceAddsOptionalMeasurements(t *testing.T) {
	source, fake := newFakeResourceSource()
	stats := fake.stats[resourceSandboxID]
	stats.PidsStats = containertypes.PidsStats{Current: 157}
	stats.MemoryStats.Stats = map[string]uint64{"shmem": 0, "anon": 1}
	stats.Networks = map[string]containertypes.NetworkStats{"eth0": {RxBytes: 1000, TxBytes: 10}, "eth1": {RxBytes: 5, TxBytes: 1}}
	fake.stats[resourceSandboxID] = stats
	inspection := fake.inspections[resourceSandboxID]
	inspection.State.Pid = 42
	fake.inspections[resourceSandboxID] = inspection
	source.cgroups = writeCgroupTree(t, "42", "/system.slice/docker-"+resourceSandboxID+".scope", map[string]string{"memory.peak": "8192\n"})

	readings, err := source.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	harness, sandbox := readings[0], readings[1]
	if *sandbox.Pids != 157 || *sandbox.MemoryShmemBytes != 0 || *sandbox.NetworkRxBytes != 1005 || *sandbox.NetworkTxBytes != 11 || *sandbox.MemoryPeakBytes != 8192 {
		t.Fatalf("sandbox reading = %+v", sandbox)
	}
	// The harness's stats carry none of them and its cgroup is not found.
	if harness.Pids != nil || harness.MemoryShmemBytes != nil || harness.NetworkRxBytes != nil || harness.MemoryPeakBytes != nil || harness.Pressure != nil {
		t.Fatalf("harness reading invents measurements: %+v", harness)
	}

	if err := os.WriteFile(filepath.Join(source.cgroups.cgroupRoot, "system.slice", "docker-"+resourceSandboxID+".scope", "memory.peak"), []byte("?"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "cgroup") {
		t.Fatalf("malformed cgroup file error = %v", err)
	}
}

func TestCgroupReaderReadsCPUThrottling(t *testing.T) {
	path := "/system.slice/docker-" + resourceSandboxID + ".scope"
	reader := writeCgroupTree(t, "42", path, map[string]string{
		"cpu.stat": "usage_usec 9000\nuser_usec 6000\nsystem_usec 3000\nnr_periods 120\nnr_throttled 30\nthrottled_usec 450000\n",
	})
	var reading core.ResourceReading
	if err := reader.read(42, resourceSandboxID, &reading); err != nil {
		t.Fatal(err)
	}
	if got := reading.CPUThrottling; got == nil || *got != (core.CPUThrottling{Periods: 120, ThrottledPeriods: 30, ThrottledMicroseconds: 450000}) {
		t.Fatalf("cpu throttling = %+v", got)
	}

	reader = writeCgroupTree(t, "42", path, map[string]string{"cpu.stat": "usage_usec 9000\nuser_usec 6000\nsystem_usec 3000\n"})
	reading = core.ResourceReading{}
	if err := reader.read(42, resourceSandboxID, &reading); err != nil {
		t.Fatal(err)
	}
	if reading.CPUThrottling != nil {
		t.Fatalf("throttling without the cpu controller = %+v, want unmeasured", reading.CPUThrottling)
	}

	reader = writeCgroupTree(t, "42", path, map[string]string{"cpu.stat": "nr_periods 120\n"})
	if err := reader.read(42, resourceSandboxID, &core.ResourceReading{}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete counters error = %v", err)
	}
}
