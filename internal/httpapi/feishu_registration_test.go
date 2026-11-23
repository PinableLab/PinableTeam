package httpapi

import (
	"agentflow/internal/auth"
	"agentflow/internal/domain"
	"agentflow/internal/feishuregistration"
	"agentflow/internal/repo"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type registrationFake struct {
	owner    int64
	project  string
	calls    int
	consumed bool
}

func (f *registrationFake) Begin(_ context.Context, o int64, p, n string) (feishuregistration.View, error) {
	f.calls++
	f.owner = o
	f.project = p
	return feishuregistration.View{ID: "reg", QRURL: "https://accounts.feishu.cn/registration?code=fixture", Status: "pending", BotName: n}, nil
}
func (f *registrationFake) Poll(_ context.Context, o int64, p, id string) (feishuregistration.View, error) {
	f.calls++
	if o != f.owner || p != f.project || id != "reg" {
		return feishuregistration.View{}, feishuregistration.ErrNotFound
	}
	return feishuregistration.View{ID: id, Status: "ready", AppID: "app-fixture"}, nil
}
func (f *registrationFake) Cancel(o int64, p, id string) error {
	f.calls++
	if o != f.owner || p != f.project || id != "reg" {
		return feishuregistration.ErrNotFound
	}
	return nil
}
func (f *registrationFake) Consume(o int64, p, id string, save func(feishuregistration.Credentials) error) error {
	f.calls++
	if f.consumed || o != f.owner || p != f.project || id != "reg" {
		return feishuregistration.ErrNotFound
	}
	if e := save(feishuregistration.Credentials{AppID: "app-fixture", AppSecret: "secret-not-for-browser", BotName: "测试Bot"}); e != nil {
		return e
	}
	f.consumed = true
	return nil
}

func TestFeishuRegistrationHTTPAuthorizationAndDisabledSave(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	users := map[string]domain.User{}
	for _, name := range []string{"owner", "member", "viewer", "outsider"} {
		if err := s.PutUser(domain.User{LoginName: name}); err != nil {
			t.Fatal(err)
		}
		users[name], _ = s.UserByLoginName(name)
	}
	project := domain.Project{ID: "p", Name: "P", OwnerID: users["owner"].ID, Members: []domain.ProjectMember{{UserID: users["member"].ID, Role: "member"}, {UserID: users["viewer"].ID, Role: "viewer"}}}
	if err := s.PutProject(project); err != nil {
		t.Fatal(err)
	}
	d := &deps{repo: s, authClient: auth.NewClient(auth.Config{SessionSecret: "registration-test-only-secret"})}
	fake := &registrationFake{}
	h := &feishuRegistrationHandlers{deps: d, store: fake}
	g := gin.New()
	base := "/api/notification-bot-registrations/feishu"
	g.POST(base, h.handle)
	g.POST(base+"/:id/poll", h.handle)
	g.POST(base+"/:id/consume", h.handle)
	g.DELETE(base+"/:id", h.handle)
	request := func(login, method, path, origin, authorization, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://loopforge.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("Authorization", authorization)
		if login != "" {
			u := users[login]
			token, e := d.authClient.SignUser(&auth.User{ID: u.ID, LoginName: u.LoginName, AuthTime: time.Now().UTC()})
			if e != nil {
				t.Fatal(e)
			}
			r.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	body := `{"projectId":"p","name":"测试Bot"}`
	for _, tc := range []struct {
		login, origin, token string
		want                 int
	}{{"", "https://loopforge.test", "", 401}, {"owner", "https://evil.test", "", 403}, {"owner", "", "", 403}, {"owner", "https://loopforge.test", "Bearer forged", 401}, {"viewer", "https://loopforge.test", "", 403}, {"outsider", "https://loopforge.test", "", 403}} {
		w := request(tc.login, "POST", base, tc.origin, tc.token, body)
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	if fake.calls != 0 {
		t.Fatal("unauthorized registration reached upstream")
	}
	w := request("owner", "POST", base, "https://loopforge.test", "", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "data:image/png;base64,") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("begin %d %s", w.Code, w.Body.String())
	}
	w = request("member", "POST", base+"/reg/poll", "https://loopforge.test", "", body)
	if w.Code != 404 {
		t.Fatal("another project writer polled owner's credentials")
	}
	// 成员资格在注册后撤销，后续轮询和保存都必须重新检查。
	fake.owner = users["member"].ID
	// PutProject treats nil as a settings-only update and preserves membership.
	project.Members = []domain.ProjectMember{}
	if err := s.PutProject(project); err != nil {
		t.Fatal(err)
	}
	if stored, ok := s.Project(project.ID); !ok || len(stored.Members) != 0 {
		t.Fatal("fixture did not revoke membership")
	}
	beforeRevokeCalls := fake.calls
	for _, suffix := range []string{"/poll", "/consume"} {
		w = request("member", "POST", base+"/reg"+suffix, "https://loopforge.test", "", body)
		if w.Code != 403 {
			t.Fatal("revoked access accepted")
		}
	}
	if fake.calls != beforeRevokeCalls {
		t.Fatal("revoked request reached registration store")
	}
	fake.owner = users["owner"].ID
	w = request("owner", "POST", base+"/reg/consume", "https://loopforge.test", "", body)
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret-not-for-browser") {
		t.Fatalf("consume %d %s", w.Code, w.Body.String())
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	bot, err := s.NotificationBot(result.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bot.Channel != "feishu" || bot.Enabled || bot.InteractionEnabled || bot.Target != nil || bot.OwnerID != users["owner"].ID {
		t.Fatalf("unsafe bot: %+v", bot)
	}
	secret, err := s.BotCredential(bot.ID)
	if err != nil || secret != "secret-not-for-browser" {
		t.Fatal("credential not saved")
	}
	w = request("owner", "POST", base+"/reg/consume", "https://loopforge.test", "", body)
	if w.Code != 404 {
		t.Fatal("consumed session reused")
	}
}

func TestFeishuRegistrationQRAndErrorRedaction(t *testing.T) {
	view, err := registrationView(feishuregistration.View{QRURL: "https://accounts.feishu.cn/registration?code=fixture"})
	if err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(view.QRImageDataURI, "data:image/png;base64,"))
	if err != nil || len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("not local PNG")
	}
	if _, err := registrationView(feishuregistration.View{QRURL: strings.Repeat("x", 2049)}); err == nil {
		t.Fatal("unbounded QR accepted")
	}
	for _, raw := range []string{"https://evil.test/qr", "http://accounts.feishu.cn/qr", "https://open.feishu.cn/not-cli", "https://user@accounts.feishu.cn/qr"} {
		if _, err := registrationView(feishuregistration.View{QRURL: raw}); err == nil {
			t.Fatalf("invalid QR accepted: %s", raw)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	registrationError(c, errors.New("private upstream credential"))
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("internal error leaked")
	}
}
