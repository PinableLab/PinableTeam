package notification

import (
	"errors"

	"agentflow/internal/domain"
)

func (s *Service) providerFor(bot domain.NotificationBot) (Provider, error) {
	switch domain.NotificationChannel(bot.Channel) {
	case "wecom":
		if s.Provider != nil {
			return s.Provider, nil
		}
	case "feishu":
		if s.FeishuProvider != nil {
			return s.FeishuProvider, nil
		}
	}
	return nil, errors.New("通知渠道不可用")
}
