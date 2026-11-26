package main

import (
	"math"
	"testing"
)

func TestBuildContainerLimitsRejectsBadValues(t *testing.T) {
	if _, err := buildContainerLimits(-1, "", 0); err == nil {
		t.Error("negative pids accepted")
	}
	if _, err := buildContainerLimits(0, "lots", 0); err == nil {
		t.Error("garbage memory accepted")
	}
	if _, err := buildContainerLimits(0, "", -1); err == nil {
		t.Error("negative cpus accepted")
	}
	if _, err := buildContainerLimits(0, "", math.NaN()); err == nil {
		t.Error("NaN cpus accepted (it would read as no ceiling)")
	}
	if _, err := buildContainerLimits(0, "", 0.001); err == nil {
		t.Error("cpus below docker's minimum accepted")
	}
	if _, err := buildContainerLimits(0, "1m", 0); err == nil {
		t.Error("memory below docker's minimum accepted")
	}
	l, err := buildContainerLimits(2048, "256m", 1.5)
	if err != nil || l.PidsLimit != 2048 || l.MemoryMaxBytes != 256<<20 || l.CPUsMax != 1.5 {
		t.Errorf("good values mangled: %+v %v", l, err)
	}
}
