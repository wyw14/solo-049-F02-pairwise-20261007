package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	passsvc "orbitlink/internal/pass"
)

type previewView struct {
	passsvc.RevisionPreview
	Summary passsvc.PreviewSummary `json:"summary"`
}

func (r *Runtime) listReplanPreviews(writer http.ResponseWriter, _ *http.Request) {
	previews := r.Replanner.Previews()
	views := make([]previewView, 0, len(previews))
	for _, preview := range previews {
		views = append(views, previewView{RevisionPreview: preview, Summary: preview.Summary()})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": views})
}

func (r *Runtime) listReplanReviews(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": r.Replanner.PendingReviews()})
}

func (r *Runtime) approveReplanReview(writer http.ResponseWriter, request *http.Request) {
	item, err := r.Replanner.Approve(chi.URLParam(request, "itemID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}

func (r *Runtime) rejectReplanReview(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, err)
		return
	}
	item, err := r.Replanner.Reject(chi.URLParam(request, "itemID"), input.Reason)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, item)
}
