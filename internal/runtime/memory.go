package runtime

import (
	"strings"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/memory"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// selectHistory returns the recent-conversation turns to inject for a message,
// mapped to the engine's core.HistoryTurn (ADR 0014). It is nil when memory is
// disabled or the message is standalone. Call it BEFORE recording the current
// turn: the buffer then holds only prior turns, so the current message never
// appears in its own injected context. A nil buffer is a safe no-op.
func selectHistory(buf *memory.Buffer, in transport.Inbound, injectN int) []core.HistoryTurn {
	turns := buf.Select(in.ReplyTo, in.Text, in.ReplyToBot, injectN)
	if len(turns) == 0 {
		return nil
	}
	out := make([]core.HistoryTurn, len(turns))
	for i, t := range turns {
		out[i] = core.HistoryTurn{
			Speaker:   t.Speaker,
			Bot:       t.Bot,
			Text:      t.Text,
			Time:      t.Time,
			MessageID: t.MessageID,
			ReplyTo:   t.ReplyTo,
		}
	}
	return out
}

// rememberUser records the sender's turn in the conversation buffer — a consented
// group turn or an admin DM turn (ADR 0014). A nil/disabled buffer is a no-op.
func rememberUser(buf *memory.Buffer, in transport.Inbound) {
	buf.Append(in.ReplyTo, memory.Turn{
		SpeakerID: in.User.ID,
		Speaker:   in.User.Display,
		Text:      in.Text,
		Time:      time.Now(),
		MessageID: in.MessageID,
		ReplyTo:   in.ReplyToMessageID,
	})
}

// rememberBot records the bot's own answer so a later "tldr" can summarize it
// (ADR 0014). The sent-message id isn't known at handler time, so the turn
// carries no MessageID — a later reply to the bot is recognized by the
// reply-to-bot signal, not id matching. Empty text and a nil/disabled buffer are
// no-ops.
func rememberBot(buf *memory.Buffer, chatID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	buf.Append(chatID, memory.Turn{Bot: true, Text: text, Time: time.Now()})
}
