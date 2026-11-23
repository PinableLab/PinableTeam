// Package feishuregistration implements temporary, server-only Feishu enrollment.
package feishuregistration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("飞书注册会话不存在或已过期")
	ErrNotReady = errors.New("飞书注册尚未完成")
	ErrUpstream = errors.New("飞书注册服务暂不可用，请重试")
	ErrInvalid  = errors.New("飞书注册响应无效或不受支持")
	ErrLimit    = errors.New("注册请求过多，请稍后重试")
)

const endpoint = "https://accounts.feishu.cn/oauth/v1/app/registration"

type View struct {
	ID          string    `json:"id"`
	QRURL       string    `json:"qrURL"`
	Status      string    `json:"status"`
	ExpiresAt   time.Time `json:"expiresAt"`
	IntervalSec int       `json:"intervalSec"`
	AppID       string    `json:"appId,omitempty"`
	BotName     string    `json:"botName,omitempty"`
}

// Credentials must never be serialized in an HTTP response.
type Credentials struct {
	AppID     string `json:"-"`
	AppSecret string `json:"-"`
	BotName   string `json:"-"`
}
type entry struct {
	view            View
	owner           int64
	project, device string
	credentials     Credentials
	next            time.Time
	polling         bool
}
type Store struct {
	mu          sync.Mutex
	items       map[string]*entry
	client      *http.Client
	now         func() time.Time
	beginning   int
	beginOwners map[int64]int
}

func NewStore() *Store {
	return &Store{items: map[string]*entry{}, beginOwners: map[int64]int{}, client: &http.Client{Timeout: 15 * time.Second}, now: time.Now}
}
func (s *Store) cleanup() {
	for id, e := range s.items {
		if !s.now().Before(e.view.ExpiresAt) {
			delete(s.items, id)
		}
	}
}
func (s *Store) get(owner int64, project, id string) (*entry, error) {
	s.cleanup()
	e := s.items[id]
	if owner <= 0 || e == nil || e.owner != owner || e.project != project {
		return nil, ErrNotFound
	}
	return e, nil
}
func (s *Store) request(ctx context.Context, form url.Values) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, ErrUpstream
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, ErrUpstream
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return nil, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, ErrInvalid
	}
	if res.StatusCode != 200 && res.StatusCode != 400 {
		return nil, ErrUpstream
	}
	return raw, nil
}
func qrURL(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || (u.Host != "accounts.feishu.cn" && !(u.Host == "open.feishu.cn" && u.Path == "/page/cli" && u.RawPath == "")) || u.User != nil || u.Fragment != "" {
		return "", ErrInvalid
	}
	q := u.Query()
	q.Set("from", "cli")
	q.Del("tp")
	q.Set("source", "go-sdk/loopforge")
	q.Set("name", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func (s *Store) Begin(ctx context.Context, owner int64, project, name string) (View, error) {
	name = strings.TrimSpace(name)
	project = strings.TrimSpace(project)
	if owner <= 0 || project == "" || name == "" || len([]rune(name)) > 48 {
		return View{}, ErrInvalid
	}
	s.mu.Lock()
	s.cleanup()
	count := 0
	for _, e := range s.items {
		if e.owner == owner {
			count++
		}
	}
	if len(s.items)+s.beginning >= 256 || count+s.beginOwners[owner] >= 3 || s.beginning >= 8 {
		s.mu.Unlock()
		return View{}, ErrLimit
	}
	s.beginning++
	s.beginOwners[owner]++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.beginning--; s.beginOwners[owner]--; s.mu.Unlock() }()
	raw, err := s.request(ctx, url.Values{"action": {"begin"}, "archetype": {"PersonalAgent"}, "auth_method": {"client_secret"}, "request_user_info": {"open_id tenant_brand"}})
	if err != nil {
		return View{}, err
	}
	var r struct {
		Device   string `json:"device_code"`
		UserCode string `json:"user_code"`
		Error    string `json:"error"`
		Complete string `json:"verification_uri_complete"`
		URI      string `json:"verification_uri"`
		Brand    string `json:"tenant_brand"`
		Expire   int    `json:"expire_in"`
		Expires  int    `json:"expires_in"`
		Interval int    `json:"interval"`
		Code     int    `json:"code"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Code != 0 || r.Error != "" || r.Device == "" || (r.Brand != "" && r.Brand != "feishu") {
		return View{}, ErrInvalid
	}
	if r.UserCode != "" {
		// Official larksuite/cli constructs /page/cli from user_code; the raw
		// verification URI may point at a different onboarding page.
		r.Complete = "https://open.feishu.cn/page/cli?" + url.Values{"user_code": {r.UserCode}}.Encode()
	} else if r.Complete == "" {
		r.Complete = r.URI
	}
	qr, err := qrURL(r.Complete, name)
	if err != nil {
		return View{}, err
	}
	ttl := r.Expire
	if ttl <= 0 {
		ttl = r.Expires
	}
	if ttl <= 0 {
		ttl = 600
	}
	if ttl > 1800 {
		ttl = 1800
	}
	if r.Interval <= 0 {
		r.Interval = 5
	}
	if r.Interval > 1800 {
		return View{}, ErrInvalid
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return View{}, ErrUpstream
	}
	now := s.now()
	v := View{ID: hex.EncodeToString(b), QRURL: qr, Status: "pending", ExpiresAt: now.Add(time.Duration(ttl) * time.Second), IntervalSec: r.Interval, BotName: name}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[v.ID] = &entry{view: v, owner: owner, project: project, device: r.Device, next: now.Add(time.Duration(r.Interval) * time.Second)}
	return v, nil
}
func (s *Store) Poll(ctx context.Context, owner int64, project, id string) (View, error) {
	s.mu.Lock()
	e, err := s.get(owner, project, id)
	if err != nil {
		s.mu.Unlock()
		return View{}, err
	}
	if e.view.Status != "pending" || e.polling || s.now().Before(e.next) {
		v := e.view
		s.mu.Unlock()
		return v, nil
	}
	e.polling = true
	e.next = s.now().Add(time.Duration(e.view.IntervalSec) * time.Second)
	device := e.device
	s.mu.Unlock()
	raw, callErr := s.request(ctx, url.Values{"action": {"poll"}, "device_code": {device}})
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.get(owner, project, id)
	if err != nil || current != e {
		return View{}, ErrNotFound
	}
	e.polling = false
	if callErr != nil {
		return e.view, callErr
	}
	var r struct {
		Status, Error string
		Code          int
		AppID         string `json:"app_id"`
		AppSecret     string `json:"app_secret"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		BotName       string `json:"bot_name"`
		AppName       string `json:"app_name"`
		Interval      int
		Brand         string `json:"tenant_brand"`
		UserInfo      struct {
			Brand string `json:"tenant_brand"`
		} `json:"user_info"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return e.view, ErrInvalid
	}
	if (r.Brand != "" && r.Brand != "feishu") || (r.UserInfo.Brand != "" && r.UserInfo.Brand != "feishu") {
		delete(s.items, id)
		return View{}, ErrInvalid
	}
	state := r.Status
	if state == "" {
		state = r.Error
	}
	if r.AppID == "" {
		r.AppID = r.ClientID
	}
	if r.AppSecret == "" {
		r.AppSecret = r.ClientSecret
	}
	if state == "" && r.Code == 0 && r.AppID != "" && r.AppSecret != "" {
		state = "authorized"
	}
	switch state {
	case "authorization_pending", "polling":
	case "slow_down":
		if r.Interval > e.view.IntervalSec {
			e.view.IntervalSec = r.Interval
		} else {
			e.view.IntervalSec += 5
		}
		if e.view.IntervalSec > 1800 {
			e.view.IntervalSec = 1800
		}
		e.next = s.now().Add(time.Duration(e.view.IntervalSec) * time.Second)
	case "authorized", "success", "done", "credentials_ready":
		if r.Code != 0 || strings.TrimSpace(r.AppID) == "" || strings.TrimSpace(r.AppSecret) == "" || len(r.AppID) > 191 || len(r.AppSecret) > 512 {
			return e.view, ErrInvalid
		}
		// The project-local name is chosen by the authenticated user, not upstream.
		e.credentials = Credentials{r.AppID, r.AppSecret, e.view.BotName}
		e.device = ""
		e.view.AppID = r.AppID
		e.view.Status = "ready"
	case "access_denied", "denied", "rejected":
		e.device = ""
		e.view.Status = "denied"
	case "expired", "expire", "expired_token", "invalid_grant":
		delete(s.items, id)
		return View{}, ErrNotFound
	default:
		return e.view, ErrInvalid
	}
	return e.view, nil
}
func (s *Store) Cancel(owner int64, project, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.get(owner, project, id); err != nil {
		return err
	}
	delete(s.items, id)
	return nil
}

// Consume serializes completion with cancellation/polling. Save must be a short,
// transactional local persistence operation, never network I/O or Store reentry.
func (s *Store) Consume(owner int64, project, id string, save func(Credentials) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.get(owner, project, id)
	if err != nil {
		return err
	}
	if e.view.Status != "ready" || save == nil {
		return ErrNotReady
	}
	if err := save(e.credentials); err != nil {
		return err
	}
	delete(s.items, id)
	return nil
}
