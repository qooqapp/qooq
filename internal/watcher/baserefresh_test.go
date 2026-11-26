package watcher

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDockerfileBases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Dockerfile")
	content := "# syntax=docker/dockerfile:1\n" +
		"ARG GO=1.25\n" +
		"FROM --platform=$BUILDPLATFORM golang:${GO}-alpine AS build\n" +
		"FROM node:20-alpine AS web\n" +
		"from build AS test\n" +
		"FROM scratch AS empty\n" +
		"FROM nginx:alpine\n" +
		"COPY --from=web /app /usr/share/nginx/html\n" +
		"FROM node:20-alpine\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := dockerfileBases(path)
	if err != nil {
		t.Fatal(err)
	}
	// golang is built from an ARG, build is an earlier stage, scratch has
	// nothing to pull, and node appears once.
	if want := []string{"node:20-alpine", "nginx:alpine"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bases = %v, want %v", got, want)
	}
}

// Each base at most once per interval, however many builds use it.
func TestBaseRefresherPullsOncePerInterval(t *testing.T) {
	b := newBaseRefresher(time.Hour)
	now := time.Now()
	if !b.due("nginx:alpine", now) {
		t.Fatal("a base never pulled is not due")
	}
	if b.due("nginx:alpine", now.Add(59*time.Minute)) {
		t.Error("pulled again inside the interval")
	}
	if !b.due("node:20", now.Add(time.Minute)) {
		t.Error("another base is held back by the first")
	}
	if !b.due("nginx:alpine", now.Add(61*time.Minute)) {
		t.Error("not due again after the interval")
	}
	if newBaseRefresher(0).due("x", now) || (*baseRefresher)(nil).due("x", now) {
		t.Error("an interval of 0 (or no refresher) must disable it")
	}
}
