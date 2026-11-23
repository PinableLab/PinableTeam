package notification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type feishuTransport func(*http.Request) (*http.Response, error)

func (f feishuTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func feishuClient(t *testing.T, fn func(*http.Request) string) *FeishuProvider {
	t.Helper()
	return &FeishuProvider{HTTP: &http.Client{Transport: feishuTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "open.feishu.cn" {
			t.Fatalf("unexpected endpoint %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fn(r))), Request: r}, nil
	})}}
}
func TestFeishuGroupsPagination(t *testing.T) {
	calls := 0
	p := feishuClient(t, func(r *http.Request) string {
		calls++
		if calls == 1 {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["app_id"] != "app" || body["app_secret"] != "secret" || r.Header.Get("Authorization") != "" {
				t.Fatal("bad token request")
			}
			return `{"code":0,"tenant_access_token":"token"}`
		}
		if r.Header.Get("Authorization") != "Bearer token" || r.Method != "GET" {
			t.Fatal("missing authentication")
		}
		if calls == 2 {
			return `{"code":0,"data":{"items":[{"chat_id":"oc_one","name":"群一"}],"has_more":true,"page_token":"next /+"}}`
		}
		if r.URL.Query().Get("page_token") != "next /+" {
			t.Fatal("page token not escaped")
		}
		return `{"code":0,"data":{"items":[{"chat_id":"oc_one"},{"chat_id":"oc_two","name":"群二"}],"has_more":false}}`
	})
	chats, err := p.Sessions(context.Background(), "app", "secret")
	if err != nil || len(chats) != 2 || chats[0].Type != "group" || calls != 3 {
		t.Fatalf("chats=%v err=%v calls=%d", chats, err, calls)
	}
}
func TestFeishuSendNestedContent(t *testing.T) {
	p := feishuClient(t, func(r *http.Request) string {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			return `{"code":0,"tenant_access_token":"token"}`
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		var content map[string]string
		json.Unmarshal([]byte(body["content"]), &content)
		if r.URL.Query().Get("receive_id_type") != "chat_id" || body["receive_id"] != "oc_target" || body["msg_type"] != "text" || content["text"] != "a\n\"测试\"" {
			t.Fatalf("unexpected body %v", body)
		}
		return `{"code":0,"data":{"message_id":"om_sent"}}`
	})
	if err := p.Send(context.Background(), "app", "secret", "oc_target", "a\n\"测试\""); err != nil {
		t.Fatal(err)
	}
}
func TestFeishuRejectsInvalidResponsesWithoutLeaking(t *testing.T) {
	for _, response := range []string{`{}`, `{"code":1,"msg":"secret token"}`, `{"code":0}`, strings.Repeat("x", (1<<20)+1)} {
		p := feishuClient(t, func(*http.Request) string { return response })
		_, err := p.Sessions(context.Background(), "app", "secret")
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
			t.Fatalf("unsafe result: %v", err)
		}
	}
}
func TestFeishuRejectsPaginationLoop(t *testing.T) {
	p := feishuClient(t, func(r *http.Request) string {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			return `{"code":0,"tenant_access_token":"token"}`
		}
		return `{"code":0,"data":{"items":[],"has_more":true,"page_token":"same"}}`
	})
	if _, err := p.Sessions(context.Background(), "app", "secret"); err == nil {
		t.Fatal("accepted loop")
	}
}
func TestFeishuRejectsUnconfirmedSend(t *testing.T) {
	p := feishuClient(t, func(r *http.Request) string {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			return `{"code":0,"tenant_access_token":"token"}`
		}
		return `{"code":0,"data":{}}`
	})
	if err := p.Send(context.Background(), "app", "secret", "oc_target", "text"); err == nil {
		t.Fatal("accepted missing message ID")
	}
}
func TestFeishuRejectsRedirectAndInvalidInput(t *testing.T) {
	calls := 0
	p := &FeishuProvider{HTTP: &http.Client{Transport: feishuTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://attacker.invalid/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}}
	if _, err := p.Sessions(context.Background(), "app", "secret"); err == nil || calls != 1 {
		t.Fatalf("redirect followed calls=%d err=%v", calls, err)
	}
	for _, args := range [][2]string{{"", "text"}, {"oc_target", ""}, {"oc_target", strings.Repeat("a", 20481)}} {
		if err := p.Send(context.Background(), "app", "secret", args[0], args[1]); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if calls != 1 {
		t.Fatal("sent invalid input")
	}
}

func TestFeishuRateLimitDoesNotBlindlyRetrySend(t *testing.T) {
	calls := 0
	p := &FeishuProvider{HTTP: &http.Client{Transport: feishuTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		status, body := 200, `{"code":0,"tenant_access_token":"token"}`
		if calls > 1 {
			status, body = 429, `{"msg":"secret token"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}}
	err := p.Send(context.Background(), "app", "secret", "oc_target", "text")
	if err == nil || calls != 2 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
