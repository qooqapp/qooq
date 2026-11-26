// Package manifest parses qooq.yaml, the app manifest a repo carries to say
// what its code needs in order to run.
//
// It lives in pkg/ rather than beside the watcher that consumes it so that the
// CLI — a separate module, which cannot import this one's internal packages at
// all — can validate a manifest without a running daemon. A manifest mistake
// otherwise surfaces only after commit, push, poll and build, which is minutes
// of feedback loop for a typo, and the error arrives somewhere the author is
// no longer looking.
//
// The watcher keeps the half of the old file that is about deploying: turning
// a parsed manifest into a deploy request, and merging the operator's
// deploy_spec over it. What moved here is only what "is this manifest valid?"
// needs, so there is still exactly one definition of the format.
package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is the M14 app-manifest format (qooq.yaml, version 1): an
// app-centric description of what a repo's code needs to run, written by
// the developer and living in the repo itself. It is parsed by the watcher
// and merged into the deploy request, with target.deploy_spec (the
// operator's config) winning field-by-field — see mergeDeploySpec.
//
// Unknown top-level/nested keys are tolerated (yaml.v3's default
// Unmarshal behavior, no KnownFields strictness) so older watchers don't
// hard-fail on a manifest written for a newer version.
type Manifest struct {
	Version int    `yaml:"version"`
	App     string `yaml:"app"`
	// Description is pure metadata (like App): not merged into the deploy
	// spec, not validated, just a human-readable note about what this app
	// is — carried for the developer's/operator's own benefit.
	Description string `yaml:"description"`
	// Domain is the public hostname the app is meant to be served on
	// (e.g. "app.example.com"). Declarative metadata in v1: passed through
	// to the component spec and echoed by qooqd, not routed — a future
	// ingress/reverse-proxy feature consumes it.
	Domain string `yaml:"domain"`
	// Ingress refines how the reverse proxy exposes this app: extra
	// hostnames, which port to forward to when there are several, a path
	// prefix so two apps can share a hostname. Omitting it keeps the
	// defaults, which is what every manifest written before it existed gets.
	Ingress *Ingress `yaml:"ingress"`
	// Network (M23) is the container network this app joins. Omitted means
	// a private per-app network ("qooq-<app>"): the app and any services it
	// declares can talk, nothing else can. Naming a shared network is how
	// separately-deployed apps reach each other by component name.
	Network string `yaml:"network"`
	// Services (M23) are dependency containers qooq deploys and manages
	// alongside the app — a database, a cache. Each entry becomes an ordinary
	// component named "<app-component>-<key>", so it can be inspected,
	// stopped or rolled back like anything else; declaring it here just means
	// the app's repo describes what it needs instead of an operator
	// remembering to deploy it.
	Services map[string]Service `yaml:"services"`
	// Inputs are values this app needs that its own repo cannot know: the
	// address a browser reaches it on, an API key, a domain. Declaring them
	// turns "deploy failed: unknown reference ${input.STRIPE_KEY}" into "this
	// deployment needs STRIPE_KEY, here is what it is for" — the difference
	// between an error and a question. Referenced as ${input.NAME}; see
	// inputs.go.
	Inputs   map[string]Input `yaml:"inputs"`
	Build    Build            `yaml:"build"`
	Run      Run              `yaml:"run"`
	Health   Health           `yaml:"health"`
	Policy   Policy           `yaml:"policy"`
	Requests Requests         `yaml:"requests"`
	Limits   Limits           `yaml:"limits"`
	// LegacyRequires exists only to detect the pre-M21 `requires:` key
	// (renamed to `resource:`) and produce a clear migration error instead
	// of silently ignoring it — see Parse. It is never read for its
	// value.
	LegacyRequires yaml.Node `yaml:"requires"`
	// LegacyResource exists only to detect the pre-M22 `resource:` key
	// (split into `requests:`/`limits:`) and produce a clear migration error
	// instead of silently ignoring it — see Parse. It is never read
	// for its value.
	LegacyResource yaml.Node `yaml:"resource"`
}

// Ingress is the ingress: block. Every field is optional.
type Ingress struct {
	// Hosts are additional hostnames, on top of the ones qooq derives from
	// the component name and the domain: shorthand.
	Hosts []string `yaml:"hosts"`
	// Port picks which container port to forward to. It must be one the app
	// declares in run.ports — see manifestToDeploySpec, which rejects a port
	// the container will never open rather than letting it 502 later.
	Port int `yaml:"port"`
	// Path routes only a URL prefix here, so two apps can share a hostname.
	Path string `yaml:"path"`
	// StripPath removes Path before forwarding. The default passes the URL
	// through unchanged; set it when the app serves from its own root.
	StripPath bool `yaml:"strip_path"`
	// TLS overrides the daemon's -ingress-tls for this app. A pointer so
	// "not specified" is distinguishable from "explicitly off".
	TLS *bool `yaml:"tls"`
}

// Service is one entry of the services: block. Every field is
// optional except on type "custom": `type: postgres` alone is a complete
// declaration, and the defaults table (serviceDefaults in services.go)
// supplies the image, port, data path, bootstrap environment and health probe.
type Service struct {
	// Type selects a built-in service definition: postgres, mysql, mariadb,
	// redis, mongo, or custom (which requires image and port).
	Type string `yaml:"type"`
	// Version is the image tag the type's image pattern is rendered with
	// (e.g. "16" for postgres:16-alpine). Ignored when Image is set.
	Version string `yaml:"version"`
	// Database/User/Password default to the app's own name, and to a
	// generated password stored on the target (see serviceState). Password
	// supports ${...}, so it can come from the watcher's environment.
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// Storage overrides where the service's data lives. Omitted, a stateful
	// type still gets a named volume at its default data path — losing a
	// database on redeploy is never the intent.
	Storage ServiceStorage `yaml:"storage"`

	// Overrides of the type's defaults.
	Image  string            `yaml:"image"`
	Port   int               `yaml:"port"`
	Env    map[string]string `yaml:"env"`
	Ports  []string          `yaml:"ports"` // publish to the host; empty keeps the service private to the network
	Health Health            `yaml:"health"`

	// Replicas and Mode are reserved for clustered services. v1 accepts only
	// the single-instance values and rejects anything else outright, rather
	// than quietly deploying one instance where several were asked for.
	Replicas int    `yaml:"replicas"`
	Mode     string `yaml:"mode"`
}

type ServiceStorage struct {
	// Volume is the named volume holding the data (default:
	// "<component>-<service>-data").
	Volume string `yaml:"volume"`
	// Path overrides the container path the volume is mounted at.
	Path string `yaml:"path"`
	// Size is recorded for the operator's benefit; nothing enforces it in v1.
	Size string `yaml:"size"`
}

type Build struct {
	Dockerfile string `yaml:"dockerfile"`
	Context    string `yaml:"context"`
	// Command is the M16 process-runtime build command (e.g.
	// ["go","build","-o","bin/app","."]), run in the clone dir when
	// requests.runtime is "process". Empty means no build step — Artifact is
	// taken directly from the freshly cloned repo as-is.
	Command []string `yaml:"command"`
	// Artifact is the repo-relative path to the build's output executable,
	// required when requests.runtime is "process". Must pass filepath.IsLocal
	// (same repo-root escape rule as Dockerfile/Context).
	Artifact string `yaml:"artifact"`
	// Template selects a built-in Dockerfile template (see templates.go),
	// letting a repo skip committing its own Dockerfile entirely. Mutually
	// exclusive with Dockerfile — Parse rejects a manifest that sets
	// both, since "use my Dockerfile" and "generate one for me" are
	// contradictory instructions.
	Template string `yaml:"template"`
	// TemplateVars overrides individual default values of the chosen
	// Template (see templateDefaults in templates.go); any variable not
	// present here falls back to the template's own default.
	TemplateVars map[string]string `yaml:"template_vars"`
}

type Run struct {
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
	// Ports entries are "hostPort:containerPort" (e.g. "8080:80") or a bare
	// container port (e.g. "80", no host publish).
	Ports []string `yaml:"ports"`
	// EnvFile (M23) lists dotenv files merged into the environment at deploy
	// time, in order (later files win). Entries take the same source forms as
	// a mount — repo-relative, absolute host path, or "volume:/path/inside" —
	// so an operator-managed secrets file can live outside the repo.
	EnvFile []string `yaml:"env_file"`
	// Files (M23) are single files or directories made visible inside the
	// container, read-only by default: the app's configuration. Sources are
	// usually repo-relative, so the config is versioned with the code.
	Files []Mount `yaml:"files"`
	// Volumes (M23) are read-write directories that survive redeploys: the
	// app's data. A bare name is a watcher-managed directory; an absolute
	// path is a host directory of the operator's choosing.
	Volumes []Mount `yaml:"volumes"`
}

// Health uses developer-friendly keys (the manifest is written by
// humans): probe type is inferred from which of http/exec is set, and
// durations are Go duration strings ("10s"), converted to the wire DTO's
// whole seconds.
type Health struct {
	HTTP     string   `yaml:"http"`     // http probe path, e.g. "/healthz"
	Port     int      `yaml:"port"`     // published host port the http probe hits
	Exec     []string `yaml:"exec"`     // exec probe command, e.g. ["sh","-c","..."]
	Interval string   `yaml:"interval"` // e.g. "10s"
	Timeout  string   `yaml:"timeout"`  // e.g. "5s"
	Failures int      `yaml:"failures"` // consecutive failures before restart
	Delay    string   `yaml:"delay"`    // initial grace period, e.g. "5s"
}

type Policy struct {
	Restart string `yaml:"restart"`
	// Replicas is reserved; v1 only supports the value 1 (or unset).
	Replicas int `yaml:"replicas"`
}

// Requests is the M22 rename of the former `resource:` block: a
// pre-deploy capacity check against the host qooqd reports (GET
// /v1/system/capacity, see checkRequests) — it does NOT constrain the
// running container. Container-level enforcement is Limits.
type Requests struct {
	CPU    float64  `yaml:"cpu"`    // cores, e.g. 0.5
	Memory string   `yaml:"memory"` // e.g. "256Mi"
	Disk   string   `yaml:"disk"`   // parsed but not validated in v1
	GPU    string   `yaml:"gpu"`    // parsed but not validated in v1
	Arch   []string `yaml:"arch"`   // e.g. ["amd64", "arm64"]
	// Runtime is "container" (default) or "process" (M16: build.command runs
	// on the host and the artifact deploys as a native process via qooqd
	// -runtime process). Validated against qooqd's actual runtime (GET
	// /v1/system/capacity) before any build work happens — see checkRequests.
	Runtime string `yaml:"runtime"`
}

// Limits (M22) is the container's hard cgroup cap: cpu (cores) and
// memory (e.g. "256Mi"), mapped 1:1 onto ComponentSpec.CPULimit/
// MemLimitBytes. Either field left unset (zero value) defaults to
// limitsMultiplier (Watcher.limitsMultiplier, default 2.0) times the
// corresponding requests field — see deriveContainerLimits. Both requests
// and limits absent means the container remains completely unbounded,
// matching pre-M22 behavior.
type Limits struct {
	CPU    float64 `yaml:"cpu"`    // cores, e.g. 1.0
	Memory string  `yaml:"memory"` // e.g. "512Mi"
}

// Parse parses the raw bytes of a qooq.yaml. It enforces version==1
// and the one hard structural rule (policy.replicas can only be unset or 1);
// everything else (build path safety, resource satisfiability, runtime
// support) is validated by the caller, which has more context (the clone
// dir, the host capacity) to produce a precise error.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid yaml: %w", err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("version: unsupported manifest version %d (only version 1 is supported)", m.Version)
	}
	if m.Policy.Replicas != 0 && m.Policy.Replicas != 1 {
		return nil, fmt.Errorf("policy.replicas: only 1 is supported (got %d)", m.Policy.Replicas)
	}
	// M21: requires: was renamed to resource: (same fields). LegacyRequires
	// only gets a non-zero Kind when the key is actually present in the
	// yaml; a bare `requires:` (no value) decodes as a null scalar and is
	// tolerated (nothing to migrate), but any actual content under it —
	// including an explicit empty map `requires: {}` — must be rejected so
	// a manifest written for the old key doesn't silently get ignored.
	if m.LegacyRequires.Kind != 0 {
		isNullScalar := m.LegacyRequires.Kind == yaml.ScalarNode && m.LegacyRequires.Tag == "!!null"
		if !isNullScalar {
			// Straight to today's name: resource: was itself renamed later, so
			// pointing there sent people through a second rejection.
			return nil, fmt.Errorf("requires: has been renamed — put capacity requirements (cpu, memory, arch, runtime) under requests: and (optionally) container caps under limits:")
		}
	}
	// M22: resource: was split into requests: (capacity check, same fields
	// resource: used to have) and limits: (container cgroup caps). Same
	// null-scalar tolerance as LegacyRequires above — a bare `resource:` with
	// no value has nothing to migrate.
	if m.LegacyResource.Kind != 0 {
		isNullScalar := m.LegacyResource.Kind == yaml.ScalarNode && m.LegacyResource.Tag == "!!null"
		if !isNullScalar {
			return nil, fmt.Errorf("resource: has been renamed — put capacity requirements under requests: and (optionally) container caps under limits:")
		}
	}
	if m.Build.Template != "" && m.Build.Dockerfile != "" {
		return nil, fmt.Errorf("build.template and build.dockerfile are mutually exclusive")
	}
	if m.Build.Template != "" {
		if err := CheckTemplate(m.Build.Template, m.Build.TemplateVars); err != nil {
			return nil, err
		}
	} else if len(m.Build.TemplateVars) > 0 {
		return nil, fmt.Errorf("build.template_vars is set but build.template is not: the variables would not be used")
	}
	if err := validateInputs(m.Inputs); err != nil {
		return nil, err
	}
	if err := ValidateHealthPath(m.Health.HTTP); err != nil {
		return nil, err
	}
	if err := validateHealthPort(m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Input declares one value the deploying operator must supply. Every
// field is optional: `PUBLIC_HOST: {}` is a complete declaration of an
// optional input.
type Input struct {
	// Description is shown to whoever has to supply the value. It is the
	// whole point of declaring an input rather than referencing ${env.X}, so
	// it is worth writing.
	Description string `yaml:"description"`
	// Default is used when no value was supplied. It may itself interpolate
	// (`default: ${host}`), which is how an app can offer a sensible local
	// answer and still be overridable.
	Default string `yaml:"default"`
	// Required blocks the deploy when there is no value and no default,
	// instead of letting the app start and fail in some less obvious way.
	Required bool `yaml:"required"`
	// Secret marks the value as sensitive: encrypted at rest with the same
	// key as the other credential fields, and masked by the API.
	Secret bool `yaml:"secret"`
	// Example is a sample value, shown alongside the description. A concrete
	// "192.168.1.50" answers "what does this want?" faster than prose.
	Example string `yaml:"example"`
}

// validateInputs rejects declarations that cannot mean anything useful.
func validateInputs(inputs map[string]Input) error {
	for name, in := range inputs {
		if name == "" {
			return fmt.Errorf("inputs: an input name cannot be empty")
		}
		if strings.ContainsAny(name, " \t.$={}") {
			return fmt.Errorf("inputs.%s: name may not contain spaces, dots or ${} characters (it is referenced as ${input.%s})", name, name)
		}
		// required + default is contradictory: the default means it can never
		// be missing, so the required flag could never fire. Rejecting it
		// beats silently honouring one and ignoring the other.
		if in.Required && in.Default != "" {
			return fmt.Errorf("inputs.%s: required and default are mutually exclusive (a default means it is never missing)", name)
		}
	}
	return nil
}

// Mount is one entry of run.files / run.volumes. Both a compose-style
// short string and an explicit mapping decode into it, because neither form is
// right for every case: the short string is what people reach for when the
// intent is obvious ("./config.toml:/app/config.toml"), the mapping is what
// stays readable when an entry needs a comment or a mode.
//
//	files:
//	  - "./config.toml:/app/config.toml"
//	  - from: config/app.toml
//	    to: /app/config.toml
//	    mode: ro
type Mount struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	Mode string `yaml:"mode"` // "ro" | "rw"; empty means "use the list's default"
}

// UnmarshalYAML accepts the scalar short form as well as the mapping form.
func (m *Mount) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		parsed, err := ParseMountEntry(s)
		if err != nil {
			return err
		}
		*m = parsed
		return nil
	}
	// Alias type to avoid recursing into this method.
	type rawMount Mount
	var raw rawMount
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*m = Mount(raw)
	return nil
}

// ParseMountEntry parses the short form "source:target[:mode]". Splitting on
// ":" is safe because a target is always an absolute container path and a
// source is a repo path, a host path or a volume name — none of which may
// contain a colon (enforced implicitly: an extra colon produces a 4-field
// split and an error rather than a silently wrong mount).
func ParseMountEntry(s string) (Mount, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	switch len(parts) {
	case 2:
		return Mount{From: parts[0], To: parts[1]}, nil
	case 3:
		mode := strings.ToLower(parts[2])
		if mode != "ro" && mode != "rw" {
			return Mount{}, fmt.Errorf("mount %q: mode must be \"ro\" or \"rw\" (got %q)", s, parts[2])
		}
		return Mount{From: parts[0], To: parts[1], Mode: mode}, nil
	default:
		return Mount{}, fmt.Errorf("mount %q: expected \"source:target\" or \"source:target:ro|rw\"", s)
	}
}

// Port is one entry of run.ports, already split into its parts.
type Port struct {
	Container int
	// Host is 0 when the port is not published to the host at all, which a
	// bare "80" means. AutoHost is the third case: publish it, but let the
	// daemon choose the number.
	Host     int
	AutoHost bool
}

// ParsePort reads the "host:container", "auto:container" and bare "container"
// forms. It lives here, with the rest of the format, so the watcher that
// deploys a manifest and the CLI that checks one cannot disagree about what a
// port string means.
func ParsePort(s string) (Port, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) == 2 {
		container, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return Port{}, fmt.Errorf("invalid port %q: container port: %w", s, err)
		}
		// "auto:8080" leaves the host side to the daemon, which is the only
		// party that knows what the rest of the machine already claims.
		if strings.EqualFold(strings.TrimSpace(parts[0]), "auto") {
			return Port{Container: container, AutoHost: true}, nil
		}
		host, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return Port{}, fmt.Errorf("invalid port %q: host port must be a number or \"auto\": %w", s, err)
		}
		return Port{Host: host, Container: container}, nil
	}
	container, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return Port{}, fmt.Errorf("invalid port %q: %w", s, err)
	}
	return Port{Container: container}, nil
}

// ValidateHealthPath rejects a health.http value that is not a path on the app.
// The probe URL is "http://127.0.0.1:<port>" + path, so "@evil.example/" would
// make the daemon request another host. Empty is fine (no http probe). Same
// rule as the daemon's own check (internal/core/health.go), stated here so the
// mistake shows up in `qooq manifest validate` instead of at deploy time.
func ValidateHealthPath(path string) error {
	if path == "" || (strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//")) {
		return nil
	}
	return fmt.Errorf("health.http: %q must start with a single \"/\" (a path on this app, not a host)", path)
}

// validateHealthPort checks that an http probe points at a port this app
// actually declares.
//
// A port belonging to nothing here is the worst mistake to let through: the
// probe falls back to 127.0.0.1:<that port>, which sooner or later belongs to
// a different component — so a broken app is checked against a healthy
// stranger, reports itself healthy, and is never restarted. Nothing about the
// symptom points at the cause.
//
// It bites hardest with "auto:" ports, where the number that used to be
// correct is no longer in run.ports at all and there is no new number to copy.
// Writing the container port is the spelling that survives: qooq resolves it to
// whichever host port the daemon assigned.
func validateHealthPort(m Manifest) error {
	if m.Health.HTTP == "" || m.Health.Port == 0 {
		return nil
	}
	var declared []string
	for _, spec := range m.Run.Ports {
		p, err := ParsePort(spec)
		if err != nil {
			// A malformed port is its own error, reported where it is parsed
			// for real; do not turn it into a confusing health-check message.
			return nil
		}
		declared = append(declared, spec)
		if p.Container == m.Health.Port || (p.Host != 0 && p.Host == m.Health.Port) {
			return nil
		}
	}
	if len(declared) == 0 {
		return fmt.Errorf("health.port: %d, but run.ports declares no ports for the probe to reach", m.Health.Port)
	}
	return fmt.Errorf("health.port: %d is not one of run.ports (%s) — with an auto: port, name the container port and qooq will probe whichever host port it assigns",
		m.Health.Port, strings.Join(declared, ", "))
}
