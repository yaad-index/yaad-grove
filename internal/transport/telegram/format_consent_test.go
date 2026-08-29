package telegram

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/langpacks"
)

// Telegram linkifies anything shaped like /command, so in a group the consent
// copy's command names render as tappable links — a member fumbled one and sent
// /consent into the group (yaad-grove #158). The fix is catalog copy: every
// command token is wrapped in a code span so the renderer emits <code>…</code>,
// which Telegram shows monospace and does not linkify. This guards the property
// across ALL packs and every catalog string — the reported strings were only two
// of a family — and it scans the langpacks dir so a newly added pack is covered
// without editing this test.

// codeSpan matches a rendered <code>…</code> fragment; slashCmd matches a
// Telegram-linkifiable /command token (slash + letter + word chars).
var (
	codeSpan = regexp.MustCompile(`(?s)<code>.*?</code>`)
	slashCmd = regexp.MustCompile(`/[a-zA-Z][a-zA-Z0-9_]*`)
)

func TestLangpackCommandsAreCodeFormatted(t *testing.T) {
	packs, err := filepath.Glob("../../../langpacks/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, packs, "no language packs found")

	for _, path := range packs {
		code := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(code, func(t *testing.T) {
			pack, err := langpacks.Load(code, "")
			require.NoError(t, err)
			for key, val := range pack.Strings {
				html := toTelegramHTML(val)
				// A bare /command only linkifies outside a code span. Drop the
				// code spans, then any surviving /command is the #158 bug.
				bare := codeSpan.ReplaceAllString(html, "")
				assert.NotRegexp(t, slashCmd, bare,
					"%s/%s renders a linkifiable command outside <code>: %q", code, key, html)
			}
		})
	}
}
