package transport_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

func TestActionsAsText(t *testing.T) {
	assert.Empty(t, transport.ActionsAsText(nil), "no actions -> empty string")

	got := transport.ActionsAsText([]core.Action{
		{Label: "Approve"},
		{Label: "Reject"},
	})
	assert.Equal(t, "\n1. Approve\n2. Reject", got)
}
