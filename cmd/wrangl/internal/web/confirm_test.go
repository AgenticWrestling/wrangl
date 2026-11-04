package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The UI confirms with the inline [data-confirm] control (static/app.js), never the browser dialog.
func TestNoBrowserConfirm(t *testing.T) {
	re := regexp.MustCompile(`(^|[^.\w])(window\.)?confirm\(`)
	for _, root := range []string{"templates", "static"} {
		fs.WalkDir(assets, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.Contains(p, "vendor/") || !(strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".js")) {
				return nil
			}
			b, _ := fs.ReadFile(assets, p)
			for i, l := range strings.Split(string(b), "\n") {
				if re.MatchString(l) && !strings.HasPrefix(strings.TrimSpace(l), "//") {
					t.Errorf("%s:%d uses confirm(); use data-confirm instead", p, i+1)
				}
			}
			return nil
		})
	}
}
