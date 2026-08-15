package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// selectableSpy embeds the interface so only the probed method needs a body.
type selectableSpy struct {
	service.ConnectionService
	project        string
	connectionType string
}

func (s *selectableSpy) ListSelectable(_ context.Context, project, connectionType string) ([]models.SelectableConnection, error) {
	s.project = project
	s.connectionType = connectionType
	return []models.SelectableConnection{}, nil
}

// Pins the wire names the console depends on. Every other test calls the service
// directly, so nothing else would notice the query parameter being renamed.
func TestListSelectableReadsTheConnectionTypeQuery(t *testing.T) {
	spy := &selectableSpy{}
	engine := gin.New()
	engine.GET("/api/projects/:name/connections/selectable", NewConnectionHandler(spy).ListSelectable)

	recorder := call(engine, http.MethodGet, "/api/projects/demo/connections/selectable?connectionType=database-server")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if spy.project != "demo" {
		t.Errorf("project = %q, want demo", spy.project)
	}
	if spy.connectionType != "database-server" {
		t.Errorf("connectionType = %q, want database-server: the console builds ?connectionType=", spy.connectionType)
	}
}

// An input with no declared connection type must still reach the service, which
// answers with everything selectable rather than nothing.
func TestListSelectableToleratesAMissingConnectionType(t *testing.T) {
	spy := &selectableSpy{connectionType: "untouched"}
	engine := gin.New()
	engine.GET("/api/projects/:name/connections/selectable", NewConnectionHandler(spy).ListSelectable)

	recorder := call(engine, http.MethodGet, "/api/projects/demo/connections/selectable")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if spy.connectionType != "" {
		t.Errorf("connectionType = %q, want empty", spy.connectionType)
	}
}
