package domain

import "strings"

// NotificationChannel keeps existing stored bots on their original WeCom channel.
func NotificationChannel(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return "wecom"
	}
	return channel
}
