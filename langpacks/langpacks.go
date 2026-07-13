// Package langpacks loads a per-language behavior pack (ADR 0018): the
// language-specific bits — prompt additions now, user-facing strings later —
// authored as data so a native speaker can add a language without touching Go.
//
// The built-in packs (en, fa, …) are source-controlled here and embedded into the
// binary, so they ship with the engine (present in the distroless image, no
// external files needed). Adding a language is a PR adding a <code>.yaml file to
// this directory. An optional external directory can add or override packs at a
// deployment.
package langpacks

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// baseCode is the reference pack every other pack overlays; it always exists and
// supplies the fallback value for any key another pack omits.
const baseCode = "en"

//go:embed *.yaml
var embedded embed.FS

// Pack is a language pack (ADR 0018). Prompt is language-specific system-prompt
// guidance appended to the prompt; Strings is the user-facing message catalog
// (reserved for a later increment, issue #25).
type Pack struct {
	Code    string            `yaml:"code"`
	Name    string            `yaml:"name"`
	Prompt  string            `yaml:"prompt"`
	Strings map[string]string `yaml:"strings"`
}

// Load resolves the effective pack for code by overlaying, in order: the embedded
// base (en) → the embedded <code> pack (if any) → an external <dir>/<code>.yaml (if
// dir is non-empty and the file exists). Each layer overrides only the keys it
// sets, so a pack states only what differs; any key absent everywhere falls back
// to en (ADR 0018). dir "" uses the embedded packs only.
//
// A selected code with no embedded and no external pack, or a malformed pack, is
// an error — an explicit --language is an explicit choice.
func Load(code, dir string) (*Pack, error) {
	base, err := readEmbedded(baseCode)
	if err != nil {
		return nil, fmt.Errorf("langpacks: base pack %q: %w", baseCode, err)
	}
	pack := *base
	// A non-base code's identity defaults to the code itself until an overlay sets
	// it; the base (en) keeps its own Code/Name (no overlay runs for it).
	if code != baseCode {
		pack.Code, pack.Name = code, code
	}

	found := code == baseCode
	if code != baseCode {
		if layer, err := readEmbedded(code); err == nil {
			overlay(&pack, layer)
			found = true
		} else if !isNotExist(err) {
			return nil, fmt.Errorf("langpacks: embedded pack %q: %w", code, err)
		}
	}
	if dir != "" {
		layer, err := readFile(filepath.Join(dir, code+".yaml"))
		switch {
		case err == nil:
			overlay(&pack, layer)
			found = true
		case !isNotExist(err):
			return nil, fmt.Errorf("langpacks: external pack %q: %w", code, err)
		}
	}
	if !found {
		return nil, fmt.Errorf("langpacks: no pack for language %q (no embedded pack and none in --langpacks-dir)", code)
	}
	return &pack, nil
}

// overlay merges src onto dst: any key src sets overrides dst, any key it omits is
// left as-is (the per-key fallback down to en). Code/Name always take src's value
// (identity of the layer); Prompt overrides only when non-empty; each Strings key
// overrides individually.
func overlay(dst, src *Pack) {
	if src.Code != "" {
		dst.Code = src.Code
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.Prompt != "" {
		dst.Prompt = src.Prompt
	}
	if len(src.Strings) > 0 {
		if dst.Strings == nil {
			dst.Strings = map[string]string{}
		}
		for k, v := range src.Strings {
			dst.Strings[k] = v
		}
	}
}

func readEmbedded(code string) (*Pack, error) {
	data, err := embedded.ReadFile(code + ".yaml")
	if err != nil {
		return nil, err
	}
	return parse(data, code+".yaml")
}

func readFile(path string) (*Pack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(data, path)
}

func parse(data []byte, name string) (*Pack, error) {
	var p Pack
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return &p, nil
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
