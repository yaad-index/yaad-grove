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
	assert.Nil(t, b.Select("chat", "tldr", true, 5))
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

// The follow-up gate: a reply to the bot, a meta request, or a referential lead
// counts; a plain standalone question does not.
func TestIsFollowUp(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		replyToBot bool
		want       bool
	}{
		// Existing signals.
		{"reply-to-bot always counts", "what is the capital?", true, true},
		{"meta request", "tldr", false, true},
		{"meta prefix", "more please", false, true},
		{"meta with punctuation", "why?", false, true},
		{"referential lead", "what about the second one", false, true},
		{"pronoun lead", "it doesn't work", false, true},

		// Short-message heuristic — language-agnostic (#84). A brief ack in any
		// language is a follow-up even when it isn't a platform reply.
		{"persian yes", "بله", false, true},
		{"persian yeah", "آره", false, true},
		{"persian why", "چرا", false, true},
		{"persian yes with punctuation", "بله؟", false, true},
		{"persian two-word ack", "بله لطفا", false, true},
		{"english one-word ack", "yes", false, true},
		{"english two-word ack", "go ahead", false, true},

		// Guards: a longer standalone question — English or not — is NOT a follow-up,
		// so a fresh question still injects no history.
		{"standalone english question", "how do I install the widget?", false, false},
		{"standalone persian question", "قوانین این بازی چیست", false, false},
		{"empty", "", false, false},
		{"whitespace only", "   ", false, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, memory.IsFollowUp(tc.query, tc.replyToBot), tc.name)
	}
}

// The end-to-end gate: a short non-English ack pulls recent history from the
// buffer, where before it would have answered in isolation (#84).
func TestSelectShortNonEnglishFollowUpInjectsHistory(t *testing.T) {
	b := memory.New(10)
	b.Append("chat", turn("u1", "Al", "the widget calibrates with the blue dial"))
	b.Append("chat", memory.Turn{Bot: true, Text: "turn the blue dial clockwise", Time: time.Now(), MessageID: "b1"})

	// "بله" ("yes") as a non-reply short ack — the reported symptom (#84).
	got := b.Select("chat", "بله", false, 5)
	assert.NotEmpty(t, got, "a short non-English ack injects recent history instead of answering in isolation")

	// A standalone non-English question still gates to nothing.
	assert.Nil(t, b.Select("chat", "قوانین این بازی چیست", false, 5),
		"a fresh non-English question pulls no history")
}

// Select gates on follow-up: a standalone question injects nothing even with a
// full buffer.
func TestSelectGatesStandalone(t *testing.T) {
	b := memory.New(10)
	b.Append("chat", turn("u1", "Al", "the widget installs via script"))
	assert.Nil(t, b.Select("chat", "how do I install?", false, 5),
		"a standalone question pulls no history")
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

	got := b.Select("chat", "tell me more about widget calibration", true, 5)
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
	got := b.Select("chat", "widget", true, 3)
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
