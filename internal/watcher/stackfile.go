package watcher

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// stackManifest is the composition layer (qooq.stack.yaml): it describes how
// several projects combine into one stack, and nothing about how any of them is
// built. How a project is made lives in that project's own qooq.yaml.
type stackManifest struct {
	Version int    `yaml:"version"`
	Stack   string `yaml:"stack"`
	// Description is metadata, like a manifest's: carried for the reader's
	// benefit, never acted on.
	Description string `yaml:"description"`
	// Projects is keyed by short name — the name the project's component takes
	// inside the stack, and the name its siblings address it by.
	//
	// It is a map, so the order entries appear in the file means nothing.
	// Deployment order comes from depends_on alone.
	Projects map[string]stackProject `yaml:"projects"`
	Policy   stackPolicy             `yaml:"policy"`
}

// stackProject is one member of a stack. Exactly one of Path or Repo says where
// it comes from.
type stackProject struct {
	// Path is a directory inside the stack's own repository — the monorepo case.
	// Its qooq.yaml is read from there.
	Path string `yaml:"path"`
	// Repo is an external git repository, cloned and built like any target.
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	// Manifest is the project's manifest path within its source (default
	// "qooq.yaml").
	Manifest string `yaml:"manifest"`
	// DependsOn lists the projects that must come first. See projectDep.
	DependsOn []projectDep `yaml:"depends_on"`
	// Env is injected into this project, overriding its own run.env — the
	// composition layer wins over the app layer, as deploy_spec already does.
	Env map[string]string `yaml:"env"`
}

// projectSource is which of the two forms a project declared.
type projectSource int

const (
	projectSourcePath projectSource = iota
	projectSourceRepo
)

func (s stackProject) source() projectSource {
	if s.Repo != "" {
		return projectSourceRepo
	}
	return projectSourcePath
}

// waitMode is how far a dependency must get before its dependents may deploy.
type waitMode string

const (
	// waitReady means Running and passing its health check — what people
	// usually mean by "depends_on", and what services: already does.
	waitReady waitMode = "ready"
	// waitStarted means only that the deploy finished. The escape hatch for a
	// slow starter that would otherwise stall everything downstream, or a project
	// with no health probe to wait on.
	waitStarted waitMode = "started"
)

// projectDep is one entry of a project's depends_on: list. It accepts a bare
// project name
// (meaning "ready") or an explicit mapping, the same string-or-object shape
// run.files uses — the short form for the common case, the long form when it
// needs to say more.
//
//	depends_on:
//	  - shell-shrimp
//	  - name: stock
//	    wait: started
type projectDep struct {
	Name string   `yaml:"name"`
	Wait waitMode `yaml:"wait"`
}

func (d *projectDep) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var name string
		if err := node.Decode(&name); err != nil {
			return err
		}
		*d = projectDep{Name: name, Wait: waitReady}
		return nil
	}
	type rawDep projectDep
	var raw rawDep
	if err := node.Decode(&raw); err != nil {
		return err
	}
	if raw.Wait == "" {
		raw.Wait = waitReady
	}
	*d = projectDep(raw)
	return nil
}

type stackPolicy struct {
	// MaxFailures is how many consecutive failures of any one project pause the
	// whole stack. Zero means "use the default"; see defaultMaxFailures. A
	// negative value disables pausing entirely (pure converging retry).
	MaxFailures int `yaml:"max_failures"`
}

// defaultMaxFailures is generous on purpose: it has to absorb registry
// hiccups and DNS blips without pausing, while still catching a project that is
// genuinely broken rather than leaving the stack degraded indefinitely.
const defaultMaxFailures = 10

// maxFailures resolves the policy's effective value. Negative disables.
func (p stackPolicy) maxFailures() int {
	switch {
	case p.MaxFailures < 0:
		return 0
	case p.MaxFailures == 0:
		return defaultMaxFailures
	default:
		return p.MaxFailures
	}
}

// stackNameRe and projectNameRe are the DNS-label rule qooqd enforces on
// namespaced components (see internal/core's validateNaming), applied here so
// a bad name is reported against the manifest line that caused it rather than
// as a deploy failure later.
var (
	stackNameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	projectNameRe = stackNameRe
)

// parseStackManifest parses and fully validates a qooq.stack.yaml. Everything that
// can be checked without touching the network is checked here: names, source
// forms, dangling dependencies and cycles. A stack manifest that parses is one the
// deployer can walk without re-validating.
func parseStackManifest(data []byte) (*stackManifest, error) {
	var r stackManifest
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("invalid yaml: %w", err)
	}
	if r.Version != 1 {
		return nil, fmt.Errorf("version: unsupported stack manifest version %d (only version 1 is supported)", r.Version)
	}
	if r.Stack == "" {
		return nil, fmt.Errorf("stack: required (it names the namespace and network this stack runs in)")
	}
	if !stackNameRe.MatchString(r.Stack) {
		return nil, fmt.Errorf("stack %q: must be a lowercase DNS label (a-z, 0-9, -), 1-63 chars", r.Stack)
	}
	if len(r.Projects) == 0 {
		return nil, fmt.Errorf("projects: a stack needs at least one project")
	}

	for _, name := range sortedProjectNames(r.Projects) {
		project := r.Projects[name]
		if !projectNameRe.MatchString(name) {
			return nil, fmt.Errorf("projects.%s: name must be a lowercase DNS label (a-z, 0-9, -), 1-63 chars", name)
		}
		if err := validateProjectSource(name, project); err != nil {
			return nil, err
		}
		for _, dep := range project.DependsOn {
			if dep.Name == "" {
				return nil, fmt.Errorf("projects.%s.depends_on: an entry has no name", name)
			}
			if dep.Name == name {
				return nil, fmt.Errorf("projects.%s.depends_on: a project cannot wait for itself", name)
			}
			if _, ok := r.Projects[dep.Name]; !ok {
				return nil, fmt.Errorf("projects.%s.depends_on: %q is not a project in this stack", name, dep.Name)
			}
			if dep.Wait != waitReady && dep.Wait != waitStarted {
				return nil, fmt.Errorf("projects.%s.after.%s: wait must be %q or %q (got %q)",
					name, dep.Name, waitReady, waitStarted, dep.Wait)
			}
		}
	}

	// Ordering is computed at parse time so a cycle is a parse error rather
	// than something the deployer discovers halfway through a deploy.
	if _, err := r.deployOrder(); err != nil {
		return nil, err
	}
	return &r, nil
}

// validateProjectSource enforces that a project names exactly one source, and that
// the fields it carries make sense for that source.
func validateProjectSource(name string, project stackProject) error {
	switch {
	case project.Path == "" && project.Repo == "":
		return fmt.Errorf("projects.%s: needs either path (a directory in this repo) or repo (an external repository)", name)
	case project.Path != "" && project.Repo != "":
		return fmt.Errorf("projects.%s: path and repo are mutually exclusive", name)
	}

	if project.source() == projectSourcePath && !validTargetPath(strings.TrimPrefix(project.Path, "./")) {
		return fmt.Errorf("projects.%s: path %q must stay inside the stack's own repo", name, project.Path)
	}
	return nil
}

// deployOrder returns the projects in an order that respects every depends_on:
// dependency, and reports a cycle as an error naming the projects involved.
//
// Kahn's algorithm over a deterministic (sorted) frontier, so the same stack manifest
// always produces the same order — a deploy log that reorders itself between
// runs is much harder to read than one that does not.
func (r *stackManifest) deployOrder() ([]string, error) {
	remaining := make(map[string]int, len(r.Projects))
	dependents := make(map[string][]string, len(r.Projects))

	for _, name := range sortedProjectNames(r.Projects) {
		remaining[name] = len(r.Projects[name].DependsOn)
		for _, dep := range r.Projects[name].DependsOn {
			dependents[dep.Name] = append(dependents[dep.Name], name)
		}
	}

	var ready []string
	for _, name := range sortedProjectNames(r.Projects) {
		if remaining[name] == 0 {
			ready = append(ready, name)
		}
	}

	order := make([]string, 0, len(r.Projects))
	for len(ready) > 0 {
		sort.Strings(ready)
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)

		for _, dependent := range dependents[name] {
			remaining[dependent]--
			if remaining[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}

	if len(order) != len(r.Projects) {
		var stuck []string
		for name, n := range remaining {
			if n > 0 {
				stuck = append(stuck, name)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("projects: depends_on: forms a cycle among %s", strings.Join(stuck, ", "))
	}
	return order, nil
}

// sortedProjectNames gives a stable iteration order over the projects map.
func sortedProjectNames(projects map[string]stackProject) []string {
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
