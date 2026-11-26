package docker

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
)

// What a hardened container (runtime.ContainerSpec.Harden) gets, on top of
// docker's defaults. The occasion was NGINX2Root (2026-11-18): it shipped a
// Linux SCTP privilege escalation that went from inside a container to root
// on the host, with no kernel fix. A user runs arbitrary code, so the nginx
// half is not needed to reach it. Blocking the sctp module on the host is the
// first line (preflight 5c); this is the one that holds when a machine was
// set up without it.

// hardenedCapDrop removes capabilities docker grants by default that an app
// serving requests has no use for. NET_RAW is the one that matters most —
// raw sockets are where a long line of kernel escalations start. CHOWN,
// SETUID, SETGID, DAC_OVERRIDE, FOWNER and NET_BIND_SERVICE stay: the
// official postgres, redis and nginx images need them to drop to their own
// user and bind their port.
var hardenedCapDrop = []string{"NET_RAW", "MKNOD", "AUDIT_WRITE", "SYS_CHROOT", "SETFCAP"}

// seccompDefault is moby/profiles seccomp/default.json at tag seccomp/v0.3.0
// (commit 19b749451e67), byte for byte. Keep it untouched: an update is a
// replacement of this file, and hardenProfile below fails the build of the
// profile — not silently passes — if the rules it rewrites are no longer
// shaped the way it expects.
//
//go:embed seccomp-default.json
var seccompDefault []byte

const (
	afINET      = 2
	afINET6     = 10
	ipprotoSCTP = 132
	sockTypeMax = 0xf // the low bits of socket()'s type; the rest are SOCK_NONBLOCK/SOCK_CLOEXEC
)

var (
	hardenedOnce sync.Once
	hardenedJSON string
	hardenedErr  error
)

// hardenedSeccomp is the profile passed as seccomp=<json>, built once.
func hardenedSeccomp() (string, error) {
	hardenedOnce.Do(func() {
		var b []byte
		b, hardenedErr = hardenProfile(seccompDefault)
		hardenedJSON = string(b)
	})
	return hardenedJSON, hardenedErr
}

// hardenProfile rewrites docker's default profile so no container can open
// an SCTP socket — which is also what would load the module on demand:
//
//   - The default allows socket() by address family, and AF_INET/AF_INET6
//     with any protocol. Those two become: type STREAM, DGRAM or RAW (flags
//     masked off), and protocol not IPPROTO_SCTP. SEQPACKET goes too, since
//     with protocol 0 it is the other way to get SCTP on an inet socket.
//   - socketcall is dropped, and the 32-bit ABIs with it (see the end of the
//     function): 32-bit processes create sockets through socketcall, whose
//     arguments seccomp cannot see (they are behind a pointer).
//
// Every other rule is kept as is. Docker allowlists: a syscall is permitted
// if any rule allows it, so a separate deny rule could not override the
// existing allows — the allows themselves have to be narrowed.
func hardenProfile(def []byte) ([]byte, error) {
	var prof map[string]any
	if err := json.Unmarshal(def, &prof); err != nil {
		return nil, fmt.Errorf("parse default seccomp profile: %w", err)
	}
	rules, ok := prof["syscalls"].([]any)
	if !ok {
		return nil, fmt.Errorf("default seccomp profile has no syscalls list")
	}

	var out []any
	var sawLT3, saw10, sawSocketcall bool
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("default seccomp profile: a syscalls entry is not an object")
		}
		names := stringList(rule["names"])

		if len(names) == 1 && names[0] == "socket" {
			op, val, ok := singleArg0(rule)
			switch {
			case ok && op == "SCMP_CMP_LT" && val == 3:
				// AF_UNSPEC, AF_UNIX, AF_INET: keep the first two as they were.
				sawLT3 = true
				out = append(out, socketRule([]any{argCmp(0, "SCMP_CMP_LT", afINET, 0)}))
				out = append(out, inetSocketRules(afINET)...)
				continue
			case ok && op == "SCMP_CMP_EQ" && val == afINET6:
				saw10 = true
				out = append(out, inetSocketRules(afINET6)...)
				continue
			}
		}

		if i := indexOf(names, "socketcall"); i >= 0 {
			sawSocketcall = true
			kept := append(append([]string{}, names[:i]...), names[i+1:]...)
			if len(kept) == 0 {
				continue
			}
			copied := make(map[string]any, len(rule))
			for k, v := range rule {
				copied[k] = v
			}
			copied["names"] = kept
			out = append(out, copied)
			continue
		}
		out = append(out, rule)
	}
	if !sawLT3 || !saw10 || !sawSocketcall {
		return nil, fmt.Errorf("default seccomp profile is not shaped as expected (socket family<3: %v, family==10: %v, socketcall: %v); review harden.go against the new profile",
			sawLT3, saw10, sawSocketcall)
	}
	prof["syscalls"] = out

	// Removing socketcall from the list is not enough on its own, measured:
	// on 32-bit x86 libseccomp turns an allow rule for socket into an allow
	// for socketcall(SYS_SOCKET), whose arguments it cannot see, so a 32-bit
	// process opened a socket the 64-bit filter refused. The only way to
	// close that path is to not admit the 32-bit ABIs at all: a 32-bit
	// program in a hardened container is killed (SIGSYS) on its first
	// syscall. Container images are 64-bit nearly without exception.
	sawX86 := false
	arches, _ := prof["archMap"].([]any)
	for _, a := range arches {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		var kept []string
		for _, sub := range stringList(m["subArchitectures"]) {
			if sub == "SCMP_ARCH_X86" {
				sawX86 = true
			}
			if strings.Contains(sub, "64") {
				kept = append(kept, sub)
			}
		}
		m["subArchitectures"] = kept
	}
	if !sawX86 {
		return nil, fmt.Errorf("default seccomp profile has no x86 sub-architecture to remove; review harden.go against the new profile")
	}
	return json.Marshal(prof)
}

// HardeningReady reports whether the hardened profile can be built from the
// embedded default. qooqd checks it at startup when hardening is on.
func HardeningReady() error {
	_, err := hardenedSeccomp()
	return err
}

// hardenHostConfig applies the hardening to a container about to be created.
// Split out so what a hardened container gets can be tested without a daemon.
func hardenHostConfig(h *container.HostConfig) error {
	prof, err := hardenedSeccomp()
	if err != nil {
		return fmt.Errorf("hardened seccomp profile: %w", err)
	}
	h.SecurityOpt = append(h.SecurityOpt, "no-new-privileges:true", "seccomp="+prof)
	h.CapDrop = append(h.CapDrop, hardenedCapDrop...)
	return nil
}

// inetSocketRules allows socket(family, type, protocol) for the stream,
// datagram and raw types with any protocol but SCTP. Conditions within one
// rule are ANDed.
func inetSocketRules(family int) []any {
	var rs []any
	for _, typ := range []int{1, 2, 3} { // SOCK_STREAM, SOCK_DGRAM, SOCK_RAW
		rs = append(rs, socketRule([]any{
			argCmp(0, "SCMP_CMP_EQ", family, 0),
			argCmp(1, "SCMP_CMP_MASKED_EQ", sockTypeMax, typ),
			argCmp(2, "SCMP_CMP_NE", ipprotoSCTP, 0),
		}))
	}
	return rs
}

func socketRule(args []any) map[string]any {
	return map[string]any{"names": []string{"socket"}, "action": "SCMP_ACT_ALLOW", "args": args}
}

// argCmp is one condition. For SCMP_CMP_MASKED_EQ docker reads value as the
// mask and valueTwo as what the masked argument must equal.
func argCmp(index int, op string, value, valueTwo int) map[string]any {
	return map[string]any{"index": index, "value": value, "valueTwo": valueTwo, "op": op}
}

// singleArg0 returns the comparison of a rule that has exactly one condition,
// on argument 0.
func singleArg0(rule map[string]any) (string, float64, bool) {
	args, ok := rule["args"].([]any)
	if !ok || len(args) != 1 {
		return "", 0, false
	}
	a, ok := args[0].(map[string]any)
	if !ok {
		return "", 0, false
	}
	idx, _ := a["index"].(float64)
	op, _ := a["op"].(string)
	val, ok := a["value"].(float64)
	if !ok || idx != 0 {
		return "", 0, false
	}
	return op, val, true
}

func stringList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}
