package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentflow/internal/domain"
)

const feishuAPI = "https://open.feishu.cn/open-apis"

// FeishuProvider uses a self-built app's tenant token. Credentials and tokens
// are never persisted, included in URLs or returned in errors.
// API contracts: https://open.feishu.cn/document/server-docs/group/chat/list
// and https://open.feishu.cn/document/server-docs/im-v1/message/create.
type FeishuProvider struct{ HTTP *http.Client }

func NewFeishuProvider() *FeishuProvider {
	return &FeishuProvider{HTTP: &http.Client{Timeout: 15 * time.Second}}
}

type FeishuProviderError struct{ Code int }

func (e *FeishuProviderError) Error() string {
	return fmt.Sprintf("飞书请求失败（错误码 %d），请检查应用凭据、权限和目标群聊", e.Code)
}

type feishuEnvelope struct {
	Code  *int            `json:"code"`
	Token string          `json:"tenant_access_token"`
	Data  json.RawMessage `json:"data"`
}

func (p *FeishuProvider) request(ctx context.Context, method, path, token string, body any) (feishuEnvelope, error) {
	var result feishuEnvelope
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return result, errors.New("飞书请求编码失败")
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, feishuAPI+path, payload)
	if err != nil {
		return result, errors.New("飞书请求创建失败")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Copy rather than mutate the injected client; even an injected client may
	// not forward credentials to redirects (including same-host redirects).
	client := http.Client{Timeout: 15 * time.Second}
	if p.HTTP != nil {
		client = *p.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return result, errors.New("飞书网络请求失败，请稍后重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, errors.New("飞书服务请求失败，请检查权限或稍后重试")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &result) != nil || result.Code == nil {
		return result, errors.New("飞书响应格式异常")
	}
	if *result.Code != 0 {
		return result, &FeishuProviderError{Code: *result.Code}
	}
	return result, nil
}

func (p *FeishuProvider) token(ctx context.Context, appID, secret string) (string, error) {
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(secret) == "" {
		return "", errors.New("请填写飞书 App ID 和 App Secret")
	}
	result, err := p.request(ctx, http.MethodPost, "/auth/v3/tenant_access_token/internal", "", map[string]string{"app_id": appID, "app_secret": secret})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Token) == "" {
		return "", errors.New("飞书鉴权未返回令牌")
	}
	return result.Token, nil
}

// Sessions lists only actual groups the bot has joined. Unlike WeCom's session
// list this endpoint cannot discover direct chats; do not fabricate recipients.
func (p *FeishuProvider) Sessions(ctx context.Context, appID, secret string) ([]domain.BotChat, error) {
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	token, err := p.token(ctx, appID, secret)
	if err != nil {
		return nil, err
	}
	chats := []domain.BotChat{}
	seenIDs := map[string]bool{}
	seenPages := map[string]bool{}
	pageToken := ""
	for page := 0; page < 100; page++ {
		query := url.Values{"page_size": {"100"}}
		if pageToken != "" {
			query.Set("page_token", pageToken)
		}
		result, err := p.request(ctx, http.MethodGet, "/im/v1/chats?"+query.Encode(), token, nil)
		if err != nil {
			return nil, err
		}
		var data struct {
			Items []struct {
				ID   string `json:"chat_id"`
				Name string `json:"name"`
			} `json:"items"`
			More bool   `json:"has_more"`
			Next string `json:"page_token"`
		}
		if len(result.Data) == 0 || string(result.Data) == "null" || json.Unmarshal(result.Data, &data) != nil {
			return nil, errors.New("飞书群聊列表格式异常")
		}
		for _, item := range data.Items {
			if item.ID != "" && len(item.ID) <= 191 && !seenIDs[item.ID] {
				seenIDs[item.ID] = true
				chats = append(chats, domain.BotChat{ID: item.ID, Name: item.Name, Type: "group"})
			}
		}
		if !data.More {
			return chats, nil
		}
		if data.Next == "" || seenPages[data.Next] {
			return nil, errors.New("飞书群聊分页响应异常")
		}
		seenPages[data.Next] = true
		pageToken = data.Next
	}
	return nil, errors.New("飞书群聊数量超出安全分页限制")
}

func (p *FeishuProvider) Send(ctx context.Context, appID, secret, target, content string) error {
	if strings.TrimSpace(target) == "" || len(target) > 191 {
		return errors.New("请选择有效的飞书群聊")
	}
	if len([]byte(content)) == 0 || len([]byte(content)) > 20480 {
		return errors.New("通知正文必须为 1 至 20480 字节")
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	token, err := p.token(ctx, appID, secret)
	if err != nil {
		return err
	}
	text, _ := json.Marshal(map[string]string{"text": content})
	result, err := p.request(ctx, http.MethodPost, "/im/v1/messages?receive_id_type=chat_id", token, map[string]string{"receive_id": target, "msg_type": "text", "content": string(text)})
	if err != nil {
		return err
	}
	var data struct {
		ID string `json:"message_id"`
	}
	if json.Unmarshal(result.Data, &data) != nil || data.ID == "" {
		return errors.New("飞书未确认发送成功")
	}
	return nil
}
