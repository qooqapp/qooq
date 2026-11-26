package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/qooqapp/qooq/internal/runtime"
)

// LoadImage imports a `docker save` tarball (docker load). The Docker daemon
// streams a JSON progress log while unpacking; only fully draining it tells
// us the load actually finished, and any {"error":...} entry in the stream
// means the load failed even though the HTTP call itself succeeded.
func (a *Adapter) LoadImage(ctx context.Context, tarball io.Reader) ([]runtime.ImageID, error) {
	resp, err := a.cli.ImageLoad(ctx, tarball)
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}
	defer resp.Body.Close()

	refs, err := drainLoadStream(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}
	if len(refs) == 0 {
		return nil, errors.New("load image: no images reported as loaded")
	}

	ids := make([]runtime.ImageID, 0, len(refs))
	for _, ref := range refs {
		insp, err := a.cli.ImageInspect(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("inspect %s after load: %w", ref, err)
		}
		ids = append(ids, runtime.ImageID(insp.ID))
	}
	return ids, nil
}

// drainLoadStream consumes the JSON progress stream from ImageLoad and
// returns the image refs/ids the daemon reported as loaded, in order.
func drainLoadStream(r io.Reader) ([]string, error) {
	var loaded []string
	dec := json.NewDecoder(r)
	for {
		var msg progressMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if msgErr := msg.err(); msgErr != "" {
			return nil, errors.New(msgErr)
		}
		line := strings.TrimSpace(msg.Stream)
		switch {
		case strings.HasPrefix(line, "Loaded image ID: "):
			loaded = append(loaded, strings.TrimPrefix(line, "Loaded image ID: "))
		case strings.HasPrefix(line, "Loaded image: "):
			loaded = append(loaded, strings.TrimPrefix(line, "Loaded image: "))
		}
	}
	return loaded, nil
}

// progressMessage is one line of the JSON progress stream shared by
// ImageLoad and ImageBuild.
type progressMessage struct {
	Stream      string `json:"stream"`
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
	Aux *struct {
		ID string `json:"ID"`
	} `json:"aux"`
}

// err returns the failure message carried by this line, or "" if it is not
// an error line.
func (m progressMessage) err() string {
	if m.ErrorDetail != nil && m.ErrorDetail.Message != "" {
		return m.ErrorDetail.Message
	}
	return m.Error
}
