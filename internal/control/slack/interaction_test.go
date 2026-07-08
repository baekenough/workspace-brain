package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestInteractionPayloadSupported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		p    interactionPayload
		want bool
	}{
		{
			name: "block_actions with an action is supported",
			p:    interactionPayload{Type: "block_actions", Actions: []interactionAction{{ActionID: actionShareResponse}}},
			want: true,
		},
		{
			name: "block_actions with no actions is unsupported",
			p:    interactionPayload{Type: "block_actions"},
			want: false,
		},
		{
			name: "non block_actions type is unsupported",
			p:    interactionPayload{Type: "view_submission", Actions: []interactionAction{{ActionID: actionShareResponse}}},
			want: false,
		},
		{
			name: "zero value payload is unsupported",
			p:    interactionPayload{},
			want: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.p.supported(); got != tt.want {
				t.Fatalf("supported() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandlerHandleActionShareResponse(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	resp, err := h.handleAction(context.Background(), interactionPayload{
		Type:    "block_actions",
		Actions: []interactionAction{{ActionID: actionShareResponse, Value: "이전 비공개 답변"}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.ResponseType != "in_channel" || resp.Text != "이전 비공개 답변" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestHandlerHandleActionConfirmCommandMissingGateway(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	_, err := h.handleAction(context.Background(), interactionPayload{
		Type:    "block_actions",
		Actions: []interactionAction{{ActionID: actionConfirmCommand, Value: "admin off tenant-1"}},
	})
	if err == nil {
		t.Fatal("expected error when Gateway is unset")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind = %q, want invalid", brainapi.KindOf(err))
	}
}

func TestHandlerHandleActionConfirmCommandDispatches(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{}
	h := NewHandler(gw, "secret", map[string]bool{"U1": true})
	resp, err := h.handleAction(context.Background(), interactionPayload{
		Type: "block_actions",
		User: struct {
			ID string `json:"id"`
		}{ID: "U1"},
		Channel: struct {
			ID string `json:"id"`
		}{ID: "C1"},
		Actions: []interactionAction{{ActionID: actionConfirmCommand, Value: "create Demo"}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if gw.created.BindingKey != "slack:channel:C1" || !gw.created.Principal.HasRole("admin") || gw.created.Metadata["name"] != "Demo" {
		t.Fatalf("created command = %+v", gw.created)
	}
	if resp.Text == "" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestHandlerHandleActionConfirmCommandPropagatesGatewayError(t *testing.T) {
	t.Parallel()
	gw := &fakeGateway{err: errors.New("gateway exploded")}
	h := NewHandler(gw, "secret", nil)
	_, err := h.handleAction(context.Background(), interactionPayload{
		Type:    "block_actions",
		Actions: []interactionAction{{ActionID: actionConfirmCommand, Value: "create Demo"}},
	})
	if err == nil || err.Error() != "gateway exploded" {
		t.Fatalf("err = %v", err)
	}
}

func TestHandlerHandleActionUnknownActionID(t *testing.T) {
	t.Parallel()
	h := NewHandler(&fakeGateway{}, "secret", nil)
	resp, err := h.handleAction(context.Background(), interactionPayload{
		Type:    "block_actions",
		Actions: []interactionAction{{ActionID: "mystery_action"}},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.ResponseType != "ephemeral" || resp.Text != "지원하지 않는 action입니다: mystery_action" {
		t.Fatalf("resp = %+v", resp)
	}
}
