package api

import (
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/solo-039/internal/service"
	"net/http"
)

func (s *Server) incidents(w http.ResponseWriter, r *http.Request) {
	openOnly, err := boolQuery(r, "open")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	items := s.plant.Incidents.List(openOnly)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"total":      len(items),
		"open_count": s.plant.Incidents.CountOpen(),
	})
}

func (s *Server) incidentByID(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIncidentID(w, r)
	if !ok {
		return
	}
	inc, found := s.plant.Incidents.Get(id)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": service.ErrIncidentNotFound.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incident": inc, "revision_stale": s.plant.RevisionStale(inc)})
}

func (s *Server) acknowledgeIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIncidentID(w, r)
	if !ok {
		return
	}
	var input acknowledgeRequest
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	inc, err := s.plant.AcknowledgeIncident(id, input.Operator, input.Note)
	if err != nil {
		writeIncidentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incident": inc})
}

func (s *Server) reviewIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIncidentID(w, r)
	if !ok {
		return
	}
	var input reviewRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	inc, checks, err := s.plant.ReviewIncident(id, input.Reviewer)
	if err != nil {
		writeIncidentError(w, err)
		return
	}
	resolved := !inc.Open
	body := map[string]any{"incident": inc, "checks": checks, "resolved": resolved, "revision_stale": s.plant.RevisionStale(inc)}
	if !resolved {
		writeJSON(w, http.StatusUnprocessableEntity, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func parseIncidentID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid incident id"})
		return uuid.Nil, false
	}
	return id, true
}

func writeIncidentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrIncidentNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, service.ErrIncidentResolved), errors.Is(err, service.ErrIncidentNotAcknowledged):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}
