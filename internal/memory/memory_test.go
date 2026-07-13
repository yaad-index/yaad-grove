package memory_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/memory"
)

func turn(id, speaker, text string) memory.Turn {
	return memory.Turn{SpeakerID: id, Speaker: speaker, Text: text, Time: time.Now(), MessageID: text}
}

// Append retains up to the bound, evicting oldest; Recent returns the last k in
// chronological order.
func TestAppendRetainAndRecent(t *testing.T) {
	b := memory.New(3)
	for _, s := range []string{"a", "b", "c", "d"} {
		b.Append("chat", turn("u1", "Al", s))
	}
	got := b.Recent("chat", 10)
	require.Len(t, got, 3, "retains only the last 3")
	assert.Equal(t, "b", got[0].Text, "oldest retained")
	assert.Equal(t, "d", got[2].Text, "newest last (chronological)")

	assert.Len(t, b.Recent("chat", 2), 2, "Recent caps at k")
	assert.Empty(t, b.Recent("chat", 0), "k<=0 returns nothing")
	assert.Empty(t, b.Recent("other", 5), "an unseen conversation is empty")
}

// A disabled buffer (retain 0) records nothing and injects nothing — pre-0014
// isolated-answer behavior.
func TestDisabledBufferIsNoOp(t *testing.T) {
	b := memory.New(0)
	assert.False(t, b.Enabled())
	b.Append("chat", turn("u1", "Al", "hi"))
	assert.Empty(t, b.Recent("chat", 5))
	assert.Nil(t, b.Select("chat", "tldr", "u1", true, 5, time.Hour))
}

// Purge removes a speaker's turns and only theirs — the bot's turns and other
// speakers survive; an empty speaker id can't wipe the bot.
func TestPurge(t *testing.T) {
	b := memory.New(10)
	b.Append("chat", turn("u1", "Al", "one"))
	b.Append("chat", memory.Turn{Bot: true, Text: "bot reply", Time: time.Now()})
	b.Append("chat", turn("u2", "Bo", "two"))
	b.Append("chat", turn("u1", "Al", "three"))

	b.Purge("chat", "u1")
	got := b.Recent("chat", 10)
	require.Len(t, got, 2, "both of u1's turns removed")
	assert.Equal(t, "bot reply", got[0].Text, "the bot's turn survives")
	assert.Equal(t, "two", got[1].Text, "another speaker survives")

	b.Purge("chat", "") // must not wipe the bot
	assert.Len(t, b.Recent("chat", 10), 2, "empty speaker id is a no-op")
}

// The follow-up gate is a language-neutral recency signal (ADR 0018): a non-reply
// is a follow-up only when its sender is mid-conversation — a prior non-bot turn
// of theirs in this chat within the window. No keywords, no language heuristics.
func TestSelectRecencyGate(t *testing.T) {
	window := time.Hour

	// A sender with a recent prior turn is mid-conversation → non-reply injects
	// (regardless of the query text — nothing language-specific).
	b := memory.New(10)
	b.Append("chat", turn("u1", "Al", "the widget calibrates with the blue dial"))
	assert.NotEmpty(t, b.Select("chat", "قوانین این بازی چیست", "u1", false, 5, window),
		"a sender with a recent turn is mid-conversation → follow-up, any language")

	// A sender who hasn't spoken → standalone, even though others have.
	assert.Nil(t, b.Select("chat", "how do I install?", "stranger", false, 5, window),
		"a sender with no prior turn is not mid-conversation → standalone")

	// window 0 → replies-only: even a mid-conversation sender's non-reply is standalone.
	assert.Nil(t, b.Select("chat", "and the red one?", "u1", false, 5, 0),
		"window 0 = replies only")

	// A sender whose only turn predates the window → standalone.
	old := memory.New(10)
	old.Append("chat", memory.Turn{SpeakerID: "u1", Speaker: "Al", Text: "long ago", Time: time.Now().Add(-2 * time.Hour)})
	assert.Nil(t, old.Select("chat", "still there?", "u1", false, 5, window),
		"a sender whose last turn predates the window → standalone")

	// Only a bot turn present → the sender is not mid-conversation.
	botOnly := memory.New(10)
	botOnly.Append("chat", memory.Turn{Bot: true, Text: "hello", Time: time.Now()})
	assert.Nil(t, botOnly.Select("chat", "hi", "u1", false, 5, window),
		"a bot turn doesn't make the sender mid-conversation")
}

// A reply always injects, bypassing the recency gate — even when its sender has no
// prior turn of their own (ADR 0018 / #105).
func TestSelectReplyBypassesRecency(t *testing.T) {
	b := memory.New(10)
	b.Append("chat", turn("u1", "Al", "the widget calibrates with the blue dial"))
	assert.NotEmpty(t, b.Select("chat", "what does this mean?", "newcomer", true, 5, time.Hour),
		"a reply injects history even from a sender with no prior turn")
}

// Select returns a recency floor plus relevant turns, capped at injectN and in
// chronological order.
func TestSelectRecencyAndRelevance(t *testing.T) {
	b := memory.New(20)
	// An old, highly relevant turn, then filler, then recent turns.
	b.Append("chat", turn("u1", "Al", "the widget calibration uses the blue dial"))
	for i := 0; i < 6; i++ {
		b.Append("chat", turn("u2", "Bo", "unrelated chatter"))
	}
	b.Append("chat", turn("u1", "Al", "recent one"))
	b.Append("chat", turn("u2", "Bo", "recent two"))
	b.Append("chat", turn("u1", "Al", "recent three"))

	got := b.Select("chat", "tell me more about widget calibration", "u1", true, 5, time.Hour)
	require.NotEmpty(t, got)
	assert.LessOrEqual(t, len(got), 5, "capped at injectN")

	var texts []string
	for _, tn := range got {
		texts = append(texts, tn.Text)
	}
	assert.Contains(t, texts, "the widget calibration uses the blue dial", "the relevant old turn is pulled in")
	assert.Contains(t, texts, "recent three", "the recency floor is included")

	// Chronological order: the old calibration turn precedes the recent ones.
	assert.Less(t, indexOf(texts, "the widget calibration uses the blue dial"), indexOf(texts, "recent three"),
		"injected turns stay in chronological order")
}

// Select honors injectN as a hard cap even when many turns are relevant.
func TestSelectCap(t *testing.T) {
	b := memory.New(20)
	for i := 0; i < 10; i++ {
		b.Append("chat", turn("u1", "Al", "widget widget widget"))
	}
	got := b.Select("chat", "widget", "u1", true, 3, time.Hour)
	assert.Len(t, got, 3, "never more than injectN")
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}
