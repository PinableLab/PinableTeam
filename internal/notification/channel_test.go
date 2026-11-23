package notification

import (
	"context"
	"testing"

	"agentflow/internal/domain"
)

type channelTestProvider struct{ id int }

func (*channelTestProvider) Sessions(context.Context, string, string) ([]domain.BotChat, error) {
	return nil, nil
}
func (*channelTestProvider) Send(context.Context, string, string, string, string) error { return nil }
func TestProviderForChannel(t *testing.T) {
	wecom, feishu := &channelTestProvider{id: 1}, &channelTestProvider{id: 2}
	s := &Service{Provider: wecom, FeishuProvider: feishu}
	for _, tc := range []struct {
		channel string
		want    Provider
	}{{"", wecom}, {"wecom", wecom}, {"feishu", feishu}} {
		got, err := s.providerFor(domain.NotificationBot{Channel: tc.channel})
		if err != nil || got != tc.want {
			t.Fatalf("channel %q: %v", tc.channel, err)
		}
	}
	if _, err := s.providerFor(domain.NotificationBot{Channel: "unknown"}); err == nil {
		t.Fatal("unknown channel must not use WeCom credentials")
	}
	s.FeishuProvider = nil
	if _, err := s.providerFor(domain.NotificationBot{Channel: "feishu"}); err == nil {
		t.Fatal("missing provider must fail closed")
	}
}
