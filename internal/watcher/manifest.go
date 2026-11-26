package watcher

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	qm "github.com/qooqapp/qooq/pkg/manifest"
)

// The manifest format itself lives in pkg/manifest so the CLI (a separate
// module, which cannot reach this package) can validate a qooq.yaml offline.
// These aliases keep the format's types spelled the way the rest of this
// package already spells them, so nothing else here had to change — the same
// arrangement types.go uses for the wire DTOs in pkg/watcherclient.
type (
	manifest               = qm.Manifest
	manifestIngress        = qm.Ingress
	manifestService        = qm.Service
	manifestServiceStorage = qm.ServiceStorage
	manifestBuild          = qm.Build
	manifestRun            = qm.Run
	manifestHealth         = qm.Health
	manifestPolicy         = qm.Policy
	manifestRequests       = qm.Requests
	manifestLimits         = qm.Limits
	manifestInput          = qm.Input
	manifestMount          = qm.Mount
)

// parseManifest and parseMountEntry are the package-local spellings of the
// pkg/manifest entry points.
func parseManifest(data []byte) (*manifest, error) { return qm.Parse(data) }

func parseMountEntry(s string) (manifestMount, error) { return qm.ParseMountEntry(s) }

// manifestIngressToSpec validates the ingress: block against the ports the app
// actually declares and converts it for the deploy request.
//
// The port check is the point: naming a port the container never opens
// produces a route that answers 502, and the cause of that is several layers
// away from where it is visible. Failing the build with the list of ports that
// do exist costs one deploy and answers the question outright.
func manifestIngressToSpec(in *manifestIngress, ports []portSpec) (*ingressSpec, error) {
	if in == nil {
		return nil, nil
	}
	if in.Port != 0 {
		found := false
		declared := make([]string, 0, len(ports))
		for _, p := range ports {
			declared = append(declared, strconv.Itoa(p.ContainerPort))
			if p.ContainerPort == in.Port {
				found = true
			}
		}
		if !found {
			if len(declared) == 0 {
				return nil, fmt.Errorf("ingress.port %d: this app declares no run.ports, so nothing is listening to route to", in.Port)
			}
			return nil, fmt.Errorf("ingress.port %d is not one of the app's run.ports (%s)", in.Port, strings.Join(declared, ", "))
		}
	}
	if in.Path != "" && !strings.HasPrefix(in.Path, "/") {
		return nil, fmt.Errorf("ingress.path %q: must start with %q", in.Path, "/")
	}
	if in.StripPath && in.Path == "" {
		return nil, fmt.Errorf("ingress.strip_path is set but there is no ingress.path to strip")
	}
	return &ingressSpec{
		Hosts:     in.Hosts,
		Port:      in.Port,
		Path:      in.Path,
		StripPath: in.StripPath,
		TLS:       in.TLS,
	}, nil
}

// manifestToDeploySpec converts the manifest's run/health/policy/limits
// sections into a deploySpecFields, the same shape target.deploy_spec parses
// into. This is the "manifest provides defaults" half of the M14 merge; the
// caller (mergeDeploySpec) then lets the operator's deploy_spec override
// any of these fields. limitsMultiplier is Watcher.limitsMultiplier (M22):
// it only matters for fields of m.Limits left unset, which default to that
// multiple of the corresponding m.Requests field — see deriveContainerLimits.
func manifestToDeploySpec(m *manifest, limitsMultiplier float64) (deploySpecFields, error) {
	var spec deploySpecFields
	spec.Command = m.Run.Command
	spec.Env = m.Run.Env
	spec.RestartPolicy = m.Policy.Restart
	spec.Domain = m.Domain
	spec.Network = m.Network

	for _, p := range m.Run.Ports {
		ps, err := parseManifestPort(p)
		if err != nil {
			return spec, fmt.Errorf("run.ports: %w", err)
		}
		spec.Ports = append(spec.Ports, ps)
	}

	ing, err := manifestIngressToSpec(m.Ingress, spec.Ports)
	if err != nil {
		return spec, err
	}
	spec.Ingress = ing

	hc, err := manifestHealthToSpec(m.Health)
	if err != nil {
		return spec, err
	}
	spec.HealthCheck = hc

	cpuLimit, memLimitBytes, err := deriveContainerLimits(m.Requests, m.Limits, limitsMultiplier)
	if err != nil {
		return spec, fmt.Errorf("limits: %w", err)
	}
	spec.CPULimit = cpuLimit
	spec.MemLimitBytes = memLimitBytes

	return spec, nil
}

// deriveContainerLimits (M22) computes the container's cgroup hard caps from
// the manifest's requests: (capacity-check-only) and limits: (enforced)
// sections. Per field: limits wins outright when set; otherwise, if the
// corresponding requests field is set, the cap defaults to multiplier times
// it (mirroring k8s's common "limits = N * requests" convention); otherwise
// the field stays 0, meaning unbounded — this is the pre-M22 behavior when
// neither section (or only requests, pre-M22's `resource:`) is present.
func deriveContainerLimits(req manifestRequests, lim manifestLimits, multiplier float64) (cpuCores float64, memBytes int64, err error) {
	switch {
	case lim.CPU > 0:
		cpuCores = lim.CPU
	case req.CPU > 0:
		cpuCores = req.CPU * multiplier
	}

	switch {
	case lim.Memory != "":
		memBytes, err = parseManifestMemBytes(lim.Memory)
		if err != nil {
			return 0, 0, fmt.Errorf("memory: %w", err)
		}
	case req.Memory != "":
		reqBytes, rerr := parseManifestMemBytes(req.Memory)
		if rerr != nil {
			return 0, 0, fmt.Errorf("requests.memory: %w", rerr)
		}
		memBytes = int64(float64(reqBytes) * multiplier)
	}

	return cpuCores, memBytes, nil
}

// manifestHealthToSpec validates the health section and converts it to the
// wire DTO. A present-but-ambiguous section is an error rather than a silent
// no-op: a typo'd probe must not deploy without its health check.
func manifestHealthToSpec(h manifestHealth) (*healthCheckSpec, error) {
	if h.HTTP == "" && len(h.Exec) == 0 && h.Port == 0 &&
		h.Interval == "" && h.Timeout == "" && h.Delay == "" && h.Failures == 0 {
		return nil, nil
	}
	if h.HTTP != "" && len(h.Exec) > 0 {
		return nil, fmt.Errorf("health: http and exec are mutually exclusive")
	}
	if h.HTTP == "" && len(h.Exec) == 0 {
		return nil, fmt.Errorf("health: one of http (probe path) or exec (probe command) is required")
	}
	hc := &healthCheckSpec{}
	if h.HTTP != "" {
		if err := qm.ValidateHealthPath(h.HTTP); err != nil {
			return nil, err
		}
		if h.Port == 0 {
			return nil, fmt.Errorf("health: http probe requires port (the published host port)")
		}
		hc.Type = "http"
		hc.HTTPPath = h.HTTP
		hc.HTTPPort = h.Port
	} else {
		hc.Type = "exec"
		hc.ExecCommand = h.Exec
	}
	var err error
	if hc.IntervalSeconds, err = manifestDurationSeconds("health.interval", h.Interval); err != nil {
		return nil, err
	}
	if hc.TimeoutSeconds, err = manifestDurationSeconds("health.timeout", h.Timeout); err != nil {
		return nil, err
	}
	if hc.InitialDelaySeconds, err = manifestDurationSeconds("health.delay", h.Delay); err != nil {
		return nil, err
	}
	if h.Failures < 0 {
		return nil, fmt.Errorf("health.failures: must not be negative")
	}
	hc.FailureThreshold = h.Failures
	return hc, nil
}

// manifestDurationSeconds parses a duration string ("10s", "1m") into whole
// seconds. Empty means unset (0 — qooqd applies its defaults).
func manifestDurationSeconds(field, s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", field, s, err)
	}
	secs := int(d.Seconds())
	if secs < 1 {
		return 0, fmt.Errorf("%s: %q is below the 1s resolution", field, s)
	}
	return secs, nil
}

// parseManifestPort maps the format's port string onto the watcher's wire DTO.
func parseManifestPort(s string) (portSpec, error) {
	p, err := qm.ParsePort(s)
	if err != nil {
		return portSpec{}, err
	}
	return portSpec{ContainerPort: p.Container, HostPort: p.Host, AutoHost: p.AutoHost}, nil
}

// mergeDeploySpec applies base (manifest-derived defaults) then overlays
// override (the operator's target.deploy_spec) field-by-field: any
// non-zero-valued field in override wins. This is the M14 merge policy,
// deliberately safety-first — the repo (manifest) is a half-trusted input,
// the operator has the final say. When override has no manifest to merge
// with (mf == nil in the caller), base is the zero value and this is a
// no-op identity function, i.e. M12 behavior is unchanged.
func mergeDeploySpec(base, override deploySpecFields) deploySpecFields {
	out := base
	if len(override.Command) > 0 {
		out.Command = override.Command
	}
	// Env merges per key rather than as one field: a stack's env or an
	// operator's single variable used to replace the manifest's whole
	// run.env, so every variable the repo set silently went missing.
	if len(override.Env) > 0 {
		env := make(map[string]string, len(base.Env)+len(override.Env))
		for k, v := range base.Env {
			env[k] = v
		}
		for k, v := range override.Env {
			env[k] = v
		}
		out.Env = env
	}
	if len(override.Ports) > 0 {
		out.Ports = override.Ports
	}
	if len(override.Mounts) > 0 {
		out.Mounts = override.Mounts
	}
	if override.Network != "" {
		out.Network = override.Network
	}
	if override.Namespace != "" {
		out.Namespace = override.Namespace
	}
	if override.GroupID != "" {
		out.GroupID = override.GroupID
	}
	if override.RestartPolicy != "" {
		out.RestartPolicy = override.RestartPolicy
	}
	if override.CPULimit != 0 {
		out.CPULimit = override.CPULimit
	}
	if override.MemLimitBytes != 0 {
		out.MemLimitBytes = override.MemLimitBytes
	}
	if override.Domain != "" {
		out.Domain = override.Domain
	}
	// Replaced wholesale rather than field-by-field: an operator overriding
	// routing is describing where the app should be reachable, and merging
	// half of their block with half of the repo's would produce a third thing
	// neither of them asked for.
	if override.Ingress != nil {
		out.Ingress = override.Ingress
	}
	if override.HealthCheck != nil {
		out.HealthCheck = override.HealthCheck
	}
	return out
}

// parseManifestMemBytes parses a memory quantity such as "256Mi", "1Gi",
// "512Ki" (binary/k8s-style units), "256MB"/"1GB"/"512KB" (decimal,
// 1000-based SI units, F9/M17 review), the 1024-based "64m"/"1g"/"512k"
// convention used by cmd/qooq/component.go's parseMemBytes, or a bare byte
// count. Logic is adapted from that function (same 1024-based unit
// convention) and extended with the Ki/Mi/Gi and decimal KB/MB/GB suffixes
// app manifests typically use. Longer suffixes (KiB/MiB/GiB, KB/MB/GB) are
// matched before their shorter prefixes (Ki/Mi/Gi, single-letter b/B) so
// e.g. "1KiB" doesn't get mis-parsed by the "b"/"B" case as "1Ki" + a
// dropped byte-unit char.
func parseManifestMemBytes(s string) (int64, error) {
	orig := s
	if s == "" {
		return 0, nil
	}
	unit := int64(1)
	switch {
	case strings.HasSuffix(s, "KiB"):
		unit, s = 1024, strings.TrimSuffix(s, "KiB")
	case strings.HasSuffix(s, "MiB"):
		unit, s = 1024*1024, strings.TrimSuffix(s, "MiB")
	case strings.HasSuffix(s, "GiB"):
		unit, s = 1024*1024*1024, strings.TrimSuffix(s, "GiB")
	case strings.HasSuffix(s, "Ki"):
		unit, s = 1024, strings.TrimSuffix(s, "Ki")
	case strings.HasSuffix(s, "Mi"):
		unit, s = 1024*1024, strings.TrimSuffix(s, "Mi")
	case strings.HasSuffix(s, "Gi"):
		unit, s = 1024*1024*1024, strings.TrimSuffix(s, "Gi")
	case strings.HasSuffix(s, "KB"):
		unit, s = 1000, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		unit, s = 1000*1000, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		unit, s = 1000*1000*1000, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		unit, s = 1024, s[:len(s)-1]
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "M"):
		unit, s = 1024*1024, s[:len(s)-1]
	case strings.HasSuffix(s, "g"), strings.HasSuffix(s, "G"):
		unit, s = 1024*1024*1024, s[:len(s)-1]
	case strings.HasSuffix(s, "b"), strings.HasSuffix(s, "B"):
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory quantity %q: %w", orig, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("invalid memory quantity %q: must not be negative", orig)
	}
	return int64(f * float64(unit)), nil
}
