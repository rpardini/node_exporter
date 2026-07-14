// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !norockchip

package collector

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type testRockchipCollector struct {
	mc Collector
}

func (c testRockchipCollector) Collect(ch chan<- prometheus.Metric) {
	c.mc.Update(ch)
}

func (c testRockchipCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

// stageRockchip lays out fake sysfs/procfs files in a temp dir and points the
// path flags at it. Passing an empty string for a file omits it (to simulate a
// non-Rockchip machine). It returns the procfs root so tests can read the
// load_interval file back after the collector primes it.
func stageRockchip(t *testing.T, npuLoad, mppLoad, loadInterval string) string {
	t.Helper()
	root := t.TempDir()
	sysDir := filepath.Join(root, "sys")
	procDir := filepath.Join(root, "proc")

	write := func(rel, content string) {
		if content == "" {
			return
		}
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	write("sys/kernel/debug/rknpu/load", npuLoad)
	write("proc/mpp_service/load", mppLoad)
	write("proc/mpp_service/load_interval", loadInterval)

	*sysPath = sysDir
	*procPath = procDir
	return procDir
}

func TestRockchipRK3588(t *testing.T) {
	*mppLoadInterval = time.Second
	stageRockchip(t,
		"NPU load:  Core0:  6%, Core1:  5%, Core2:  0%,\n",
		`fdb90000.jpegd            load:  12.50% utilization:   6.25%
fdbd0000.rkvenc-core      load:  50.00% utilization:  25.00%
fdbe0000.rkvenc-core      load:   0.00% utilization:   0.00%
`,
		"1000\n",
	)

	testcase := `# HELP node_rknpu_load_ratio NPU core load, as a ratio (0.0-1.0), from /sys/kernel/debug/rknpu/load.
# TYPE node_rknpu_load_ratio gauge
node_rknpu_load_ratio{core="0"} 0.06
node_rknpu_load_ratio{core="1"} 0.05
node_rknpu_load_ratio{core="2"} 0
# HELP node_rkmpp_load_ratio MPP device load, as a ratio (0.0-1.0), from /proc/mpp_service/load.
# TYPE node_rkmpp_load_ratio gauge
node_rkmpp_load_ratio{device="fdb90000.jpegd"} 0.125
node_rkmpp_load_ratio{device="fdbd0000.rkvenc-core"} 0.5
node_rkmpp_load_ratio{device="fdbe0000.rkvenc-core"} 0
# HELP node_rkmpp_utilization_ratio MPP device utilization, as a ratio (0.0-1.0), from /proc/mpp_service/load.
# TYPE node_rkmpp_utilization_ratio gauge
node_rkmpp_utilization_ratio{device="fdb90000.jpegd"} 0.0625
node_rkmpp_utilization_ratio{device="fdbd0000.rkvenc-core"} 0.25
node_rkmpp_utilization_ratio{device="fdbe0000.rkvenc-core"} 0
`

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc, err := NewRockchipCollector(logger)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(testRockchipCollector{mc})

	if err := testutil.GatherAndCompare(reg, strings.NewReader(testcase)); err != nil {
		t.Fatal(err)
	}
}

// TestRockchipSingleCore covers the RK3568 single-core NPU format that has no
// "CoreN:" label, and a machine where the RKMPP interface is absent.
func TestRockchipSingleCore(t *testing.T) {
	*mppLoadInterval = time.Second
	stageRockchip(t, "NPU load:  0%\n", "", "")

	testcase := `# HELP node_rknpu_load_ratio NPU core load, as a ratio (0.0-1.0), from /sys/kernel/debug/rknpu/load.
# TYPE node_rknpu_load_ratio gauge
node_rknpu_load_ratio{core="0"} 0
`

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc, err := NewRockchipCollector(logger)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(testRockchipCollector{mc})

	if err := testutil.GatherAndCompare(reg, strings.NewReader(testcase)); err != nil {
		t.Fatal(err)
	}
}

// TestRockchipLoadIntervalWrite verifies the collector primes load_interval when
// it is unset, and leaves an already-configured value untouched.
func TestRockchipLoadIntervalWrite(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("writes when unset", func(t *testing.T) {
		*mppLoadInterval = time.Second
		procDir := stageRockchip(t, "", "", "0\n")
		if _, err := NewRockchipCollector(logger); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(procDir, "mpp_service/load_interval"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "1000" {
			t.Fatalf("expected load_interval 1000, got %q", string(got))
		}
	})

	t.Run("leaves preset value untouched", func(t *testing.T) {
		*mppLoadInterval = time.Second
		procDir := stageRockchip(t, "", "", "500\n")
		if _, err := NewRockchipCollector(logger); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(procDir, "mpp_service/load_interval"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "500" {
			t.Fatalf("expected load_interval 500, got %q", string(got))
		}
	})

	t.Run("does not write when disabled", func(t *testing.T) {
		*mppLoadInterval = 0
		procDir := stageRockchip(t, "", "", "0\n")
		if _, err := NewRockchipCollector(logger); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(procDir, "mpp_service/load_interval"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "0" {
			t.Fatalf("expected load_interval unchanged (0), got %q", string(got))
		}
	})
}

// TestRockchipNoData ensures the collector is a clean no-op on non-Rockchip machines.
func TestRockchipNoData(t *testing.T) {
	*mppLoadInterval = 0
	stageRockchip(t, "", "", "")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc, err := NewRockchipCollector(logger)
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan prometheus.Metric, 32)
	err = mc.Update(ch)
	close(ch)

	if err != ErrNoData {
		t.Fatalf("expected ErrNoData, got %v", err)
	}
}
