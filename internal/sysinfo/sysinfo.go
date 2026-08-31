// Package sysinfo reads lightweight host stats (uptime, memory, disk, CPU
// temperature) for display on the OLED status screen.
package sysinfo

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Stats holds the values the OLED status line needs. Zero-value Uptime means
// a read failed (e.g. /proc unavailable), letting callers distinguish "no
// data" from "device just booted".
type Stats struct {
	Uptime      time.Duration
	MemPercent  int // 0-100
	DiskPercent int // 0-100, root filesystem
	// CPUTempC is the SoC temperature in degrees Celsius, read from the
	// kernel's thermal_zone0 sysfs node. 0 means "unavailable" (missing
	// thermal_zone0, e.g. off-Pi development), which a real reading never
	// naturally produces.
	CPUTempC float64
}

// Read gathers whatever stats are available, leaving individual fields at
// their zero value on a per-metric read failure rather than failing outright
// - the status screen degrades gracefully rather than losing every stat
// because one proc file was briefly unreadable.
func Read() Stats {
	var st Stats
	if up, err := readUptime(); err == nil {
		st.Uptime = up
	}
	if pct, err := readMemPercent(); err == nil {
		st.MemPercent = pct
	}
	if pct, err := readDiskPercent("/"); err == nil {
		st.DiskPercent = pct
	}
	if c, err := readCPUTemp(); err == nil {
		st.CPUTempC = c
	}
	return st
}

func readUptime() (time.Duration, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected /proc/uptime format")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func readMemPercent() (int, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var totalKB, availKB uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = parseMeminfoValue(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB = parseMeminfoValue(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if totalKB == 0 {
		return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	return int((totalKB - availKB) * 100 / totalKB), nil
}

func parseMeminfoValue(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

func readDiskPercent(path string) (int, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	if total == 0 {
		return 0, fmt.Errorf("statfs reported zero total blocks")
	}
	return int((total - free) * 100 / total), nil
}

// readCPUTemp reads the SoC temperature from the kernel's thermal sysfs node.
// thermal_zone0 reports in milli-degrees Celsius, so the raw value is divided
// by 1000 to get whole-plus-fractional degrees.
func readCPUTemp() (float64, error) {
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0, err
	}
	milliC, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, err
	}
	return milliC / 1000, nil
}
