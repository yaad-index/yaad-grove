package langpacks_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/langpacks"
)

// The base pack (en) loads from the embedded set with an empty prompt — so a bot
// on the default language adds no language guidance.
func TestLoadBase(t *testing.T) {
	p, err := langpacks.Load("en", "")
	require.NoError(t, err)
	assert.Equal(t, "en", p.Code)
	assert.Equal(t, "English", p.Name, "the base keeps its own Name (not the code)")
	assert.Empty(t, p.Prompt, "the base language adds no prompt guidance")
}

// A second embedded pack (fa) overlays the base: it sets its own prompt and keeps
// en's values for anything it omits.
func TestLoadEmbeddedOverlay(t *testing.T) {
	p, err := langpacks.Load("fa", "")
	require.NoError(t, err)
	assert.Equal(t, "fa", p.Code)
	assert.Contains(t, p.Prompt, "Persian", "fa sets its own prompt guidance")
}

// A selected language with no embedded and no external pack is an error (an
// explicit choice that can't be honored).
func TestLoadUnknownLanguageErrors(t *testing.T) {
	_, err := langpacks.Load("zz", "")
	assert.Error(t, err)
}

// An external --langpacks-dir pack overlays the embedded layers per key: it can
// override a single key of an embedded pack, and unset keys still fall back to en.
func TestLoadExternalOverlayPerKey(t *testing.T) {
	dir := t.TempDir()
	// Override only fa's prompt via an external file; omit strings/name entirely.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fa.yaml"),
		[]byte("code: fa\nprompt: \"custom operator guidance\"\n"), 0o600))

	p, err := langpacks.Load("fa", dir)
	require.NoError(t, err)
	assert.Equal(t, "custom operator guidance", p.Prompt, "external file overrides the embedded prompt")
	assert.Equal(t, "فارسی", p.Name, "an omitted key still inherits the embedded fa value")
}

// An external pack for a brand-new language (not embedded) still inherits missing
// keys from en, and needs only what it sets.
func TestLoadExternalNewLanguage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "de.yaml"),
		[]byte("code: de\nname: Deutsch\nprompt: \"Antworte auf Deutsch.\"\n"), 0o600))

	p, err := langpacks.Load("de", dir)
	require.NoError(t, err)
	assert.Equal(t, "de", p.Code)
	assert.Equal(t, "Deutsch", p.Name)
	assert.Contains(t, p.Prompt, "Deutsch")
	assert.NotNil(t, p.Strings, "strings falls back to the en base (empty map), never nil-unusable")
}

// A malformed external pack is an error, not a silent skip.
func TestLoadMalformedErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fa.yaml"), []byte("code: fa\nprompt: [unterminated\n"), 0o600))
	_, err := langpacks.Load("fa", dir)
	assert.Error(t, err)
}
