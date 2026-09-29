package api

import (
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"net/http"
)

func (s *Server) incident(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid incident id"})
		return
	}
	item, ok := s.plant.Incidents.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "incident not found"})
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// resolveIncident 触发结案前复核：复核通过才结案，否则保持未结并回传失败的检查项。
func (s *Server) resolveIncident(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid incident id"})
		return
	}
	item, err := s.plant.ResolveIncident(id)
	if err != nil {
		if err.Error() == "incident not found" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "incident": item})
		return
	}
	writeJSON(w, http.StatusOK, item)
}
