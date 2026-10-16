package webdav

import (
	"path"
	"path/filepath"
	"strings"
)

// resolve turns the path of a request into the path of the object in the root. The path is cleaned as an absolute one, so that no `..` can leave the root.
// ok is false when the path does not start with the prefix of the share.
func resolve(root, prefix, urlPath string) (rel, full string, ok bool) {
	p := urlPath
	if prefix != "" {
		if !strings.HasPrefix(p, prefix) {
			return "", "", false
		}
		p = p[len(prefix):]
		if p != "" && !strings.HasPrefix(p, "/") {
			return "", "", false
		}
	}
	rel = path.Clean("/" + p)
	return rel, filepath.Join(filepath.Clean(root), filepath.FromSlash(rel)), true
}
