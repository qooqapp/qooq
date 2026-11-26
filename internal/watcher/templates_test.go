package watcher

import (
	"strings"
	"testing"
)

func TestRenderDockerfileDefaults(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"static-nginx", "FROM nginx:alpine"},
		{"go", "FROM golang:1.25-alpine AS build"},
		{"node", "FROM node:20-alpine"},
		{"python", "FROM python:3.12-slim"},
		{"go-react", "FROM node:20-alpine AS webbuild"},
	}
	for _, c := range cases {
		out, err := renderDockerfile(c.name, nil, "")
		if err != nil {
			t.Fatalf("renderDockerfile(%q, nil): %v", c.name, err)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("renderDockerfile(%q) = %q, want to contain %q", c.name, out, c.want)
		}
	}
}

func TestRenderDockerfileUserVarsOverrideDefaults(t *testing.T) {
	out, err := renderDockerfile("go", map[string]string{"go_version": "1.24"}, "")
	if err != nil {
		t.Fatalf("renderDockerfile: %v", err)
	}
	if !strings.Contains(out, "FROM golang:1.24-alpine AS build") {
		t.Errorf("go_version override not applied: %q", out)
	}
	// Unrelated defaults still present.
	if !strings.Contains(out, "EXPOSE 8080") {
		t.Errorf("unrelated default (port) should still apply: %q", out)
	}
}

func TestRenderDockerfileUnknownTemplate(t *testing.T) {
	_, err := renderDockerfile("does-not-exist", nil, "")
	if err == nil {
		t.Fatal("unknown template name should error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should mention the bad name: %v", err)
	}
	// Should list available templates so the operator can fix the typo.
	if !strings.Contains(err.Error(), "go") {
		t.Errorf("error should list available templates: %v", err)
	}
}

func TestRenderTemplateStringMissingKeyErrors(t *testing.T) {
	_, err := renderTemplateString("FROM {{.unknown_var}}", map[string]string{"other": "x"})
	if err == nil {
		t.Fatal("missing template variable should error, not render <no value>")
	}
}

func TestRenderTemplateStringKnownKeys(t *testing.T) {
	out, err := renderTemplateString("FROM {{.image}}", map[string]string{"image": "alpine:3.20"})
	if err != nil {
		t.Fatalf("renderTemplateString: %v", err)
	}
	if out != "FROM alpine:3.20" {
		t.Errorf("got %q", out)
	}
}
