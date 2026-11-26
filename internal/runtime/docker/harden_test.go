package docker

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

type seccompArg struct {
	Index    int    `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo"`
	Op       string `json:"op"`
}

type seccompRule struct {
	Names  []string     `json:"names"`
	Action string       `json:"action"`
	Args   []seccompArg `json:"args"`
}

func rulesOf(t *testing.T, b []byte) []seccompRule {
	t.Helper()
	var p struct {
		DefaultAction string        `json:"defaultAction"`
		Syscalls      []seccompRule `json:"syscalls"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatalf("defaultAction = %q: the rewrite assumes an allowlist", p.DefaultAction)
	}
	return p.Syscalls
}

// allows evaluates the socket() rules the way the kernel filter does: allowed
// if any ALLOW rule's conditions all hold.
func allows(rules []seccompRule, family, typ, proto uint64) bool {
	argv := []uint64{family, typ, proto}
	for _, r := range rules {
		if r.Action != "SCMP_ACT_ALLOW" || !slices.Contains(r.Names, "socket") {
			continue
		}
		ok := true
		for _, a := range r.Args {
			v := argv[a.Index]
			switch a.Op {
			case "SCMP_CMP_EQ":
				ok = ok && v == a.Value
			case "SCMP_CMP_NE":
				ok = ok && v != a.Value
			case "SCMP_CMP_LT":
				ok = ok && v < a.Value
			case "SCMP_CMP_MASKED_EQ":
				ok = ok && v&a.Value == a.ValueTwo
			default:
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestHardenedProfileRefusesSCTPAndNothingElseItShouldNot(t *testing.T) {
	b, err := hardenProfile(seccompDefault)
	if err != nil {
		t.Fatal(err)
	}
	hard, def := rulesOf(t, b), rulesOf(t, seccompDefault)
	const nonblock = 0x800
	cases := []struct {
		name                string
		family, typ, proto  uint64
		wantDefault, wantHd bool
	}{
		{"unix stream", 1, 1, 0, true, true},
		{"tcp", 2, 1, 0, true, true},
		{"tcp nonblock", 2, 1 | nonblock, 6, true, true},
		{"udp", 2, 2, 0, true, true},
		{"tcp6", 10, 1, 0, true, true},
		{"udp6", 10, 2, 17, true, true},
		{"netlink", 16, 3, 0, true, true},
		{"sctp one-to-one", 2, 1, 132, true, false},
		{"sctp one-to-many", 2, 5, 132, true, false},
		{"seqpacket proto 0", 2, 5, 0, true, false},
		{"sctp6", 10, 1 | nonblock, 132, true, false},
	}
	for _, c := range cases {
		if got := allows(def, c.family, c.typ, c.proto); got != c.wantDefault {
			t.Errorf("%s: docker's default allows=%v, want %v (the premise of the test)", c.name, got, c.wantDefault)
		}
		if got := allows(hard, c.family, c.typ, c.proto); got != c.wantHd {
			t.Errorf("%s: hardened allows=%v, want %v", c.name, got, c.wantHd)
		}
	}
	for _, r := range hard {
		if slices.Contains(r.Names, "socketcall") {
			t.Error("socketcall is still allowed: a 32-bit process can open SCTP through it")
		}
	}
	// Nothing else lost: every other allowed syscall name survives.
	names := func(rs []seccompRule) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			for _, n := range r.Names {
				m[n] = true
			}
		}
		return m
	}
	hn := names(hard)
	for n := range names(def) {
		if n != "socketcall" && !hn[n] {
			t.Errorf("%s was allowed by docker's default and is not any more", n)
		}
	}
}

// An updated upstream profile that no longer has the rules being rewritten
// must stop the hardening, not pass it through unchanged.
func TestHardeningRefusesAProfileOfAnotherShape(t *testing.T) {
	changed := strings.Replace(string(seccompDefault), `"socketcall",`, ``, 1)
	if changed == string(seccompDefault) {
		t.Fatal("test setup: socketcall not found in the profile")
	}
	if _, err := hardenProfile([]byte(changed)); err == nil {
		t.Error("accepted a profile it could not fully rewrite")
	}
}

func TestHardenHostConfig(t *testing.T) {
	var h container.HostConfig
	if err := hardenHostConfig(&h); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", h.SecurityOpt)
	}
	var seccomp bool
	for _, o := range h.SecurityOpt {
		seccomp = seccomp || strings.HasPrefix(o, "seccomp={")
	}
	if !seccomp {
		t.Error("no inline seccomp profile")
	}
	for _, c := range []string{"NET_RAW", "MKNOD"} {
		if !slices.Contains([]string(h.CapDrop), c) {
			t.Errorf("CapDrop = %v, want %s", h.CapDrop, c)
		}
	}
	for _, c := range []string{"CHOWN", "SETUID", "SETGID", "NET_BIND_SERVICE"} {
		if slices.Contains([]string(h.CapDrop), c) {
			t.Errorf("dropped %s, which the official images need", c)
		}
	}
}

// The 32-bit ABIs are not admitted: measured, a 32-bit process got a socket
// through socketcall that the same filter refused a 64-bit one.
func TestHardenedProfileAdmitsNo32BitABI(t *testing.T) {
	b, err := hardenProfile(seccompDefault)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		ArchMap []struct {
			Architecture     string   `json:"architecture"`
			SubArchitectures []string `json:"subArchitectures"`
		} `json:"archMap"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	for _, a := range p.ArchMap {
		for _, s := range a.SubArchitectures {
			if !strings.Contains(s, "64") {
				t.Errorf("%s still admits %s", a.Architecture, s)
			}
		}
		if a.Architecture == "SCMP_ARCH_X86_64" && len(a.SubArchitectures) != 0 {
			t.Errorf("x86_64 still has sub-architectures %v", a.SubArchitectures)
		}
	}
}
