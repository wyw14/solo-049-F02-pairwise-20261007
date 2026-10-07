package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (r *Runtime) listReplanPreviews(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": r.ReplanPreviews.List()})
}

func (r *Runtime) listReplanImpacts(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": r.ReplanPreviews.Impacts()})
}

func (r *Runtime) approveReplanReview(writer http.ResponseWriter, request *http.Request) {
	impact, err := r.Replanner.Approve(chi.URLParam(request, "impactID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, impact)
}

func (r *Runtime) rejectReplanReview(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, err)
		return
	}
	impact, err := r.Replanner.Reject(chi.URLParam(request, "impactID"), input.Reason)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, impact)
}
