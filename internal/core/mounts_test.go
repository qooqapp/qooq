package core

import (
	"errors"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

func TestValidateMounts(t *testing.T) {
	tests := []struct {
		name    string
		mounts  []runtime.Mount
		wantErr bool
	}{
		// nil is what every component stored before M23 decodes to; it must
		// stay valid or old records fail to redeploy.
		{name: "nil", mounts: nil},
		{name: "ok", mounts: []runtime.Mount{{Source: "/host/a", Target: "/app/a"}}},
		{name: "relative source", mounts: []runtime.Mount{{Source: "host/a", Target: "/app/a"}}, wantErr: true},
		{name: "relative target", mounts: []runtime.Mount{{Source: "/host/a", Target: "app/a"}}, wantErr: true},
		{name: "root target", mounts: []runtime.Mount{{Source: "/host/a", Target: "/"}}, wantErr: true},
		{name: "duplicate target", mounts: []runtime.Mount{
			{Source: "/host/a", Target: "/app/a"},
			{Source: "/host/b", Target: "/app/a"},
		}, wantErr: true},
		// Same target written two ways is still the same target.
		{name: "duplicate target unnormalized", mounts: []runtime.Mount{
			{Source: "/host/a", Target: "/app/a"},
			{Source: "/host/b", Target: "/app/./a"},
		}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMounts(tc.mounts)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				if !errors.Is(err, ErrInvalidMount) {
					t.Fatalf("error %v does not wrap ErrInvalidMount", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
