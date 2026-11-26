//go:build linux

package api

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// hostMemoryBytes returns total physical memory on Linux by reading the
// MemTotal line out of /proc/meminfo (reported in kB).
func hostMemoryBytes() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected /proc/meminfo MemTotal line: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse /proc/meminfo MemTotal: %w", err)
		}
		return kb * 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}
