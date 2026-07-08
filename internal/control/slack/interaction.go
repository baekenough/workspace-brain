package slack

import (
	"context"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Slack interactivity action identifiers this adapter understands. Block-kit
// interactive elements (buttons, etc.) set action_id to one of these values
// to trigger the corresponding handling in handleAction.
const (
	// actionShareResponse re-posts a previously private (ephemeral) answer
	// publicly to the channel. The triggering button's value carries the
	// exact text to share, so no state needs to be recalled from the
	// original slash command request.
	actionShareResponse = "share_response"
	// actionConfirmCommand re-dispatches a pending command (typically a
	// destructive admin command such as `admin off <tenant>`) after the user
	// confirms via a Slack confirmation button. The button's value carries
	// the original command text.
	actionConfirmCommand = "confirm_command"
)

// interactionPayload is the subset of the Slack block_actions interactivity
// payload (https://api.slack.com/reference/interaction-payloads) this
// adapter understands. Slack delivers it form-encoded under a single
// "payload" field as a JSON string.
type interactionPayload struct {
	Type      string `json:"type"`
	TriggerID string `json:"trigger_id"`
	User      struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Actions []interactionAction `json:"actions"`
}

// interactionAction is one entry of payload.actions.
type interactionAction struct {
	ActionID string `json:"action_id"`
	BlockID  string `json:"block_id"`
	Value    string `json:"value"`
}

// supported reports whether p is a block_actions payload carrying at least
// one action this adapter can handle. Other interaction types (e.g.
// view_submission, shortcut) are acknowledged generically without further
// processing.
func (p interactionPayload) supported() bool {
	return p.Type == "block_actions" && len(p.Actions) > 0
}

// handleAction executes the first action in payload and returns the Slack
// response to acknowledge it with. Slack only expects one immediate
// acknowledgement per interaction request, so only the first action (the
// common case for a single button click) is processed.
func (h *Handler) handleAction(ctx context.Context, payload interactionPayload) (slackResponse, error) {
	action := payload.Actions[0]
	switch action.ActionID {
	case actionShareResponse:
		return slackResponse{ResponseType: "in_channel", Text: action.Value}, nil
	case actionConfirmCommand:
		if h.Gateway == nil {
			return slackResponse{}, brainapi.E(brainapi.KindInvalid, "slack_interaction", "Slack 어댑터 설정이 불완전합니다.", nil)
		}
		return h.dispatch(ctx, payload.Channel.ID, payload.User.ID, action.Value)
	default:
		return ephemeral("지원하지 않는 action입니다: " + action.ActionID), nil
	}
}
