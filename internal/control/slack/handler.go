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
	"sync"
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

	dedup     *dedupCache
	dedupOnce sync.Once
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
	if h.dedupCache().seenRecently(dedupKeyFromBody(values.Get("trigger_id"), body)) {
		writeJSON(w, http.StatusOK, ephemeral(duplicateMessage(r)))
		return
	}
	response, err := h.dispatch(r.Context(), values.Get("channel_id"), values.Get("user_id"), values.Get("text"))
	if err != nil {
		h.writeError(w, http.StatusOK, safeMessage(err))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// ServeInteraction verifies Slack interactivity requests, parses the
// block_actions payload, and dispatches the triggering action. Interaction
// types this adapter does not recognize (or requests carrying no payload at
// all) are acknowledged generically for forward compatibility.
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
	values, err := url.ParseQuery(string(body))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "interaction payload form 파싱 실패")
		return
	}
	raw := values.Get("payload")
	if raw == "" {
		writeJSON(w, http.StatusOK, ephemeral("상호작용 요청이 접수되었습니다."))
		return
	}
	var payload interactionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		h.writeError(w, http.StatusBadRequest, "interaction payload 파싱 실패")
		return
	}
	if !payload.supported() {
		writeJSON(w, http.StatusOK, ephemeral("상호작용 요청이 접수되었습니다."))
		return
	}
	if h.dedupCache().seenRecently(dedupKeyFromBody(payload.TriggerID, body)) {
		writeJSON(w, http.StatusOK, ephemeral(duplicateMessage(r)))
		return
	}
	response, err := h.handleAction(r.Context(), payload)
	if err != nil {
		writeJSON(w, http.StatusOK, ephemeral(safeMessage(err)))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// dedupCache returns h's lazily-initialized retry/duplicate guard.
func (h *Handler) dedupCache() *dedupCache {
	h.dedupOnce.Do(func() {
		h.dedup = newDedupCache(dedupTTL)
	})
	return h.dedup
}

// dedupKeyFromBody derives a stable identity for a Slack delivery. Slack
// assigns a fresh trigger_id to every distinct user interaction (slash
// command invocation or block action click), but resends the identical
// trigger_id when at-least-once retrying the same delivery, so it is a
// precise dedup key. When trigger_id is unavailable, the raw body is hashed
// as a fallback so identical retried payloads are still recognized.
func dedupKeyFromBody(triggerID string, body []byte) string {
	if triggerID != "" {
		return "trigger:" + triggerID
	}
	sum := sha256.Sum256(body)
	return "body:" + hex.EncodeToString(sum[:])
}

// isSlackRetry reports whether Slack marked r as a retried delivery via the
// X-Slack-Retry-Num header (https://api.slack.com/apis/connections/events-api#retries).
func isSlackRetry(r *http.Request) bool {
	return r.Header.Get("X-Slack-Retry-Num") != ""
}

// duplicateMessage renders the user-facing ack for a request the dedup
// cache recognized as already processed, mentioning Slack's retry mechanism
// when the request was explicitly flagged as a retry.
func duplicateMessage(r *http.Request) string {
	if isSlackRetry(r) {
		return "Slack 재시도 요청이 감지되어 중복 처리를 건너뛰었습니다."
	}
	return "이미 처리된 요청입니다."
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

func (h *Handler) dispatch(ctx context.Context, channelID, userID, text string) (slackResponse, error) {
	response, err := frontend.NewDispatcher(h.Gateway).Handle(ctx, frontend.Request{
		BindingKey: brainapi.BindingKey("slack:channel:" + channelID),
		Principal:  h.principal(userID),
		Text:       text,
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
	text := response.Text + frontend.FormatCitations(response.Sources)
	if response.Visibility == frontend.VisibilityPrivate {
		return ephemeral(text)
	}
	return slackResponse{ResponseType: "in_channel", Text: text}
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
