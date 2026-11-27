package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/pkg/client"
)

// reserve sets a component's address aside before it is deployed (see
// core.Reservation). The answer has the shape of a component, with phase
// "Reserved", so a caller resolving another app's address reads it the same
// way whether that app is running yet or not. A name that is already deployed
// is answered with the real component.
func (h *handlers) reserve(w http.ResponseWriter, r *http.Request) {
	var req client.DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	comp, reserved, err := h.svc.Reserve(r.Context(), specFromRequest(req))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	if !reserved {
		writeJSON(w, http.StatusOK, h.toResponse(comp))
		return
	}
	writeJSON(w, http.StatusOK, h.reservedResponse(comp, nil))
}

func (h *handlers) listReservations(w http.ResponseWriter, r *http.Request) {
	rs, err := h.svc.Reservations(r.Context())
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	comps, err := h.svc.List(r.Context())
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	out := make([]client.ComponentResponse, 0, len(rs))
	for _, res := range rs {
		out = append(out, h.reservedResponse(&core.Component{Spec: res.Spec}, comps))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) releaseReservation(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	if err := h.svc.Release(r.Context(), r.URL.Query().Get("namespace"), name); err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reservedResponse reports a reservation as the component it will become.
// The placeholder id only keeps it distinct from real components while its
// ingress hostnames are assigned; it is not reported.
func (h *handlers) reservedResponse(c *core.Component, all []*core.Component) client.ComponentResponse {
	c.ID = "reserved:" + c.Spec.Namespace + "/" + c.Spec.Name
	resp := h.toResponseAmong(c, all)
	resp.ID = ""
	resp.Phase = client.PhaseReserved
	return resp
}
