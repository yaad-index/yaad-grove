package store

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// persianFold maps Arabic-script letter variants to their Persian canonical form.
// NFKC does NOT unify these, so a name typed on an Arabic keyboard (yeh U+064A,
// kaf U+0643) and the same name typed on a Persian one (yeh U+06CC, keheh U+06A9)
// would otherwise produce different keys and the alias lookup would miss — the
// exact cross-script case the alias feature exists to solve (ADR 0019).
var persianFold = map[rune]rune{
	'ي': 'ی', // ARABIC YEH ي → FARSI YEH ی
	'ى': 'ی', // ARABIC ALEF MAKSURA ى → FARSI YEH ی
	'ك': 'ک', // ARABIC KAF ك → KEHEH ک
	'ة': 'ه', // TEH MARBUTA ة → HEH ه
	'ۀ': 'ه', // HEH WITH YEH ABOVE ۀ → HEH ه
	'ہ': 'ه', // HEH GOAL ہ → HEH ه
}

// removed reports whether r is a joiner or diacritic stripped entirely before
// key comparison: the zero-width joiners (so a name written with or without the
// ZWNJ half-space matches — وینگ‌اسپن vs وینگاسپن), the harakat/tashkil vowel
// marks, the Quranic superscript alef, and the tatweel/kashida elongation.
func removed(r rune) bool {
	switch {
	case r == '‌', r == '‍': // ZWNJ, ZWJ
		return true
	case r >= 'ً' && r <= 'ْ': // harakat / tashkil
		return true
	case r == 'ٰ': // superscript alef
		return true
	case r == 'ـ': // tatweel / kashida
		return true
	}
	return false
}

// foldDigit maps an Arabic-Indic (U+0660–U+0669) or Persian (U+06F0–U+06F9) digit
// to its ASCII equivalent, else returns the rune unchanged — so "۴۷" and "47"
// share a key.
func foldDigit(r rune) rune {
	switch {
	case r >= '٠' && r <= '٩':
		return '0' + (r - '٠')
	case r >= '۰' && r <= '۹':
		return '0' + (r - '۰')
	}
	return r
}

// normalizeKey produces the deterministic lookup key for a dimension value or an
// alias surface form (ADR 0019). It is script-aware so cross-script and
// cross-keyboard spellings of one entity collapse to a single key — the whole
// reason the alias feature exists (a Persian transliteration must reach its Latin
// canonical). The pipeline, in order:
//
//  1. NFKC — ligatures / compatibility forms.
//  2. Persian letter fold — Arabic vs Persian yeh/kaf, teh-marbuta and heh variants.
//  3. Strip joiners + diacritics — ZWNJ/ZWJ, harakat/tashkil, superscript alef, tatweel.
//  4. Fold Arabic-Indic / Persian digits to ASCII.
//  5. Lowercase → trim → collapse internal separator runs (whitespace, hyphens,
//     punctuation, symbols) to a single space.
//
// It is deterministic key normalization, NOT fuzzy scoring. Both Store.Index (on
// the way in) and Enumerate (on the query value) call it, so a spelling that
// differs only in these dimensions can never drop a document. It is harmless to
// Latin text (a non-Persian instance is unaffected beyond case/whitespace folding).
func normalizeKey(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if removed(r) {
			continue
		}
		if f, ok := persianFold[r]; ok {
			r = f
		}
		r = foldDigit(r)
		b.WriteRune(unicode.ToLower(r))
	}
	return collapseSeparators(strings.TrimSpace(b.String()))
}

// collapseSeparators replaces every run of separators with a single space, so
// "acme-rail", "Acme  Rail", "Acme Rail", and "Route/Network Building" vs
// "route network building" all share one key. (Leading/trailing separators are
// already trimmed by the caller for whitespace; a stray leading/trailing
// separator is dropped here.)
func collapseSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSep := false
	for _, r := range s {
		if separator(r) {
			inSep = true
			continue
		}
		if inSep && b.Len() > 0 {
			b.WriteByte(' ')
		}
		inSep = false
		b.WriteRune(r)
	}
	return b.String()
}

// separator reports whether r delimits tokens in a normalized key: whitespace,
// or any punctuation/symbol — so "/", "(", ")", ",", ".", "&", "|", "_" and the
// like all fold to a single space (ADR 0020). Folding punctuation to a separator
// is symmetric (Index and Enumerate share this normalizer) and can only MERGE
// keys that were previously distinct, never split one — so a value like
// "Route/Network Building" becomes reachable by "route network building" without
// any spelling drift dropping a document. Hyphen-minus is itself punctuation and
// thus covered; it stays listed for clarity alongside the pre-ADR-0020 behavior.
func separator(r rune) bool {
	return unicode.IsSpace(r) || r == '-' || unicode.IsPunct(r) || unicode.IsSymbol(r)
}
