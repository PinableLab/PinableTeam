package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/feishuregistration"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"rsc.io/qr"
)

type feishuRegistrationStore interface {
	Begin(context.Context, int64, string, string) (feishuregistration.View, error)
	Poll(context.Context, int64, string, string) (feishuregistration.View, error)
	Cancel(int64, string, string) error
	Consume(int64, string, string, func(feishuregistration.Credentials) error) error
}

type feishuRegistrationHandlers struct {
	*deps
	store feishuRegistrationStore
}

// 每次重新读取网页登录身份和项目写权限；无认证部署也不能匿名注册应用。
func (h *feishuRegistrationHandlers) authorize(c *gin.Context, projectID string) (int64, bool) {
	c.Header("Cache-Control", "no-store")
	if _, bearer := bearerToken(c.Request); bearer {
		c.JSON(401, gin.H{"error": "请使用平台网页登录注册飞书应用"})
		return 0, false
	}
	current, ok := h.interactiveUser(c.Request)
	if !ok || current == nil || current.IsGuest || current.ID <= 0 {
		c.JSON(401, gin.H{"error": "请先登录平台"})
		return 0, false
	}
	if _, ok := h.repo.User(current.ID); !ok {
		c.JSON(401, gin.H{"error": "平台账号不存在"})
		return 0, false
	}
	if !botBindingSameOrigin(c) {
		c.JSON(403, gin.H{"error": "请从平台通知配置页面操作"})
		return 0, false
	}
	project, ok := h.repo.Project(projectID)
	if !ok {
		c.JSON(404, gin.H{"error": "项目不存在"})
		return 0, false
	}
	if h.isPlatformAdmin(c.Request) || project.OwnerID == current.ID {
		return current.ID, true
	}
	for _, member := range project.Members {
		if member.UserID == current.ID && !strings.EqualFold(strings.TrimSpace(member.Role), "viewer") {
			return current.ID, true
		}
	}
	c.JSON(403, gin.H{"error": "你没有该项目的通知管理权限"})
	return 0, false
}

type feishuRegistrationInput struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
}

type feishuRegistrationView struct {
	feishuregistration.View
	QRImageDataURI string `json:"qrImageDataUri"`
}

// 二维码只在服务端由已验证的注册地址离线生成，不向外部图片服务发送地址。
func registrationView(view feishuregistration.View) (feishuRegistrationView, error) {
	result := feishuRegistrationView{View: view}
	if view.QRURL == "" {
		return result, nil
	}
	u, parseErr := url.Parse(view.QRURL)
	if parseErr != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" ||
		(u.Host != "accounts.feishu.cn" && !(u.Host == "open.feishu.cn" && u.Path == "/page/cli")) || len(view.QRURL) > 2048 {
		return result, feishuregistration.ErrInvalid
	}
	code, err := qr.Encode(view.QRURL, qr.M)
	if err != nil {
		return result, feishuregistration.ErrInvalid
	}
	result.QRImageDataURI = "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
	return result, nil
}

func (h *feishuRegistrationHandlers) handle(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input feishuRegistrationInput
	if c.Request.Method == http.MethodDelete {
		input.ProjectID = c.Query("projectId")
	} else if !decode(c, &input) {
		return
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.Name = strings.TrimSpace(input.Name)
	if input.ProjectID == "" || len(input.ProjectID) > 191 {
		c.JSON(400, gin.H{"error": "请选择有效项目"})
		return
	}
	owner, ok := h.authorize(c, input.ProjectID)
	if !ok {
		return
	}
	id := c.Param("id")
	if id == "" {
		if input.Name == "" || len([]rune(input.Name)) > 48 {
			c.JSON(400, gin.H{"error": "请输入不超过 48 字的机器人名称"})
			return
		}
		view, err := h.store.Begin(c.Request.Context(), owner, input.ProjectID, input.Name)
		if err != nil {
			registrationError(c, err)
			return
		}
		result, err := registrationView(view)
		if err != nil {
			_ = h.store.Cancel(owner, input.ProjectID, view.ID)
			registrationError(c, err)
			return
		}
		c.JSON(200, result)
		return
	}
	if c.Request.Method == http.MethodDelete {
		if err := h.store.Cancel(owner, input.ProjectID, id); err != nil {
			registrationError(c, err)
			return
		}
		c.Status(204)
		return
	}
	if strings.HasSuffix(c.FullPath(), "/poll") {
		view, err := h.store.Poll(c.Request.Context(), owner, input.ProjectID, id)
		if err != nil {
			registrationError(c, err)
			return
		}
		result, err := registrationView(view)
		if err != nil {
			registrationError(c, err)
			return
		}
		c.JSON(200, result)
		return
	}
	botID := "bot-" + randomID()
	err := h.store.Consume(owner, input.ProjectID, id, func(credentials feishuregistration.Credentials) error {
		if strings.TrimSpace(credentials.BotName) == "" || len([]rune(credentials.BotName)) > 80 {
			return feishuregistration.ErrInvalid
		}
		bot := domain.NotificationBot{ID: botID, Channel: "feishu", ProjectID: input.ProjectID, OwnerID: owner,
			Name: credentials.BotName, BotID: credentials.AppID, Model: domain.DefaultNotificationReportModel,
			Enabled: false, InteractionEnabled: false, SecretConfigured: true, Health: "unchecked"}
		return h.repo.SaveNotificationBot(bot, credentials.AppSecret)
	})
	if err != nil {
		registrationError(c, err)
		return
	}
	c.JSON(200, gin.H{"id": botID})
}

func registrationError(c *gin.Context, err error) {
	status := http.StatusBadGateway
	message := "飞书注册服务暂不可用，请重试"
	switch {
	case errors.Is(err, feishuregistration.ErrNotFound):
		status = http.StatusNotFound
		message = "飞书注册会话不存在或已过期"
	case errors.Is(err, feishuregistration.ErrNotReady):
		status = http.StatusConflict
		message = "飞书注册尚未完成"
	case errors.Is(err, feishuregistration.ErrLimit):
		status = http.StatusTooManyRequests
		message = "注册请求过多，请稍后重试"
	case errors.Is(err, feishuregistration.ErrInvalid):
		status = http.StatusBadRequest
		message = "飞书注册响应无效或不受支持"
	}
	// 不回传上游响应、存储错误或凭据。
	c.JSON(status, gin.H{"error": message})
}
