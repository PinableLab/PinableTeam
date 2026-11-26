package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
)

const (
	RunnerDesktopProbeIDPrefix   = "runner-desktop-"
	RunnerProviderDesktop        = "pinable-desktop"
	RunnerTaskActionDesktopProbe = "desktop_probe_v1"
	RunnerCapabilityDesktopProbe = "desktop.probe/v1"
)

// OnlyDesktopRuntimes identifies registrations that need a probe-scoped session.
// A normal daemon may advertise both CLI adapters and the desktop adapter.
func OnlyDesktopRuntimes(runtimes []RunnerRuntime) bool {
	return len(runtimes) > 0 && !slices.ContainsFunc(runtimes, func(rt RunnerRuntime) bool {
		return !strings.EqualFold(strings.TrimSpace(rt.Provider), RunnerProviderDesktop)
	})
}

func (r Runner) SupportsDesktopProbe() bool {
	if r.Status != RunnerStatusOnline {
		return false
	}
	for _, rt := range r.Runtimes {
		if rt.Provider == RunnerProviderDesktop && rt.Status == RuntimeStatusOnline && slices.Contains(rt.Capabilities, RunnerCapabilityDesktopProbe) {
			return true
		}
	}
	return false
}

func (task RunnerTask) IsDesktopProbe() bool {
	return task.Provider == RunnerProviderDesktop && task.Action == RunnerTaskActionDesktopProbe && task.RunnerID != ""
}

// DesktopProbeReceipt exposes the probe protocol's routing, lifecycle and result
// fields when a machine reads a claim or an old task by ID.
func (task RunnerTask) DesktopProbeReceipt() RunnerTask {
	return RunnerTask{
		ID: task.ID, Provider: task.Provider, RunnerID: task.RunnerID, Action: task.Action,
		Status: task.Status, DesiredState: task.DesiredState, Attempt: task.Attempt, MaxAttempts: task.MaxAttempts,
		LeaseOwner: task.LeaseOwner, LeaseUntil: task.LeaseUntil, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
		StartedAt: task.StartedAt, CompletedAt: task.CompletedAt, ExitCode: task.ExitCode, Result: task.Result, Error: task.Error,
	}
}

// ValidDesktopProbeForQueue admits only metadata needed to create a new probe.
// New execution fields on RunnerTask stay excluded until explicitly supported.
func (task RunnerTask) ValidDesktopProbeForQueue() bool {
	if !task.IsDesktopProbe() || strings.TrimSpace(task.RunnerID) == "" {
		return false
	}
	envelope := RunnerTask{
		ID: task.ID, Provider: RunnerProviderDesktop, RunnerID: task.RunnerID,
		Action: RunnerTaskActionDesktopProbe, Status: RunnerTaskQueued, MaxAttempts: 1,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
	return reflect.DeepEqual(task, envelope)
}

// DecodeManualRunnerTask parses the management API's task creation request.
// The desktop protocol is closed to new fields until both peers support them.
func DecodeManualRunnerTask(body io.Reader) (RunnerTask, error) {
	dec := json.NewDecoder(body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return RunnerTask{}, err
	}
	var input struct {
		Provider    string   `json:"provider"`
		RunnerID    string   `json:"runnerId"`
		Prompt      string   `json:"prompt"`
		Workspace   string   `json:"workspace"`
		Args        []string `json:"args"`
		MaxAttempts int      `json:"maxAttempts"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return RunnerTask{}, err
	}
	provider := strings.TrimSpace(input.Provider)
	if strings.EqualFold(provider, RunnerProviderDesktop) {
		if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
			return RunnerTask{}, errors.New("桌面探测请求只能包含一个 JSON 对象")
		}
		return parseDesktopProbe(raw)
	}
	if provider == "" {
		return RunnerTask{}, errors.New("provider 不能为空")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return RunnerTask{}, errors.New("prompt 不能为空")
	}
	return RunnerTask{
		Provider: provider, RunnerID: strings.TrimSpace(input.RunnerID),
		Prompt: input.Prompt, Workspace: strings.TrimSpace(input.Workspace), Args: input.Args,
		Status: RunnerTaskQueued, MaxAttempts: max(1, min(5, input.MaxAttempts)),
	}, nil
}

func parseDesktopProbe(raw []byte) (RunnerTask, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return RunnerTask{}, err
	}
	fields := make(map[string]string, 3)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return RunnerTask{}, err
		}
		key, ok := token.(string)
		if !ok || (key != "provider" && key != "runnerId" && key != "action") {
			return RunnerTask{}, fmt.Errorf("桌面探测不接受字段 %q", key)
		}
		if _, exists := fields[key]; exists {
			return RunnerTask{}, fmt.Errorf("桌面探测字段 %q 重复", key)
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return RunnerTask{}, err
		}
		fields[key] = value
	}
	if fields["provider"] != RunnerProviderDesktop || fields["action"] != RunnerTaskActionDesktopProbe {
		return RunnerTask{}, errors.New("桌面探测需要 pinable-desktop 和 desktop_probe_v1")
	}
	runnerID := strings.TrimSpace(fields["runnerId"])
	if runnerID == "" {
		return RunnerTask{}, errors.New("桌面探测需要指定 runnerId")
	}
	return RunnerTask{
		Provider: RunnerProviderDesktop, Action: RunnerTaskActionDesktopProbe, RunnerID: runnerID,
		Status: RunnerTaskQueued, MaxAttempts: 1,
	}, nil
}
