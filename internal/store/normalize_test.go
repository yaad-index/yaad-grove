package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// THE PIN (ADR 0019): one entity authored three ways — Arabic-keyboard letters,
// Persian-keyboard letters, and a ZWNJ half-space variant — must resolve to a
// single key. NFKC alone does NOT unify Arabic yeh/kaf with Persian yeh/keheh nor
// strip the ZWNJ, so without the script-aware fold the cross-script alias lookup
// silently misses on the live Persian bot. Built from explicit code points so the
// test data is unambiguous.
func TestNormalizeKeyPersianCrossScript(t *testing.T) {
	// ک ی م ی ا  spelled with Arabic kaf (U+0643) + Arabic yeh (U+064A).
	arabic := "كيميا"
	// same, Persian keheh (U+06A9) + Persian yeh (U+06CC).
	persian := "کیمیا"
	// same Persian spelling with a ZWNJ (U+200C) inserted mid-word.
	zwnj := "کیم\u200cیا"

	kArabic, kPersian, kZwnj := normalizeKey(arabic), normalizeKey(persian), normalizeKey(zwnj)
	assert.NotEmpty(t, kArabic)
	assert.Equal(t, kPersian, kArabic, "Arabic-keyboard spelling folds to the Persian key")
	assert.Equal(t, kPersian, kZwnj, "a ZWNJ half-space variant folds to the same key")
}

// Alef maksura (U+0649) and teh marbuta (U+0629) fold to Persian yeh / heh.
func TestNormalizeKeyLetterFolds(t *testing.T) {
	assert.Equal(t, normalizeKey("ی"), normalizeKey("ى"), "alef maksura → Persian yeh")
	assert.Equal(t, normalizeKey("ه"), normalizeKey("ة"), "teh marbuta → heh")
}

// Harakat / tashkil vowel marks and the tatweel elongation are stripped, so a
// vocalized or kashida-stretched spelling matches the plain one.
func TestNormalizeKeyStripsDiacriticsAndTatweel(t *testing.T) {
	// محمد with harakat (fatha/damma) vs plain.
	vocalized := "مُحَمَمَد"
	plain := "محممد"
	assert.Equal(t, normalizeKey(plain), normalizeKey(vocalized), "harakat are stripped")

	// اکبر with a tatweel (U+0640) between letters vs plain.
	stretched := "اکـبر"
	assert.Equal(t, normalizeKey("اکبر"), normalizeKey(stretched), "tatweel is stripped")
}

// Arabic-Indic and Persian digits fold to ASCII, so "۴۷", "٤٧", and "47" share a key.
func TestNormalizeKeyFoldsDigits(t *testing.T) {
	assert.Equal(t, "47", normalizeKey("۴۷"), "Persian digits → ASCII")
	assert.Equal(t, "47", normalizeKey("٤٧"), "Arabic-Indic digits → ASCII")
	assert.Equal(t, normalizeKey("47"), normalizeKey("۴۷"))
}

// Latin text: case-folded and whitespace/hyphen-collapsed, otherwise untouched —
// a non-Persian instance is unaffected beyond that.
func TestNormalizeKeyLatin(t *testing.T) {
	assert.Equal(t, "acme rail", normalizeKey("Acme Rail"))
	assert.Equal(t, normalizeKey("Acme Rail"), normalizeKey("acme-rail"), "hyphen collapses to space")
	assert.Equal(t, normalizeKey("Acme Rail"), normalizeKey("  ACME   RAIL  "), "runs collapse, ends trim")
	assert.Equal(t, "testville", normalizeKey("Testville"))
	// Space is NOT removed (only collapsed): a spaced name is distinct from a joined one.
	assert.NotEqual(t, normalizeKey("sky bridge"), normalizeKey("skybridge"))
}

// The empty and whitespace-only inputs normalize to the empty key.
func TestNormalizeKeyEmpty(t *testing.T) {
	assert.Empty(t, normalizeKey(""))
	assert.Empty(t, normalizeKey("   "))
	assert.Empty(t, normalizeKey(" - "))
}
