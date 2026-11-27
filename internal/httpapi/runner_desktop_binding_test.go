package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestDesktopProbeBindingRestoreAndRevoke(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SESSION_SECRET", "desktop-binding-test-secret")
	h, store := newRunnerTestServerStore(t)
	owner := domain.User{ID: 42, LoginName: "alice"}
	stranger := domain.User{ID: 43, LoginName: "bob"}
	if err := store.PutUser(stranger); err != nil {
		t.Fatal(err)
	}
	runtimes := []domain.RunnerRuntime{{Provider: domain.RunnerProviderDesktop, Status: domain.RuntimeStatusOnline, Capabilities: []string{domain.RunnerCapabilityDesktopProbe}}}
	type registration struct {
		ID    string `json:"runnerId"`
		Token string `json:"sessionToken"`
	}
	pair := func() registration {
		t.Helper()
		code := requestJSONAuthed[struct {
			Code string `json:"code"`
		}](t, h, owner, http.MethodPost, "/api/runners/bind-codes", nil, http.StatusOK)
		rec := requestWithHeaders(t, h, http.MethodPost, "/runners/register", map[string]any{"runtimes": runtimes}, map[string]string{"X-Bootstrap-Token": code.Code})
		var device registration
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &device) != nil || device.ID == "" || device.Token == "" {
			t.Fatalf("pairing failed: HTTP %d", rec.Code)
		}
		return device
	}
	post := func(device registration, path string, body any, want int) {
		t.Helper()
		rec := requestWithHeaders(t, h, http.MethodPost, path, body, map[string]string{"Authorization": "Bearer " + device.Token})
		if rec.Code != want {
			t.Fatalf("%s: got %d, want %d: %s", path, rec.Code, want, rec.Body)
		}
	}
	device := pair()
	heartbeat := map[string]any{"runtimes": runtimes}
	post(device, "/runners/disconnect", nil, http.StatusOK)
	for i := 0; i < 2; i++ {
		post(device, "/runners/heartbeat", heartbeat, http.StatusOK)
	}
	mine := requestJSONAuthed[[]domain.Runner](t, h, owner, http.MethodGet, "/api/runners?mine=1", nil, http.StatusOK)
	if len(mine) != 1 || mine[0].ID != device.ID || mine[0].Status != domain.RunnerStatusOnline {
		t.Fatalf("restoration changed machine identity: %+v", mine)
	}
	if rec := requestWithHeaders(t, h, http.MethodDelete, "/api/runners/"+device.ID, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous owner revoke = %d", rec.Code)
	}
	requestJSONAuthed[map[string]string](t, h, stranger, http.MethodDelete, "/api/runners/"+device.ID, nil, http.StatusForbidden)
	second := pair()
	post(device, "/api/runners/deregister", map[string]string{"runnerId": second.ID}, http.StatusBadRequest)
	post(device, "/api/runners/deregister?runnerId="+second.ID, nil, http.StatusBadRequest)
	post(device, "/api/runners/deregister", map[string]any{}, http.StatusOK)
	post(device, "/runners/deregister", nil, http.StatusUnauthorized)
	post(device, "/runners/heartbeat", heartbeat, http.StatusUnauthorized)
	post(second, "/runners/heartbeat", heartbeat, http.StatusOK)
	for i := 0; i < 2; i++ {
		requestJSONAuthed[map[string]bool](t, h, owner, http.MethodDelete, "/api/runners/"+second.ID, nil, http.StatusOK)
	}
	post(second, "/runners/heartbeat", heartbeat, http.StatusUnauthorized)
	expired := pair()
	machine, ok := store.Runner(expired.ID)
	if !ok {
		t.Fatal("missing machine")
	}
	past := time.Now().UTC().Add(-time.Hour)
	machine.SessionExpiresAt = &past
	if err := store.PutRunner(machine); err != nil {
		t.Fatal(err)
	}
	post(expired, "/runners/heartbeat", heartbeat, http.StatusUnauthorized)
	post(expired, "/runners/deregister", nil, http.StatusUnauthorized)
}
