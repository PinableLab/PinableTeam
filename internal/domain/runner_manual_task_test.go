package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestDesktopProbeRequest(t *testing.T) {
	const prefix = `{"provider":"pinable-desktop","action":"desktop_probe_v1","runnerId":"rd-1"`
	created, err := DecodeManualRunnerTask(strings.NewReader(prefix + "}"))
	want := RunnerTask{Provider: RunnerProviderDesktop, Action: RunnerTaskActionDesktopProbe, RunnerID: "rd-1", Status: RunnerTaskQueued, MaxAttempts: 1}
	if err != nil || !reflect.DeepEqual(created, want) {
		t.Fatalf("probe task = %+v, err = %v", created, err)
	}
	for _, body := range []string{
		`{"provider":"pinable-desktop","runnerId":"rd"}`,
		`{"provider":"pinable-desktop","runnerId":"rd","action":"future"}`,
		`{"provider":"PINABLE-DESKTOP","runnerId":"rd","action":"desktop_probe_v1"}`,
		`{"provider":"pinable-desktop","action":"desktop_probe_v1","runnerId":"  "}`,
		`{"provider":"pinable-desktop","action":"desktop_probe_v1"}`,
		prefix + `,"prompt":""}`, prefix + `,"args":[]}`, prefix + `,"workspace":null}`,
		prefix + `,"invocation":{}}`, prefix + `,"command":"id"}`, prefix + `,"sandbox":{}}`,
		prefix + `,"unknown":true}`, prefix + `,"maxAttempts":1}`, prefix + `,"runnerId":"other"}`,
		prefix + `,"RunnerID":"other"}`, prefix + "} {}", prefix + "} junk",
	} {
		if _, err := DecodeManualRunnerTask(strings.NewReader(body)); err == nil {
			t.Errorf("accepted invalid probe %s", body)
		}
	}
}

func TestDesktopProbeRequiresOnlineRuntimeAdvertisement(t *testing.T) {
	runtime := RunnerRuntime{Provider: RunnerProviderDesktop, Status: RuntimeStatusOnline, Capabilities: []string{RunnerCapabilityDesktopProbe}}
	if !(Runner{Status: RunnerStatusOnline, Runtimes: []RunnerRuntime{runtime}}).SupportsDesktopProbe() {
		t.Fatal("online desktop runtime was rejected")
	}
	for _, runner := range []Runner{
		{Status: RunnerStatusOffline, Runtimes: []RunnerRuntime{runtime}},
		{Status: RunnerStatusOnline, Runtimes: []RunnerRuntime{{Provider: "mock", Status: RuntimeStatusOnline, Capabilities: runtime.Capabilities}}},
		{Status: RunnerStatusOnline, Runtimes: []RunnerRuntime{{Provider: RunnerProviderDesktop, Status: RuntimeStatusDisabled, Capabilities: runtime.Capabilities}}},
		{Status: RunnerStatusOnline, Runtimes: []RunnerRuntime{{Provider: RunnerProviderDesktop, Status: RuntimeStatusOnline, Capabilities: []string{"desktop.probe/v2"}}}},
	} {
		if runner.SupportsDesktopProbe() {
			t.Errorf("accepted incompatible runner %+v", runner)
		}
	}
}

func TestManualRunnerTaskPreservesCLIInput(t *testing.T) {
	const input = `{"provider":" mock ","prompt":" old prompt ","workspace":" dir ","args":["-a"],"maxAttempts":20,"unknown":true} {}`
	task, err := DecodeManualRunnerTask(strings.NewReader(input))
	want := RunnerTask{Provider: "mock", Prompt: " old prompt ", Workspace: "dir", Args: []string{"-a"}, Status: RunnerTaskQueued, MaxAttempts: 5}
	if err != nil || !reflect.DeepEqual(task, want) {
		t.Fatalf("CLI task = %+v, err = %v", task, err)
	}
}

func TestDesktopProbeStoreAndResponseEnvelope(t *testing.T) {
	base := RunnerTask{ID: "rt", Provider: RunnerProviderDesktop, Action: RunnerTaskActionDesktopProbe, RunnerID: "rd", Status: RunnerTaskQueued, MaxAttempts: 1}
	if !base.ValidDesktopProbeForQueue() {
		t.Fatal("valid probe was rejected")
	}
	dirty := base
	dirty.Invocation = &RunnerInvocation{}
	dirty.Command = "unexpected"
	dirty.Workspace = "/private"
	if dirty.ValidDesktopProbeForQueue() {
		t.Fatal("accepted execution payload")
	}
	if got := dirty.DesktopProbeReceipt(); !reflect.DeepEqual(got, base) {
		t.Fatalf("unexpected executed payload in receipt: %+v", got)
	}
	dirty = base
	dirty.MaxAttempts = 2
	if dirty.ValidDesktopProbeForQueue() {
		t.Fatal("accepted a multi-attempt task")
	}
}
