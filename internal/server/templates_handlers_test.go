package server

import (
	"io/fs"
	"regexp"
	"testing"
)

// inlineHandlerWithData matches an inline event handler attribute whose value
// interpolates a JavaScript template expression. The browser decodes HTML
// entities in the attribute before running the handler, so escapeHTML cannot
// keep a quote in the interpolated value from ending the handler's string.
var inlineHandlerWithData = regexp.MustCompile(`\son[a-z]+\s*=\s*"[^"]*\$\{`)

func TestTemplatesInterpolateNoDataIntoInlineHandlers(t *testing.T) {
	err := fs.WalkDir(templatesFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		source, err := fs.ReadFile(templatesFS, path)
		if err != nil {
			return err
		}
		for _, match := range inlineHandlerWithData.FindAll(source, -1) {
			t.Errorf("%s: inline handler interpolates data: %s", path, match)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
