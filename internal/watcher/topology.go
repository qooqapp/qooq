package watcher

import (
	"encoding/json"

	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// encodeTopology renders what a deploy is connected to as the JSON stored on
// the Target. Nothing to record yields "", not "{}", so an unconnected app
// carries no topology at all.
func encodeTopology(services []watcherclient.TopologyService, refs []string) string {
	if len(services) == 0 && len(refs) == 0 {
		return ""
	}
	b, err := json.Marshal(watcherclient.Topology{Services: services, References: refs})
	if err != nil {
		return ""
	}
	return string(b)
}

// topologyForResponse decodes the stored topology; nil when there is none or
// it does not parse (a topology is a convenience and never worth failing a
// listing over).
func (t *Target) topologyForResponse() *watcherclient.Topology {
	if t.Topology == "" {
		return nil
	}
	var topo watcherclient.Topology
	if err := json.Unmarshal([]byte(t.Topology), &topo); err != nil {
		return nil
	}
	return &topo
}
