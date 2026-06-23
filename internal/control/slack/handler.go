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
	"strings"
	"time"

	"github.com/sangyi/workspace-brain/internal/control/gateway"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const signatureVersion = "v0"

// Gateway is the subset of gateway.Gateway used by the Slack adapter.
type Gateway interface {
	CreateProject(ctx context.Context, cmd gateway.CreateProjectCommand) (gateway.CreateProjectResult, error)
	Ask(ctx context.Context, cmd gateway.AskCommand) (brainapi.QueryResponse, error)
	Discover(ctx context.Context, cmd gateway.DiscoverCommand) (brainapi.DiscoverResponse, error)
	Ingest(ctx context.Context, cmd gateway.IngestCommand) (gateway.IngestResult, error)
	Status(ctx context.Context, cmd gateway.StatusCommand) (brainapi.JobSnapshot, error)
}

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
	text := strings.TrimSpace(values.Get("text"))
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return slackResponse{}, brainapi.E(brainapi.KindInvalid, "slack_dispatch", "서브커맨드를 입력하세요: create|ingest|ask|discover|status", nil)
	}
	binding := brainapi.BindingKey("slack:channel:" + values.Get("channel_id"))
	principal := h.principal(values.Get("user_id"))
	subcommand := strings.ToLower(fields[0])
	args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	switch subcommand {
	case "create":
		result, err := h.Gateway.CreateProject(ctx, gateway.CreateProjectCommand{BindingKey: binding, Principal: principal, Metadata: map[string]string{"name": args}})
		if err != nil {
			return slackResponse{}, err
		}
		return ephemeral("프로젝트가 생성되었습니다: " + string(result.TenantID)), nil
	case "ask":
		answer, err := h.Gateway.Ask(ctx, gateway.AskCommand{BindingKey: binding, Principal: principal, Question: args})
		if err != nil {
			return slackResponse{}, err
		}
		return ephemeral(answer.Answer), nil
	case "discover":
		result, err := h.Gateway.Discover(ctx, gateway.DiscoverCommand{BindingKey: binding, Principal: principal, Query: args})
		if err != nil {
			return slackResponse{}, err
		}
		return ephemeral("메타데이터 결과: " + intString(len(result.Results))), nil
	case "ingest":
		if args == "" {
			return slackResponse{}, brainapi.E(brainapi.KindInvalid, "slack_ingest", "source URI가 필요합니다.", nil)
		}
		result, err := h.Gateway.Ingest(ctx, gateway.IngestCommand{BindingKey: binding, Principal: principal, Source: brainapi.SourceRef{URI: args, Name: args}})
		if err != nil {
			return slackResponse{}, err
		}
		return ephemeral("수집 작업이 접수되었습니다: " + string(result.JobID)), nil
	case "status":
		if args == "" {
			return slackResponse{}, brainapi.E(brainapi.KindInvalid, "slack_status", "job_id가 필요합니다.", nil)
		}
		status, err := h.Gateway.Status(ctx, gateway.StatusCommand{BindingKey: binding, Principal: principal, JobID: brainapi.JobID(args), Reconcile: true})
		if err != nil {
			return slackResponse{}, err
		}
		return ephemeral("작업 상태: " + string(status.Status)), nil
	default:
		return slackResponse{}, brainapi.E(brainapi.KindInvalid, "slack_dispatch", "알 수 없는 서브커맨드입니다.", nil)
	}
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

func safeMessage(err error) string {
	if brainapi.IsKind(err, brainapi.KindUnauthorized) {
		return "프로젝트에 접근할 수 없습니다."
	}
	return err.Error()
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
