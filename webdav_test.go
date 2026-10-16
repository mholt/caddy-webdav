package webdav

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// newHandler makes a module with its root in a directory below a scratch one (so that a write outside the root can be seen), and a function that serves a request.
func newHandler(t *testing.T, tune ...func(*WebDAV)) (root, outside string, do func(method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder) {
	t.Helper()
	outside = t.TempDir()
	root = filepath.Join(outside, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := &WebDAV{Root: root}
	for _, f := range tune {
		f(wd)
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := wd.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error { w.WriteHeader(http.StatusTeapot); return nil })
	do = func(method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://store"+target, body)
		r.URL.Path = target
		if i := strings.IndexByte(target, '?'); i >= 0 {
			r.URL.Path, r.URL.RawQuery = target[:i], target[i+1:]
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
		w := httptest.NewRecorder()
		if err := wd.ServeHTTP(w, r, next); err != nil {
			t.Fatal(err)
		}
		return w
	}
	return
}

func createDirs(w *WebDAV) { w.CreateDirs = true }

func TestCreateDirsMakesTheParents(t *testing.T) {
	root, _, do := newHandler(t, createDirs)
	if w := do("PUT", "/acme/r1/dist/app.txt", strings.NewReader("hello")); w.Code != http.StatusCreated {
		t.Fatalf("code %d", w.Code)
	}
	if got, err := os.ReadFile(filepath.Join(root, "acme/r1/dist/app.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("content: %v %q", err, got)
	}
}

func TestWithoutCreateDirsAMissingParentIsRefused(t *testing.T) {
	root, _, do := newHandler(t)
	if w := do("PUT", "/acme/r1/app.txt", strings.NewReader("hello")); w.Code < 400 {
		t.Fatalf("code %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "acme")); err == nil {
		t.Fatal("a directory was made")
	}
}

func TestCreateDirsNeverLeavesTheRoot(t *testing.T) {
	_, outside, do := newHandler(t, createDirs)
	for _, p := range []string{"/../../escape1/x.bin", "/a/../../../escape2/x.bin", "/./../escape3/x.bin", "//../escape4/x.bin"} {
		do("PUT", p, strings.NewReader("x"))
	}
	entries, _ := os.ReadDir(outside)
	for _, e := range entries {
		if e.Name() != "root" {
			t.Fatalf("%q was made outside the root", e.Name())
		}
	}
}

func TestCreateDirsKeepsToThePrefix(t *testing.T) {
	root, _, do := newHandler(t, createDirs, func(w *WebDAV) { w.Prefix = "/store" })
	if w := do("PUT", "/store/a/b", strings.NewReader("x")); w.Code != http.StatusCreated {
		t.Fatalf("under the prefix: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	do("PUT", "/storeX/c/d", strings.NewReader("x"))
	do("PUT", "/other/e/f", strings.NewReader("x"))
	for _, d := range []string{"c", "e", "storeX", "other"} {
		if _, err := os.Stat(filepath.Join(root, d)); err == nil {
			t.Fatalf("%q was made for a path outside the prefix", d)
		}
	}
}

func TestSettings(t *testing.T) {
	d := caddyfile.NewTestDispenser(`webdav {
		root /data
		prefix /s
		create_dirs
	}`)
	wd := new(WebDAV)
	if err := wd.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if wd.Root != "/data" || wd.Prefix != "/s" || !wd.CreateDirs {
		t.Fatalf("%+v", wd)
	}
}
