// Package memory is the bounded, per-conversation short-term memory buffer
// (ADR 0014): the recent-turn context that lets the bot answer follow-ups
// ("tldr", "what about X") instead of treating every message in isolation.
//
// It is deliberately dumb and transient. Turns live in memory only, keyed by
// conversation; a restart clears them. Consent gates entry upstream (only
// consented human turns and the bot's own answers are appended), and a
// withdrawal purges a speaker's turns immediately — because, unlike the
// append-only growth corpus (ADR 0004), this buffer is read back into prompts.
//
// Remembering and injecting are decoupled: the buffer RETAINS many turns
// (Append/retain bound), but Select INJECTS only a small, relevant slice per
// query — a recency floor plus the most relevant retained turns — and only when
// a cheap heuristic judges the message a follow-up. Turns keep their structure
// (speaker, timestamp, reply-to) so the injected context can be threaded and
// chronological.
package memory

import (
	"sync"
	"time"
)

// Turn is one structured conversation turn (ADR 0014). It carries who spoke,
// when, and which message it replies to, so the injected context preserves
// conversation structure rather than a flat list.
type Turn struct {
	// SpeakerID is the platform user id of a human speaker; empty for the bot's
	// own answer. It is the purge key on consent withdrawal.
	SpeakerID string
	// Speaker is a best-effort human label for the prompt (display name); for the
	// bot it is empty and rendered as the assistant.
	Speaker string
	// Bot marks the bot's own answer (SpeakerID is empty for these).
	Bot bool
	// Text is the message content.
	Text string
	// Time is when the turn happened, for chronological ordering.
	Time time.Time
	// MessageID is the platform message id of this turn — the target a later
	// turn's ReplyTo points at.
	MessageID string
	// ReplyTo is the MessageID this turn replies to, or empty. A ReplyTo whose
	// target is not (or no longer) in the buffer renders as "not shown".
	ReplyTo string
}

// Buffer is a bounded, chat-keyed, in-memory conversation memory (ADR 0014). It
// retains up to `retain` most-recent turns per conversation and is safe for
// concurrent use. A retain of 0 disables it: Append is a no-op and Select
// returns nothing, so the bot answers each message in isolation (pre-0014
// behavior).
type Buffer struct {
	mu     sync.Mutex
	retain int
	convos map[string][]Turn // conversation id -> recent turns, oldest first
}

// New returns a Buffer retaining up to `retain` turns per conversation. retain <=
// 0 disables the buffer.
func New(retain int) *Buffer {
	return &Buffer{retain: retain, convos: map[string][]Turn{}}
}

// Enabled reports whether the buffer retains anything.
func (b *Buffer) Enabled() bool { return b != nil && b.retain > 0 }

// Append records a turn in a conversation, evicting the oldest when over the
// retain bound. It is a no-op when the buffer is disabled.
func (b *Buffer) Append(chatID string, t Turn) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	turns := append(b.convos[chatID], t)
	if len(turns) > b.retain {
		turns = turns[len(turns)-b.retain:]
	}
	b.convos[chatID] = turns
}

// Recent returns the last k turns of a conversation, oldest first. k <= 0 returns
// nothing; k beyond what is retained returns all of it.
func (b *Buffer) Recent(chatID string, k int) []Turn {
	if !b.Enabled() || k <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	turns := b.convos[chatID]
	if k > len(turns) {
		k = len(turns)
	}
	return append([]Turn(nil), turns[len(turns)-k:]...)
}

// Purge removes every turn from speakerID in a conversation — the consent
// withdrawal path (ADR 0012/0014). The bot's own turns (empty SpeakerID) are
// never purged; an empty speakerID is a no-op so a withdrawal can't wipe the bot.
func (b *Buffer) Purge(chatID, speakerID string) {
	if !b.Enabled() || speakerID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeLocked(chatID, speakerID)
}

// PurgeUser removes a speaker's turns from EVERY conversation — the consent
// withdrawal path proper (ADR 0012/0014): consent is per-user, not per-chat, so a
// withdrawal must clear the user everywhere they've spoken, not only the chat the
// /consent remove arrived on. The bot's turns are never purged.
func (b *Buffer) PurgeUser(speakerID string) {
	if !b.Enabled() || speakerID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for chatID := range b.convos {
		b.purgeLocked(chatID, speakerID)
	}
}

// purgeLocked drops speakerID's turns from one conversation; the caller holds mu.
func (b *Buffer) purgeLocked(chatID, speakerID string) {
	turns := b.convos[chatID]
	kept := turns[:0]
	for _, t := range turns {
		if t.SpeakerID != speakerID {
			kept = append(kept, t)
		}
	}
	b.convos[chatID] = append([]Turn(nil), kept...)
}
