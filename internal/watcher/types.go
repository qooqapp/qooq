package watcher

import (
	"encoding/json"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// CreateTargetRequest, TargetResponse and errorResponse are aliases of the
// wire DTOs in pkg/watcherclient — that package is the single source of
// truth for the JSON contract, shared with cmd/qooq's `qooq watch` CLI
// (which cannot import this package: gin/fx/gorm/go-git must never reach
// cmd/qooq, see the dependency wall in docs/ARCHITECTURE.md).
type CreateTargetRequest = watcherclient.CreateTargetRequest
type TargetResponse = watcherclient.TargetResponse
type errorResponse = watcherclient.ErrorResponse
type DeployTargetRequest = watcherclient.DeployTargetRequest
type CreateStackRequest = watcherclient.CreateStackRequest
type StackResponse = watcherclient.StackResponse
type StackProjectResponse = watcherclient.StackProjectResponse
type InputResponse = watcherclient.InputResponse
type SetInputsRequest = watcherclient.SetInputsRequest
type UpdateTargetRequest = watcherclient.UpdateTargetRequest
type UploadSourceResponse = watcherclient.UploadSourceResponse

// deploySpecFields mirrors client.DeployRequest minus Name/Image, which the
// pipeline fills in itself. Stored/parsed as JSON in Target.DeploySpec.
type deploySpecFields struct {
	Namespace     string            `json:"namespace,omitempty"`
	Command       []string          `json:"command,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Ports         []portSpec        `json:"ports,omitempty"`
	Mounts        []mountSpec       `json:"mounts,omitempty"`
	Network       string            `json:"network,omitempty"`
	GroupID       string            `json:"group_id,omitempty"`
	RestartPolicy string            `json:"restart_policy,omitempty"`
	CPULimit      float64           `json:"cpu_limit,omitempty"`
	MemLimitBytes int64             `json:"mem_limit_bytes,omitempty"`
	Domain        string            `json:"domain,omitempty"`
	Ingress       *ingressSpec      `json:"ingress,omitempty"`
	HealthCheck   *healthCheckSpec  `json:"health_check,omitempty"`
}

// ingressSpec mirrors client.IngressSpec: how the reverse proxy should expose
// this component.
type ingressSpec struct {
	Hosts     []string `json:"hosts,omitempty"`
	Port      int      `json:"port,omitempty"`
	Path      string   `json:"path,omitempty"`
	StripPath bool     `json:"strip_path,omitempty"`
	TLS       *bool    `json:"tls,omitempty"`
}

type portSpec struct {
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	AutoHost      bool   `json:"auto_host,omitempty"`
}

// UnmarshalJSON also accepts the "host:container" (or bare "container") string
// a manifest's run.ports and the CLI's --port both use. A deploy spec that
// spelled ports the way everything else spells them used to be rejected as
// malformed JSON, which reads as "my JSON is wrong" rather than "this one
// field wants a different shape".
func (p *portSpec) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, err := parseManifestPort(s)
		if err != nil {
			return err
		}
		*p = parsed
		return nil
	}
	// A distinct type, or json.Unmarshal would recurse into this method.
	type portSpecFields portSpec
	var fields portSpecFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*p = portSpec(fields)
	return nil
}

// mountSpec mirrors client.MountSpec. Both paths are absolute host/container
// paths: by the time a mount reaches this struct the manifest's declarative
// forms (repo-relative, named volume) have been resolved by materializeMounts.
type mountSpec struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"`
	ReadOnly      bool   `json:"read_only,omitempty"`
}

type healthCheckSpec struct {
	Type                string   `json:"type"`
	HTTPPath            string   `json:"http_path,omitempty"`
	HTTPPort            int      `json:"http_port,omitempty"`
	ExecCommand         []string `json:"exec_command,omitempty"`
	IntervalSeconds     int      `json:"interval_seconds,omitempty"`
	TimeoutSeconds      int      `json:"timeout_seconds,omitempty"`
	FailureThreshold    int      `json:"failure_threshold,omitempty"`
	InitialDelaySeconds int      `json:"initial_delay_seconds,omitempty"`
}
