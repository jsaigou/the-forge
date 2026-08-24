// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type metricsView struct{ d *DB }

// Metrics returns the metric_samples time-series surface (Sprint 0 §0.4).
func (d *DB) Metrics() Metrics { return metricsView{d} }

// RecordSample inserts (or, for a re-sampled second, replaces) one row.
// ts is the SQLite PRIMARY KEY — INSERT OR REPLACE keeps a re-tick of the
// same unix second idempotent instead of erroring.
func (v metricsView) RecordSample(ctx context.Context, s MetricSample) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT OR REPLACE INTO metric_samples
		   (ts, gtt_used_bytes, gtt_total_bytes, gpu_use_pct, mem_used_bytes, mem_total_bytes,
		    disk_used_bytes, disk_total_bytes, temp_celsius, inference_rss_bytes,
		    package_power_w, active_slots, energy_wh_total,
		    cpu_pct, net_rx_bytes_per_sec, net_tx_bytes_per_sec,
		    gpu_junction_temp_celsius, cpu_package_temp_celsius, nvme_temp_celsius)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		unixOf(s.TS), intPtrArg(s.GTTUsedBytes), intPtrArg(s.GTTTotalBytes),
		floatPtrArg(s.GPUUsePct), intPtrArg(s.MemUsedBytes), intPtrArg(s.MemTotalBytes),
		intPtrArg(s.DiskUsedBytes), intPtrArg(s.DiskTotalBytes), floatPtrArg(s.TempCelsius),
		intPtrArg(s.InferenceRSSBytes),
		floatPtrArg(s.PackagePowerW), intPtrArgInt(s.ActiveSlots), floatPtrArg(s.EnergyWhTotal),
		floatPtrArg(s.CPUPct), floatPtrArg(s.NetRxBytesPerSec), floatPtrArg(s.NetTxBytesPerSec),
		floatPtrArg(s.GPUJunctionTempC), floatPtrArg(s.CPUPackageTempC), floatPtrArg(s.NVMeTempC),
	)
	if err != nil {
		return fmt.Errorf("store: metrics.record_sample: %w", err)
	}
	return nil
}

// Range returns samples with from <= ts <= to, oldest first — the raw feed
// both the history downsampler and the export dump read.
func (v metricsView) Range(ctx context.Context, from, to time.Time) ([]MetricSample, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT ts, gtt_used_bytes, gtt_total_bytes, gpu_use_pct, mem_used_bytes, mem_total_bytes,
		        disk_used_bytes, disk_total_bytes, temp_celsius, inference_rss_bytes,
		        package_power_w, active_slots, energy_wh_total,
		        cpu_pct, net_rx_bytes_per_sec, net_tx_bytes_per_sec,
		        gpu_junction_temp_celsius, cpu_package_temp_celsius, nvme_temp_celsius
		 FROM metric_samples WHERE ts >= ? AND ts <= ? ORDER BY ts ASC`,
		unixOf(from), unixOf(to))
	if err != nil {
		return nil, fmt.Errorf("store: metrics.range: %w", err)
	}
	defer rows.Close()
	var out []MetricSample
	for rows.Next() {
		var s MetricSample
		var ts int64
		var gttUsed, gttTotal, memUsed, memTotal, diskUsed, diskTotal, rss, activeSlots sql.NullInt64
		var gpuPct, temp, packagePowerW, energyWhTotal sql.NullFloat64
		var cpuPct, netRx, netTx, junctionTemp, cpuTemp, nvmeTemp sql.NullFloat64
		if err := rows.Scan(&ts, &gttUsed, &gttTotal, &gpuPct, &memUsed, &memTotal,
			&diskUsed, &diskTotal, &temp, &rss,
			&packagePowerW, &activeSlots, &energyWhTotal,
			&cpuPct, &netRx, &netTx, &junctionTemp, &cpuTemp, &nvmeTemp); err != nil {
			return nil, fmt.Errorf("store: metrics.range: %w", err)
		}
		s.TS = time.Unix(ts, 0).UTC()
		s.GTTUsedBytes = nullInt64Ptr(gttUsed)
		s.GTTTotalBytes = nullInt64Ptr(gttTotal)
		s.GPUUsePct = nullFloat64Ptr(gpuPct)
		s.MemUsedBytes = nullInt64Ptr(memUsed)
		s.MemTotalBytes = nullInt64Ptr(memTotal)
		s.DiskUsedBytes = nullInt64Ptr(diskUsed)
		s.DiskTotalBytes = nullInt64Ptr(diskTotal)
		s.TempCelsius = nullFloat64Ptr(temp)
		s.InferenceRSSBytes = nullInt64Ptr(rss)
		s.PackagePowerW = nullFloat64Ptr(packagePowerW)
		s.ActiveSlots = nullIntPtr(activeSlots)
		s.EnergyWhTotal = nullFloat64Ptr(energyWhTotal)
		s.CPUPct = nullFloat64Ptr(cpuPct)
		s.NetRxBytesPerSec = nullFloat64Ptr(netRx)
		s.NetTxBytesPerSec = nullFloat64Ptr(netTx)
		s.GPUJunctionTempC = nullFloat64Ptr(junctionTemp)
		s.CPUPackageTempC = nullFloat64Ptr(cpuTemp)
		s.NVMeTempC = nullFloat64Ptr(nvmeTemp)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: metrics.range: %w", err)
	}
	return out, nil
}

// Prune deletes rows older than cutoff (Sprint 0 §0.4 90-day retention,
// metrics.retention_days). Returns the number of rows removed.
func (v metricsView) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`DELETE FROM metric_samples WHERE ts < ?`, unixOf(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: metrics.prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: metrics.prune: %w", err)
	}
	return n, nil
}

// intPtrArg/floatPtrArg convert a nullable pointer field to a driver arg
// (nil pointer -> SQL NULL), matching the explicit-conversion convention
// this package already uses for nullable writes (see nullStr/nullUnix in
// impl.go) rather than relying on database/sql's reflection-based pointer
// handling.
func intPtrArg(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func floatPtrArg(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// intPtrArgInt is intPtrArg for the *int fields (ActiveSlots) — SQLite has
// no separate int/int64 storage class, so this is purely a Go-side type
// convenience, not a different wire representation.
func intPtrArgInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func nullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	x := int(v.Int64)
	return &x
}

func nullFloat64Ptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	x := v.Float64
	return &x
}
