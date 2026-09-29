package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyw14/solo-039/internal/service"
)

func newTestServer(t *testing.T) (*Server, *service.Plant) {
	t.Helper()
	plant := service.NewPlant(t.TempDir())
	return NewServerWithPlant(plant), plant
}

func doRequest(t *testing.T, server *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func openIncidentID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(list.Items))
	}
	return list.Items[0].ID
}

func TestIncidentAPIStatusCodes(t *testing.T) {
	server, plant := newTestServer(t)
	if _, err := plant.Start(context.Background(), "baseline", "primary"); err != nil {
		t.Fatal(err)
	}
	firstLease, err := plant.AcquireReflux("T-101", 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plant.AcquireReflux("blocked", 1.0); err == nil {
		t.Fatal("expected reflux capacity failure")
	}

	// List carries the correlated structure.
	list := doRequest(t, server, http.MethodGet, "/api/incidents", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	id := openIncidentID(t, list)

	// Invalid id shape -> 400.
	if rec := doRequest(t, server, http.MethodPost, "/api/incidents/not-a-uuid/acknowledge", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d", rec.Code)
	}
	// Missing incident -> 404.
	if rec := doRequest(t, server, http.MethodGet, "/api/incidents/00000000-0000-0000-0000-000000000000", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing incident status = %d", rec.Code)
	}
	// Review before acknowledgement -> 409.
	if rec := doRequest(t, server, http.MethodPost, "/api/incidents/"+id+"/review", ""); rec.Code != http.StatusConflict {
		t.Fatalf("review before acknowledgement status = %d", rec.Code)
	}
	// Acknowledge without operator -> 400.
	if rec := doRequest(t, server, http.MethodPost, "/api/incidents/"+id+"/acknowledge", `{"operator":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("acknowledge without operator status = %d", rec.Code)
	}
	// Acknowledge -> 200.
	if rec := doRequest(t, server, http.MethodPost, "/api/incidents/"+id+"/acknowledge", `{"operator":"ops","note":"fixing"}`); rec.Code != http.StatusOK {
		t.Fatalf("acknowledge status = %d body = %s", rec.Code, rec.Body.String())
	}
	// Review with capacity still exhausted -> 422, incident still open.
	rec := doRequest(t, server, http.MethodPost, "/api/incidents/"+id+"/review", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("failed review status = %d body = %s", rec.Code, rec.Body.String())
	}
	var failed struct {
		Resolved bool `json:"resolved"`
		Checks   []struct {
			Passed bool `json:"passed"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Resolved || len(failed.Checks) != 1 || failed.Checks[0].Passed {
		t.Fatalf("failed review must report resolved=false with a failing check: %s", rec.Body.String())
	}
	if rec := doRequest(t, server, http.MethodGet, "/api/incidents?open=true", ""); rec.Code != http.StatusOK {
		t.Fatalf("open list status = %d", rec.Code)
	}

	// Restore capacity, then review again: 200 and resolved.
	plant.Reflux.Release(firstLease)
	rec = doRequest(t, server, http.MethodPost, "/api/incidents/"+id+"/review", `{"reviewer":"ops"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("successful review status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resolved struct {
		Resolved bool `json:"resolved"`
		Incident struct {
			Status string `json:"status"`
		} `json:"incident"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if !resolved.Resolved || resolved.Incident.Status != "resolved" {
		t.Fatalf("review response must report the resolved incident: %s", rec.Body.String())
	}
	// Reviewing again is a conflict.
	if rec := doRequest(t, server, http.MethodPost, "/api/incidents/"+id+"/review", ""); rec.Code != http.StatusConflict {
		t.Fatalf("review after resolution status = %d", rec.Code)
	}
}
