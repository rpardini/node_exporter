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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// The Rockchip vendor kernel (e.g. 6.1.x-vendor-rk35xx) exposes accelerator load
// through non-mainline interfaces:
//   - RKNPU (NPU) at /sys/kernel/debug/rknpu/load
//   - RKMPP (media, "mpp_service") at /proc/mpp_service/load
//
// The RKMPP load file only produces data once a non-zero sampling interval (in
// milliseconds) has been written to /proc/mpp_service/load_interval.

const (
	rknpuSubsystem = "rknpu"
	rkmppSubsystem = "rkmpp"
)

var (
	mppLoadInterval = kingpin.Flag("collector.rockchip.mpp-load-interval",
		"Sampling interval to write to /proc/mpp_service/load_interval if currently unset (0 to disable writing).").
		Default("1s").Duration()

	// "NPU load:  Core0:  6%, Core1:  5%, Core2:  5%,"
	rknpuCoreRE = regexp.MustCompile(`Core(\d+):\s*(\d+)\s*%`)
	// "NPU load:  0%" (single-core boards without a CoreN: label)
	rknpuSingleRE = regexp.MustCompile(`(\d+)\s*%`)
	// "fdbd0000.rkvenc-core      load:  11.20% utilization:  10.62%"
	rkmppLoadRE = regexp.MustCompile(`^(\S+)\s+load:\s*([\d.]+)\s*%\s+utilization:\s*([\d.]+)\s*%`)
)

type rockchipCollector struct {
	rknpuLoad        *prometheus.Desc
	rkmppLoad        *prometheus.Desc
	rkmppUtilization *prometheus.Desc
	logger           *slog.Logger
}

func init() {
	registerCollector("rockchip", defaultDisabled, NewRockchipCollector)
}

// NewRockchipCollector returns a new Collector exposing Rockchip vendor-kernel
// NPU (rknpu) and media (rkmpp) accelerator load.
func NewRockchipCollector(logger *slog.Logger) (Collector, error) {
	c := &rockchipCollector{
		rknpuLoad: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, rknpuSubsystem, "load_ratio"),
			"NPU core load, as a ratio (0.0-1.0), from /sys/kernel/debug/rknpu/load.",
			[]string{"core"}, nil,
		),
		rkmppLoad: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, rkmppSubsystem, "load_ratio"),
			"MPP device load, as a ratio (0.0-1.0), from /proc/mpp_service/load.",
			[]string{"device"}, nil,
		),
		rkmppUtilization: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, rkmppSubsystem, "utilization_ratio"),
			"MPP device utilization, as a ratio (0.0-1.0), from /proc/mpp_service/load.",
			[]string{"device"}, nil,
		),
		logger: logger,
	}

	// Best-effort priming of the RKMPP sampling interval at startup so the load
	// file has data by the time the first scrape arrives.
	c.maybeSetLoadInterval()

	return c, nil
}

// sourceAbsent reports whether err means the source file simply isn't there or
// isn't readable on this system (i.e. not a Rockchip vendor kernel, or no
// privileges to read debugfs), as opposed to a genuine unexpected error.
func sourceAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}

// maybeSetLoadInterval writes the configured sampling interval to
// /proc/mpp_service/load_interval, but only if it is currently unset (0). An
// already-configured interval is left untouched. All errors are non-fatal.
func (c *rockchipCollector) maybeSetLoadInterval() {
	if *mppLoadInterval == 0 {
		return
	}
	path := procFilePath("mpp_service/load_interval")
	current, err := readUintFromFile(path)
	if err != nil {
		if !sourceAbsent(err) {
			c.logger.Debug("could not read mpp load_interval", "err", err)
		}
		return
	}
	if current != 0 {
		return
	}
	ms := strconv.FormatInt(mppLoadInterval.Milliseconds(), 10)
	if err := os.WriteFile(path, []byte(ms), 0644); err != nil {
		c.logger.Debug("could not set mpp load_interval", "interval_ms", ms, "err", err)
		return
	}
	c.logger.Debug("primed mpp load_interval", "interval_ms", ms)
}

func (c *rockchipCollector) Update(ch chan<- prometheus.Metric) error {
	npuErr := c.updateRKNPU(ch)
	mppErr := c.updateRKMPP(ch)

	// An absent/unreadable source (e.g. debugfs not mounted into the container,
	// or no privileges to read it) is expected on non-Rockchip machines, so it is
	// not treated as an error. Log it at debug level so a partially-working setup
	// (e.g. rkmpp visible but rknpu is not) can be diagnosed with --log.level=debug.
	if sourceAbsent(npuErr) {
		c.logger.Debug("rknpu source unavailable, skipping", "path", sysFilePath("kernel/debug/rknpu/load"), "err", npuErr)
	}
	if sourceAbsent(mppErr) {
		c.logger.Debug("rkmpp source unavailable, skipping", "path", procFilePath("mpp_service/load"), "err", mppErr)
	}

	// If neither accelerator interface is present, report no data rather than an
	// error, so the collector is a clean no-op on non-Rockchip machines.
	if sourceAbsent(npuErr) && sourceAbsent(mppErr) {
		return ErrNoData
	}
	if npuErr != nil && !sourceAbsent(npuErr) {
		return npuErr
	}
	if mppErr != nil && !sourceAbsent(mppErr) {
		return mppErr
	}
	return nil
}

func (c *rockchipCollector) updateRKNPU(ch chan<- prometheus.Metric) error {
	data, err := os.ReadFile(sysFilePath("kernel/debug/rknpu/load"))
	if err != nil {
		return err
	}

	line := string(data)
	if cores := rknpuCoreRE.FindAllStringSubmatch(line, -1); len(cores) > 0 {
		for _, m := range cores {
			load, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				return fmt.Errorf("could not parse rknpu core load %q: %w", m[2], err)
			}
			ch <- prometheus.MustNewConstMetric(c.rknpuLoad, prometheus.GaugeValue, load/100, m[1])
		}
		return nil
	}

	// Single-core boards report just "NPU load:  0%" with no CoreN: label.
	if m := rknpuSingleRE.FindStringSubmatch(line); m != nil {
		load, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return fmt.Errorf("could not parse rknpu load %q: %w", m[1], err)
		}
		ch <- prometheus.MustNewConstMetric(c.rknpuLoad, prometheus.GaugeValue, load/100, "0")
		return nil
	}

	return fmt.Errorf("could not parse rknpu load: %q", strings.TrimSpace(line))
}

func (c *rockchipCollector) updateRKMPP(ch chan<- prometheus.Metric) error {
	// Self-heal in case the interval was reset (e.g. by a module reload). Note
	// this only works where /proc is writable: in the common Kubernetes setup the
	// host /proc is bind-mounted read-only (--path.procfs=/host/proc), so this
	// write fails silently and load_interval must be primed out-of-band (see the
	// Rockchip section of the README for an init-container example).
	c.maybeSetLoadInterval()

	data, err := os.ReadFile(procFilePath("mpp_service/load"))
	if err != nil {
		return err
	}

	body := string(data)
	// When the sampling interval is unset the kernel prints a help message
	// instead of data; skip quietly until it warms up.
	if strings.Contains(body, "please set load_interval") {
		c.logger.Debug("mpp load not sampled yet; load_interval unset")
		return nil
	}

	for _, l := range strings.Split(body, "\n") {
		m := rkmppLoadRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		load, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return fmt.Errorf("could not parse rkmpp load %q: %w", m[2], err)
		}
		util, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			return fmt.Errorf("could not parse rkmpp utilization %q: %w", m[3], err)
		}
		ch <- prometheus.MustNewConstMetric(c.rkmppLoad, prometheus.GaugeValue, load/100, m[1])
		ch <- prometheus.MustNewConstMetric(c.rkmppUtilization, prometheus.GaugeValue, util/100, m[1])
	}

	return nil
}
