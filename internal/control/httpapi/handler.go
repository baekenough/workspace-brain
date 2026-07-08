// Package httpapi adapts trusted JSON HTTP requests into frontend commands.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sangyi/workspace-brain/internal/control/frontend"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Handler serves a small token-protected command API for non-Slack frontends.
type Handler struct {
	Gateway frontend.Gateway
	Token   string
}

// NewHandler creates a JSON command API handler.
func NewHandler(gw frontend.Gateway, token string) *Handler {
	return &Handler{Gateway: gw, Token: token}
}

// ServeHTTP validates the bearer token, decodes a normalized frontend request, and dispatches it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST만 지원합니다."})
		return
	}
	if h.Gateway == nil || h.Token == "" {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "HTTP API 어댑터 설정이 불완전합니다."})
		return
	}
	if !h.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "인증이 필요합니다."})
		return
	}
	defer r.Body.Close()
	var req commandRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "JSON 요청을 파싱할 수 없습니다."})
		return
	}
	response, err := frontend.NewDispatcher(h.Gateway).Handle(r.Context(), frontend.Request{
		BindingKey: brainapi.BindingKey(req.BindingKey),
		Principal:  req.Principal,
		Text:       req.Text,
	})
	if err != nil {
		writeJSON(w, statusFor(err), errorResponse{Error: frontend.SafeMessage(err)})
		return
	}
	writeJSON(w, http.StatusOK, commandResponse{
		Visibility:         response.Visibility,
		Text:               response.Text,
		Sources:            response.Sources,
		GroundingAvailable: response.GroundingAvailable,
	})
}

func (h *Handler) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.Token)) == 1
}

type commandRequest struct {
	BindingKey string             `json:"binding_key"`
	Principal  brainapi.Principal `json:"principal"`
	Text       string             `json:"text"`
}

type commandResponse struct {
	Visibility frontend.Visibility `json:"visibility"`
	Text       string              `json:"text"`
	// Sources lists grounded citations for "ask" answers. Omitted (empty)
	// for non-ask commands and for ungrounded ("모른다") answers.
	Sources []brainapi.Source `json:"sources,omitempty"`
	// GroundingAvailable reports whether the underlying query found grounded
	// evidence backing Sources.
	GroundingAvailable bool `json:"grounding_available,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func statusFor(err error) int {
	switch brainapi.KindOf(err) {
	case brainapi.KindInvalid:
		return http.StatusBadRequest
	case brainapi.KindUnauthorized:
		return http.StatusForbidden
	case brainapi.KindNotFound:
		return http.StatusNotFound
	case brainapi.KindConflict, brainapi.KindAlreadyExists:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
