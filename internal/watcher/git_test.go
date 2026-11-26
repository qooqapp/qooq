package watcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSSHUserFromURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"scp-style default git", "git@github.com:org/repo.git", "git"},
		{"scp-style custom user", "gitea@host.example.com:org/repo.git", "gitea"},
		{"ssh scheme custom user", "ssh://deploy@host.example.com/org/repo.git", "deploy"},
		{"ssh scheme no user", "ssh://host.example.com/org/repo.git", "git"},
		{"https no user", "https://github.com/org/repo.git", "git"},
		{"empty", "", "git"},
		{"garbage", "not a url ::::", "git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sshUserFromURL(c.url); got != c.want {
				t.Errorf("sshUserFromURL(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}

func TestSSHHostKeyPolicyCallback(t *testing.T) {
	// Default (both zero): nil callback, nil error — go-git applies its own
	// strict default.
	t.Run("default is nil callback", func(t *testing.T) {
		cb, err := sshHostKeyPolicy{}.callback()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cb != nil {
			t.Fatal("expected nil callback for zero policy")
		}
	})

	// Insecure: non-nil callback that ignores host keys.
	t.Run("insecure returns callback", func(t *testing.T) {
		cb, err := sshHostKeyPolicy{Insecure: true}.callback()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cb == nil {
			t.Fatal("expected non-nil callback for insecure policy")
		}
	})

	// KnownHostsPath: reads the file and returns a verifying callback.
	t.Run("known_hosts path returns callback", func(t *testing.T) {
		dir := t.TempDir()
		khPath := filepath.Join(dir, "known_hosts")
		// A syntactically valid known_hosts line (ed25519 host key entry).
		line := "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"
		if err := os.WriteFile(khPath, []byte(line), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
		cb, err := sshHostKeyPolicy{KnownHostsPath: khPath}.callback()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cb == nil {
			t.Fatal("expected non-nil callback for known_hosts policy")
		}
	})

	// Insecure takes precedence over a (here nonexistent) known_hosts path.
	t.Run("insecure wins over known_hosts path", func(t *testing.T) {
		cb, err := sshHostKeyPolicy{KnownHostsPath: "/nonexistent/known_hosts", Insecure: true}.callback()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cb == nil {
			t.Fatal("expected non-nil callback")
		}
	})
}
