package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"agentflow/internal/domain"
)

func TestDesktopProbeManagementAndClaim(t *testing.T) {
	h := newRunnerTestServer(t)
	runnerID, session := registerTestRunner(t, h)
	_, otherSession := registerTestRunner(t, h)
	payload := map[string]any{"runnerId": runnerID, "provider": "pinable-desktop", "action": "desktop_probe_v1"}
	const collection = "/api/runners/tasks"
	check := func(method, path string, body any, session string, status int) []byte {
		t.Helper()
		var auth map[string]string
		if session != "" {
			auth = map[string]string{"Authorization": "Bearer " + session}
		}
		rec := requestWithHeaders(t, h, method, path, body, auth)
		if rec.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, rec.Code, status, rec.Body)
		}
		return rec.Body.Bytes()
	}
	report := func(session string, capabilities []string) {
		t.Helper()
		check(http.MethodPost, "/runners/heartbeat", map[string]any{
			"runtimes": []domain.RunnerRuntime{{Provider: domain.RunnerProviderDesktop, Status: domain.RuntimeStatusOnline, Capabilities: capabilities}},
		}, session, http.StatusOK)
	}
	claim := func(session, provider string, preflight bool, code int) []byte {
		t.Helper()
		return check(http.MethodPost, "/runners/tasks/claim", map[string]any{
			"availableRuntimes": []string{provider}, "preflight": preflight, "noWait": true,
		}, session, code)
	}
	check(http.MethodPost, collection, payload, "", http.StatusConflict)
	caps := []string{domain.RunnerCapabilityDesktopProbe}
	report(session, caps)
	report(otherSession, caps)
	payload["command"] = "id"
	check(http.MethodPost, collection, payload, "", http.StatusBadRequest)
	delete(payload, "command")
	payload["action"] = ""
	check(http.MethodPost, collection, payload, "", http.StatusBadRequest)
	payload["action"] = domain.RunnerTaskActionDesktopProbe
	payload["runnerId"] = "absent-runner"
	check(http.MethodPost, collection, payload, "", http.StatusNotFound)
	payload["runnerId"] = runnerID
	created := requestJSON[domain.RunnerTask](t, h, http.MethodPost, collection, payload, http.StatusCreated)
	if !created.IsDesktopProbe() || created.MaxAttempts != 1 || created.RunnerID != runnerID || created.ID == "" {
		t.Fatalf("invalid persisted probe: %+v", created)
	}
	for _, preflight := range []bool{true, false} {
		claim(otherSession, domain.RunnerProviderDesktop, preflight, http.StatusNoContent)
		claim(session, "mock", preflight, http.StatusNoContent)
	}
	report(session, nil)
	for _, preflight := range []bool{true, false} {
		claim(session, " PINABLE-DESKTOP ", preflight, http.StatusNoContent)
	}
	report(session, caps)
	claim(session, domain.RunnerProviderDesktop, true, http.StatusOK)
	path := collection + "/" + created.ID
	status := requestJSON[map[string]string](t, h, http.MethodGet, path+"/status", nil, http.StatusOK)
	if len(status) != 2 || status["id"] != created.ID || status["status"] != domain.RunnerTaskQueued {
		t.Fatalf("unexpected management status: %#v", status)
	}
	raw := claim(session, domain.RunnerProviderDesktop, false, http.StatusOK)
	var leased struct {
		Task domain.RunnerTask `json:"task"`
	}
	if err := json.Unmarshal(raw, &leased); err != nil || leased.Task.Action != domain.RunnerTaskActionDesktopProbe || leased.Task.LeaseOwner != runnerID || leased.Task.ID != created.ID {
		t.Fatalf("invalid claim %s: %v", raw, err)
	}
	check(http.MethodPost, path+"/start", map[string]any{}, session, http.StatusOK)
	check(http.MethodPost, path+"/cancel", nil, "", http.StatusOK)
	raw = check(http.MethodPost, path+"/complete", map[string]any{"status": "success", "result": "late"}, session, http.StatusOK)
	var receipt struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Status != domain.RunnerTaskCanceled {
		t.Fatalf("cancel did not win: %s, %v", raw, err)
	}
	saved := requestJSON[map[string]string](t, h, http.MethodGet, path+"/status", nil, http.StatusOK)
	if saved["status"] != domain.RunnerTaskCanceled {
		t.Fatalf("management view after canceled completion: %v", saved)
	}
	normal := requestJSON[domain.RunnerTask](t, h, http.MethodPost, collection, map[string]string{"provider": "mock", "prompt": "old"}, http.StatusCreated)
	check(http.MethodGet, collection+"/"+normal.ID+"/status", nil, "", http.StatusNotFound)
}

func TestDesktopProbeRegistrationKeepsDeviceIdentity(t *testing.T) {
	h := newRunnerTestServer(t)
	legacyID, _ := registerTestRunner(t, h)
	bootstrap := map[string]string{"X-Bootstrap-Token": "boot-secret"}
	desktop := []domain.RunnerRuntime{{Provider: domain.RunnerProviderDesktop, Status: domain.RuntimeStatusOnline, Capabilities: []string{domain.RunnerCapabilityDesktopProbe}}}
	register := func(id string, runtimes []domain.RunnerRuntime, want int) (string, string) {
		t.Helper()
		rec := requestWithHeaders(t, h, http.MethodPost, "/api/runners/register", map[string]any{"runnerId": id, "name": "pairing", "runtimes": runtimes}, bootstrap)
		if rec.Code != want {
			t.Fatalf("register %s: got %d, want %d: %s", id, rec.Code, want, rec.Body)
		}
		var out struct {
			RunnerID     string `json:"runnerId"`
			SessionToken string `json:"sessionToken"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.RunnerID, out.SessionToken
	}
	register(legacyID, desktop, http.StatusConflict)
	if rotated, _ := register(legacyID, nil, http.StatusOK); rotated != legacyID {
		t.Fatalf("legacy CLI registration changed id: %s", rotated)
	}
	deviceID, session := register("", desktop, http.StatusOK)
	if deviceID == "" || session == "" {
		t.Fatal("missing desktop identity")
	}
	register(deviceID, desktop, http.StatusConflict)
	register(deviceID, nil, http.StatusConflict)
	auth := map[string]string{"Authorization": "Bearer " + session}
	rec := requestWithHeaders(t, h, http.MethodPost, "/runners/heartbeat", map[string]any{"runtimes": []domain.RunnerRuntime{}}, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("original session lost after rotation attempt: %d %s", rec.Code, rec.Body)
	}
	register(deviceID, nil, http.StatusConflict)
	// A scoped session cannot widen its inventory, request a public CLI task,
	// read another device's probe, or reach management and relay endpoints.
	publicTask := requestJSON[domain.RunnerTask](t, h, http.MethodPost, "/api/runners/tasks", map[string]string{"provider": "mock", "prompt": "private CLI prompt"}, http.StatusCreated)
	otherDevice, _ := register("", desktop, http.StatusOK)
	otherProbe := requestJSON[domain.RunnerTask](t, h, http.MethodPost, "/api/runners/tasks", map[string]string{"provider": "pinable-desktop", "action": "desktop_probe_v1", "runnerId": otherDevice}, http.StatusCreated)
	for _, tc := range []struct {
		method, path string
		body         any
		code         int
	}{
		{http.MethodPost, "/runners/tasks/claim", map[string]any{"availableRuntimes": []string{"mock"}, "noWait": true}, http.StatusForbidden},
		{http.MethodPost, "/runners/tasks/claim", map[string]any{"availableRuntimes": []string{"mock", "pinable-desktop"}, "preflight": true, "noWait": true}, http.StatusForbidden},
		{http.MethodGet, "/api/runners/tasks/" + publicTask.ID, nil, http.StatusNotFound},
		{http.MethodGet, "/api/runners/tasks/" + publicTask.ID + "/events", nil, http.StatusNotFound},
		{http.MethodGet, "/api/runners/tasks/" + otherProbe.ID, nil, http.StatusNotFound},
		{http.MethodPost, "/runners/tasks/" + otherProbe.ID + "/events", nil, http.StatusNotFound},
		{http.MethodPost, "/runners/heartbeat", map[string]any{"runtimes": []domain.RunnerRuntime{{Provider: "mock"}}}, http.StatusForbidden},
		{http.MethodGet, "/api/runners/tasks", nil, http.StatusForbidden},
		{http.MethodGet, "/api/runners/tasks/" + otherProbe.ID + "/status", nil, http.StatusForbidden},
		{http.MethodPost, "/runners/deregister", nil, http.StatusForbidden},
		{http.MethodGet, "/runners/connect", nil, http.StatusForbidden},
		{http.MethodPut, "/runners/previews/sample/content", nil, http.StatusForbidden},
		{http.MethodPost, "/runners/knowledge/mcp", nil, http.StatusForbidden},
	} {
		rec := requestWithHeaders(t, h, tc.method, tc.path, tc.body, auth)
		if rec.Code != tc.code {
			t.Errorf("%s %s with probe credential: got %d, want %d: %s", tc.method, tc.path, rec.Code, tc.code, rec.Body)
		}
	}
	mixed := append([]domain.RunnerRuntime{{Provider: "mock", Status: domain.RuntimeStatusOnline}}, desktop...)
	mixedID, mixedSession := register("", mixed, http.StatusOK)
	rec = requestWithHeaders(t, h, http.MethodPost, "/runners/tasks/claim", map[string]any{"availableRuntimes": []string{"mock"}, "noWait": true}, map[string]string{"Authorization": "Bearer " + mixedSession})
	if rec.Code != http.StatusOK {
		t.Fatalf("mixed CLI daemon could not claim regular task: %d %s", rec.Code, rec.Body)
	}
	if rotated, _ := register(mixedID, mixed, http.StatusOK); rotated != mixedID {
		t.Fatalf("mixed CLI registration changed id: %s", rotated)
	}
}
