// Package slack adapts Slack slash-command requests into surface-neutral gateway calls.
package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sangyi/workspace-brain/internal/control/frontend"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const signatureVersion = "v0"

// Gateway is the surface-neutral command gateway consumed by frontend adapters.
type Gateway = frontend.Gateway

// Handler serves Slack slash commands.
type Handler struct {
	Gateway       Gateway
	SigningSecret string
	CommandName   string
	AdminUsers    map[string]bool
	Now           func() time.Time
}

// NewHandler creates a Slack handler with safe defaults.
func NewHandler(gw Gateway, signingSecret string, adminUsers map[string]bool) *Handler {
	return &Handler{Gateway: gw, SigningSecret: signingSecret, CommandName: "brain", AdminUsers: cloneBoolMap(adminUsers), Now: time.Now}
}

// ServeHTTP verifies Slack signature, parses the slash command, and returns an immediate response.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "POST만 지원합니다.")
		return
	}
	if h.Gateway == nil || h.SigningSecret == "" {
		h.writeError(w, http.StatusInternalServerError, "Slack 어댑터 설정이 불완전합니다.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "요청 본문을 읽을 수 없습니다.")
		return
	}
	if err := h.verify(r, body); err != nil {
		h.writeError(w, http.StatusUnauthorized, "Slack 서명 검증 실패")
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "slash command form 파싱 실패")
		return
	}
	if err := h.validateCommand(values.Get("command")); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	response, err := h.dispatch(r.Context(), values)
	if err != nil {
		h.writeError(w, http.StatusOK, safeMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// ServeInteraction verifies and acknowledges Slack interactivity requests.
func (h *Handler) ServeInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "POST만 지원합니다.")
		return
	}
	if h.SigningSecret == "" {
		h.writeError(w, http.StatusInternalServerError, "Slack 어댑터 설정이 불완전합니다.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "요청 본문을 읽을 수 없습니다.")
		return
	}
	if err := h.verify(r, body); err != nil {
		h.writeError(w, http.StatusUnauthorized, "Slack 서명 검증 실패")
		return
	}
	writeJSON(w, http.StatusOK, ephemeral("상호작용 요청이 접수되었습니다."))
}

func (h *Handler) verify(r *http.Request, body []byte) error {
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")
	signature := r.Header.Get("X-Slack-Signature")
	if timestamp == "" || signature == "" {
		return brainapi.E(brainapi.KindUnauthorized, "slack_verify", "missing signature headers", nil)
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	parsed, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return brainapi.E(brainapi.KindUnauthorized, "slack_verify", "invalid timestamp", err)
	}
	if delta := now().Sub(time.Unix(parsed, 0)); delta > 5*time.Minute || delta < -5*time.Minute {
		return brainapi.E(brainapi.KindUnauthorized, "slack_verify", "stale timestamp", nil)
	}
	mac := hmac.New(sha256.New, []byte(h.SigningSecret))
	_, _ = mac.Write([]byte(signatureVersion + ":" + timestamp + ":"))
	_, _ = mac.Write(body)
	want := signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return brainapi.E(brainapi.KindUnauthorized, "slack_verify", "signature mismatch", nil)
	}
	return nil
}

func (h *Handler) validateCommand(command string) error {
	name := h.CommandName
	if name == "" {
		name = "brain"
	}
	if command != "/"+name {
		return brainapi.E(brainapi.KindInvalid, "slack_command", "등록된 command와 요청 command가 다릅니다.", nil)
	}
	return nil
}

func (h *Handler) dispatch(ctx context.Context, values url.Values) (slackResponse, error) {
	response, err := frontend.NewDispatcher(h.Gateway).Handle(ctx, frontend.Request{
		BindingKey: brainapi.BindingKey("slack:channel:" + values.Get("channel_id")),
		Principal:  h.principal(values.Get("user_id")),
		Text:       values.Get("text"),
	})
	if err != nil {
		return slackResponse{}, err
	}
	return render(response), nil
}

func (h *Handler) principal(userID string) brainapi.Principal {
	roles := []string{"member"}
	if h.AdminUsers[userID] {
		roles = append(roles, "admin")
	}
	return brainapi.Principal{Source: "slack", ID: userID, Roles: roles}
}

type slackResponse struct {
	ResponseType string `json:"response_type"`
	Text         string `json:"text"`
}

func ephemeral(text string) slackResponse {
	return slackResponse{ResponseType: "ephemeral", Text: text}
}

func render(response frontend.Response) slackResponse {
	return ephemeral(response.Text)
}

func safeMessage(err error) string {
	return frontend.SafeMessage(err)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ephemeral(message))
}

func writeJSON(w http.ResponseWriter, status int, response slackResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
