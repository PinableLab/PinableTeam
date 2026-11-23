package feishuregistration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixture(t *testing.T) (*Store, *time.Time, *int) {
	t.Helper()
	now := time.Unix(1000, 0)
	calls := 0
	s := NewStore()
	s.now = func() time.Time { return now }
	s.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != endpoint {
			t.Fatal(r.URL)
		}
		r.ParseForm()
		body := `{"client_id":"cli_test","client_secret":"SERVER_SECRET","bot_name":"test"}`
		if r.Form.Get("action") == "begin" {
			body = `{"device_code":"DEVICE_SECRET","verification_uri_complete":"https://accounts.feishu.cn/verify?x=1","interval":5,"expire_in":600}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	return s, &now, &calls
}
func TestLifecycleAndSecrets(t *testing.T) {
	s, now, calls := fixture(t)
	v, e := s.Begin(context.Background(), 1, "p", "name")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Poll(context.Background(), 2, "p", v.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if _, e = s.Poll(context.Background(), 1, "other", v.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	s.Poll(context.Background(), 1, "p", v.ID)
	if *calls != 1 {
		t.Fatal("early poll")
	}
	*now = now.Add(6 * time.Second)
	v, e = s.Poll(context.Background(), 1, "p", v.ID)
	if e != nil || v.Status != "ready" {
		t.Fatal(v, e)
	}
	if v.BotName != "name" {
		t.Fatalf("upstream name replaced local view: %q", v.BotName)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal("secret leak")
	}
	failure := errors.New("db")
	if e = s.Consume(1, "p", v.ID, func(Credentials) error { return failure }); e != failure {
		t.Fatal(e)
	}
	count := 0
	if e = s.Consume(1, "p", v.ID, func(c Credentials) error {
		count++
		if c.BotName != "name" {
			t.Fatalf("upstream name replaced local name: %q", c.BotName)
		}
		if c.AppSecret != "SERVER_SECRET" {
			t.Fatal(c)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e = s.Consume(1, "p", v.ID, func(Credentials) error { count++; return nil }); !errors.Is(e, ErrNotFound) || count != 1 {
		t.Fatal(e, count)
	}
}
func TestExpiryCancelAndHost(t *testing.T) {
	s, now, _ := fixture(t)
	v, _ := s.Begin(context.Background(), 1, "p", "name")
	*now = now.Add(time.Hour)
	if _, e := s.Poll(context.Background(), 1, "p", v.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	v, _ = s.Begin(context.Background(), 1, "p", "name")
	if e := s.Cancel(1, "p", v.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Poll(context.Background(), 1, "p", v.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	for _, u := range []string{"http://accounts.feishu.cn/a", "https://accounts.feishu.cn.evil/a", "https://open.larksuite.com/a", "https://user@accounts.feishu.cn/a", "https://accounts.feishu.cn:443/a", "https://accounts.feishu.cn/a#x"} {
		if _, e := qrURL(u, "x"); e == nil {
			t.Fatal(u)
		}
	}
}
func TestSlowDownAndCancelInFlight(t *testing.T) {
	s, now, _ := fixture(t)
	v, _ := s.Begin(context.Background(), 1, "p", "name")
	*now = now.Add(6 * time.Second)
	s.client.Transport = transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"slow_down","interval":20}`))}, nil
	})
	v, e := s.Poll(context.Background(), 1, "p", v.ID)
	if e != nil || v.IntervalSec != 20 {
		t.Fatal(v, e)
	}
	*now = now.Add(21 * time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	s.client.Transport = transport(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"client_id":"x","client_secret":"y"}`))}, nil
	})
	done := make(chan error)
	go func() { _, e := s.Poll(context.Background(), 1, "p", v.ID); done <- e }()
	<-entered
	s.Cancel(1, "p", v.ID)
	close(release)
	if e := <-done; !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}
func TestBoundedAndRedactedResponse(t *testing.T) {
	s, _, _ := fixture(t)
	for _, body := range []string{strings.Repeat("x", (1<<20)+1), `{"client_secret":"LEAK"}`, `not json LEAK`} {
		s.client.Transport = transport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		_, e := s.Begin(context.Background(), 1, "p", "n")
		if e == nil || strings.Contains(e.Error(), "LEAK") {
			t.Fatal(e)
		}
	}
}

func TestRejectLarkAndRedirect(t *testing.T) {
	s, now, _ := fixture(t)
	v, _ := s.Begin(context.Background(), 1, "p", "name")
	*now = now.Add(6 * time.Second)
	s.client.Transport = transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"client_id":"x","client_secret":"y","user_info":{"tenant_brand":"lark"}}`))}, nil
	})
	if _, e := s.Poll(context.Background(), 1, "p", v.ID); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	calls := 0
	s.client.Transport = transport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://evil.example/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, e := s.Begin(context.Background(), 1, "p", "name"); e == nil || calls != 1 {
		t.Fatal(e, calls)
	}
}
func TestOwnerLimit(t *testing.T) {
	s, _, _ := fixture(t)
	for i := 0; i < 3; i++ {
		if _, e := s.Begin(context.Background(), 1, "p", "n"); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.Begin(context.Background(), 1, "q", "n"); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
}

func TestOfficialVerificationPageAndUserCode(t *testing.T) {
	for _, raw := range []string{"https://open.feishu.cn/page/cli?user_code=abc", "https://accounts.feishu.cn/verify?code=abc"} {
		if _, e := qrURL(raw, "bot"); e != nil {
			t.Fatal(raw, e)
		}
	}
	for _, raw := range []string{"https://open.feishu.cn/page/cli/", "https://open.feishu.cn/page/launcher", "https://open.feishu.cn/%70age/cli", "https://open.feishu.cn/page/cli/../evil", "https://open.feishu.cn:443/page/cli", "https://open.feishu.cn.evil/page/cli"} {
		if _, e := qrURL(raw, "bot"); e == nil {
			t.Fatal(raw)
		}
	}
	s, now, _ := fixture(t)
	s.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
		r.ParseForm()
		if r.Form.Get("request_user_info") != "open_id tenant_brand" {
			t.Fatal(r.Form)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"device_code":"d","user_code":"a&b","verification_uri":"https://open.feishu.cn/page/launcher"}`))}, nil
	})
	v, e := s.Begin(context.Background(), 1, "p", "bot")
	if e != nil || !strings.Contains(v.QRURL, "user_code=a%26b") {
		t.Fatal(v, e)
	}
	*now = now.Add(6 * time.Second)
	s.client.Transport = transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"authorization_pending","code":20094}`))}, nil
	})
	v, e = s.Poll(context.Background(), 1, "p", v.ID)
	if e != nil || v.Status != "pending" {
		t.Fatal(v, e)
	}
	*now = now.Add(6 * time.Second)
	s.client.Transport = transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`))}, nil
	})
	if _, e = s.Poll(context.Background(), 1, "p", v.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}
