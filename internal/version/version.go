// Package version reports which build of qooq is running.
package version

import "runtime/debug"

// Version and Commit are set at build time with -ldflags -X (see the Makefile
// and .goreleaser.yaml). The defaults are what an unadorned `go build`
// produces, and saying "dev" is the honest answer for one: a binary built from
// a working tree is not any released version.
//
// They are variables rather than constants for exactly that reason — a
// constant cannot be injected, which is how every build ended up claiming to
// be 0.1.0 regardless of what it actually contained.
var (
	Version = "dev"
	Commit  = ""
)

// String describes this build for a human: the version, and the commit when
// one is known and not already implied by the version.
func String() string {
	v := Version
	if c := commit(); c != "" {
		v += " (" + c + ")"
	}
	return v
}

// commit falls back to the revision Go records in the binary. `go install
// module@version` sets no ldflags, so without this a binary installed that way
// could not say what it was built from.
func commit() string {
	if Commit != "" {
		return short(Commit)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return short(s.Value)
		}
	}
	return ""
}

func short(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
