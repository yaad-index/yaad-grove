package runtime

import (
	"fmt"

	"github.com/yaad-index/yaad-grove/langpacks"
)

// Language-pack string keys (ADR 0018 / #25): the ids under a pack's `strings:`
// catalog for the bot's user-facing operational messages. en.yaml is the source of
// truth for their English text; other packs override per key.
const (
	StrConsentDisclosureIntro = "consent_disclosure_intro"
	StrConsentTranscriptLine  = "consent_transcript_line"
	StrConsentDisclosureTap   = "consent_disclosure_tap"
	StrConsentGranted         = "consent_granted"
	StrConsentAlready         = "consent_already"
	StrConsentRemoved         = "consent_removed"
	StrConsentError           = "consent_error"
	StrConsentOptInLabel      = "consent_opt_in_label"
	StrNudge                  = "nudge"
	StrRefuse                 = "refuse"
	StrRateLimited            = "rate_limited"
	StrAtCapacity             = "at_capacity"
	StrCallbackDone           = "callback_done"
	StrCallbackExpired        = "callback_expired"
	StrCallbackConsumed       = "callback_consumed"
	StrCallbackError          = "callback_error"
	StrCallbackDenied         = "callback_denied"
	StrCallbackUnknownVerb    = "callback_unknown_verb"
	StrCallbackInvalid        = "callback_invalid"
	StrCallbackFailed         = "callback_failed"
)

// stringKeys is every key the runtime looks up — used to validate en completeness.
var stringKeys = []string{
	StrConsentDisclosureIntro, StrConsentTranscriptLine, StrConsentDisclosureTap,
	StrConsentGranted, StrConsentAlready, StrConsentRemoved, StrConsentError, StrConsentOptInLabel,
	StrNudge, StrRefuse, StrRateLimited, StrAtCapacity,
	StrCallbackDone, StrCallbackExpired, StrCallbackConsumed, StrCallbackError,
	StrCallbackDenied, StrCallbackUnknownVerb, StrCallbackInvalid, StrCallbackFailed,
}

// Strings is the resolved per-language message catalog for one instance (ADR
// 0018): the selected pack's `strings`, already en-completed by the pack merge. A
// nil catalog (no pack wired — e.g. a test Policy) falls back to the embedded en
// pack, so the bot always has English text and no key ever resolves empty.
type Strings map[string]string

// Get returns the catalog's text for key, falling back to the embedded en pack
// (baseStrings) when the catalog is nil or omits the key.
func (s Strings) Get(key string) string {
	if v, ok := s[key]; ok && v != "" {
		return v
	}
	return baseStrings[key]
}

// baseStrings is the embedded en pack's `strings`, the ultimate English fallback.
// Loading it from en.yaml (not a Go copy) keeps a single source of truth (ADR
// 0018): the pack file is authoritative, the runtime never re-states the text.
var baseStrings = mustBaseStrings()

func mustBaseStrings() map[string]string {
	p, err := langpacks.Load("en", "")
	if err != nil {
		panic(fmt.Sprintf("runtime: load embedded en language pack: %v", err))
	}
	return p.Strings
}
