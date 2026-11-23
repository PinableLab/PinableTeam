package domain

import "testing"

func TestNotificationChannel(t *testing.T) {
	for raw, want := range map[string]string{"": "wecom", "wecom": "wecom", " FEISHU ": "feishu", "unknown": "unknown"} {
		if got := NotificationChannel(raw); got != want {
			t.Errorf("%q: got %q want %q", raw, got, want)
		}
	}
}
