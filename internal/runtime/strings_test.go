package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every key the runtime looks up resolves to non-empty English via the embedded en
// pack — so a missing key in en.yaml (a typo, or a new runtime key without its
// string) is caught here, not at runtime as an empty user message (ADR 0018 / #25).
func TestStringsEnComplete(t *testing.T) {
	var nilCatalog Strings
	for _, key := range stringKeys {
		assert.NotEmpty(t, nilCatalog.Get(key), "en.yaml is missing the %q string", key)
	}
}

// A configured catalog overrides per key; a key it omits falls back to en, and a
// nil catalog is all-en. So a non-base pack (en-completed by the pack merge) never
// returns an empty message.
func TestStringsCatalogOverride(t *testing.T) {
	cat := Strings{StrConsentGranted: "پیوستید"}
	assert.Equal(t, "پیوستید", cat.Get(StrConsentGranted), "the catalog value wins")
	assert.Equal(t, baseStrings[StrRefuse], cat.Get(StrRefuse), "an omitted key falls back to en")

	var nilCatalog Strings
	assert.Equal(t, baseStrings[StrConsentGranted], nilCatalog.Get(StrConsentGranted), "a nil catalog is all-en")
}
