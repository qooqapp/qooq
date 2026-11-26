//go:build linux

package process

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicksPerSec is USER_HZ, standard (getconf CLK_TCK) on virtually every
// modern Linux kernel/config; hardcoding it avoids a cgo dependency on
// sysconf(3) just for this one constant.
const clockTicksPerSec = 100

// cpuSample is the previous-tick state sampleUsage needs to compute a CPU%
// delta.
type cpuSample struct {
	ticks uint64
	at    time.Time
}

func readProcUsage(pid int) (cpuTicks uint64, rssBytes uint64, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, 0, false
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 13 { // utime=field14=fields[11], stime=field15=fields[12]
		return 0, 0, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	cpuTicks = utime + stime

	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return cpuTicks, 0, true
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) >= 2 {
			if kb, err := strconv.ParseUint(fs[1], 10, 64); err == nil {
				rssBytes = kb * 1024
			}
		}
		break
	}
	return cpuTicks, rssBytes, true
}

// sampleUsage returns a CPU%/RSS sample for pid, using prev as the previous
// sample for delta computation (prev's zero value is treated as "no prior
// sample yet", reporting 0% for the first tick).
func sampleUsage(pid int, prev cpuSample) (cpuPercent float64, memBytes uint64, cur cpuSample, ok bool) {
	ticks, rss, ok := readProcUsage(pid)
	if !ok {
		return 0, 0, prev, false
	}
	now := time.Now()
	cur = cpuSample{ticks: ticks, at: now}
	if prev.at.IsZero() {
		return 0, rss, cur, true
	}
	wallSeconds := now.Sub(prev.at).Seconds()
	if wallSeconds <= 0 || ticks < prev.ticks {
		return 0, rss, cur, true
	}
	cpuSeconds := float64(ticks-prev.ticks) / clockTicksPerSec
	return cpuSeconds / wallSeconds * 100, rss, cur, true
}
