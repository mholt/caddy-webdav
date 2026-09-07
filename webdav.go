// Copyright 2015 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package webdav implements a WebDAV server handler module for Caddy.
//
// Derived from work by Henrique Dias: https://github.com/hacdias/caddy-webdav
package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"golang.org/x/net/webdav"
	"golang.org/x/sync/singleflight"
)

// Configurable values and thresholds for atomic uploads. All tunables
// live here so no magic numbers are scattered through the code.
const (
	// defaultTempDirName is the default temp directory name, created
	// alongside the resolved Root when TempFileDir is not configured.
	defaultTempDirName = ".caddy_webdav_temp"

	// tempDirPerm is the permission used when creating the temp directory.
	tempDirPerm = 0o755

	// defaultFilePerm is the permission for newly created files on both
	// the rename and the copy-fallback paths, used when no previous
	// target file exists to inherit permissions from. It matches the
	// result of a direct write under the common umask 022.
	defaultFilePerm = 0o644

	// copyBufferSize is the buffer size for the EXDEV copy fallback
	// (1MB), minimizing write syscalls for large files.
	copyBufferSize = 1 << 20

	// smallCopyBufferSize is the buffer used for small files in the
	// EXDEV copy fallback (128KB), reducing pooled memory residency.
	smallCopyBufferSize = 128 << 10

	// smallFileThreshold: files at or below this size are copied with
	// smallCopyBufferSize instead of copyBufferSize.
	smallFileThreshold = 128 << 10

	// staleTempFileMaxAge is the age after which leftover temp files are
	// considered orphans and removed by the periodic cleanup. It must
	// stay far longer than any plausible upload duration, since cleanup
	// also runs while uploads are in flight (and, on shared filesystems,
	// while OTHER instances may be staging uploads in the same
	// directory).
	staleTempFileMaxAge = 7 * 24 * time.Hour

	// cleanupInterval is how often the stale temp file cleanup runs.
	cleanupInterval = 24 * time.Hour

	// placeholderOpen mirrors the unexported phOpen constant in Caddy's
	// replacer (the opening delimiter of placeholders). Used to detect
	// dynamic, placeholder-bearing config values.
	placeholderOpen = "{"
)

// copyBufPool recycles 1MB buffers used by the EXDEV copy fallback to
// avoid per-upload heap allocations.
var copyBufPool = sync.Pool{
	New: func() any { return make([]byte, copyBufferSize) },
}

// smallCopyBufPool recycles 128KB buffers for small-file copies.
var smallCopyBufPool = sync.Pool{
	New: func() any { return make([]byte, smallCopyBufferSize) },
}

// cleanupGroup deduplicates stale-temp-file cleanup across handler
// instances: multiple WebDAV instances configured with the same temp
// directory share a single cleanup run instead of scanning (and
// racing removes in) the same directory concurrently.
var cleanupGroup singleflight.Group

// bodyTrackerCtxKey is the context key carrying the per-request upload
// body tracker from ServeHTTP down to atomicFile.
type bodyTrackerCtxKey struct{}

// bodyTracker records read-side failures and the declared content
// length for a single upload request, so that atomicFile.Close can
// detect incomplete uploads that never produce a write-side error
// (client disconnects, truncated bodies).
type bodyTracker struct {
	err           error
	contentLength int64
}

// trackingReadCloser wraps the request body and records any read
// failure into the tracker. It is a zero-overhead passthrough as long
// as reads succeed.
type trackingReadCloser struct {
	io.ReadCloser
	tracker *bodyTracker
}

func (rc *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := rc.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		rc.tracker.err = err
	}
	return n, err
}

func init() {
	caddy.RegisterModule(WebDAV{})
}

// WebDAV implements an HTTP handler for responding to WebDAV clients.
type WebDAV struct {
	// The root directory out of which to serve files. If
	// not specified, `{http.vars.root}` will be used if
	// set; otherwise, the current directory is assumed.
	// Accepts placeholders.
	Root string `json:"root,omitempty"`

	// The base path prefix used to access the WebDAV share.
	// Should be used if one more more matchers are used with the
	// webdav directive and it's needed to let the webdav share know
	// what the request base path will be.
	// For example:
	// webdav /some/path/match/* {
	//   root /path
	//   prefix /some/path/match
	// }
	// Accepts placeholders.
	Prefix string `json:"prefix,omitempty"`

	// AtomicUpload enables "write to temp file, then atomically move"
	// for PUT uploads, so readers only ever see a complete file or no
	// file at all. It is DISABLED by default: when off, the handler
	// behaves exactly like upstream (direct writes, per-request root
	// resolution, no temp directory machinery at all).
	//
	// When enabled and Root contains placeholders (i.e. it is
	// resolved per request), TempFileDir MUST be set explicitly,
	// because the temp directory cannot be derived from a
	// request-dependent root. Provisioning fails otherwise.
	AtomicUpload bool `json:"atomic_upload,omitempty"`

	// TempFileDir is the directory where uploads are staged as
	// temporary files before being atomically moved to their final
	// destination. If empty, it defaults to a ".caddy_webdav_temp"
	// directory inside the resolved Root (only allowed when Root is
	// static). Accepts placeholders, but is resolved EXACTLY ONCE on
	// the first request and stays fixed for the process lifetime;
	// if a fixed temp directory is unacceptable, leave AtomicUpload
	// disabled instead. Only effective when AtomicUpload is enabled.
	TempFileDir string `json:"temp_file_dir,omitempty"`

	lockSystem webdav.LockSystem
	logger     *zap.Logger

	// initOnce runs the one-time temp directory initialization lazily
	// on the first request. It is a pointer so that the struct can be
	// safely copied by value (e.g. by the CaddyModule value receiver).
	// initErr persists an initialization failure so every later
	// request fails fast with HTTP 500.
	initOnce   *sync.Once
	initErr    error
	absTempDir string

	// ctx is the provisioning context; it is canceled when this config
	// generation is unloaded (e.g. on reload), and terminates the
	// cleanup goroutine so instance turnover does not leak it.
	ctx context.Context
}

// CaddyModule returns the Caddy module information.
func (WebDAV) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.webdav",
		New: func() caddy.Module { return new(WebDAV) },
	}
}

// Provision sets up the module. It performs no filesystem operations;
// directory creation and stale temp cleanup are deferred to the first
// request via initOnce.
func (wd *WebDAV) Provision(ctx caddy.Context) error {
	wd.logger = ctx.Logger(wd)
	wd.initOnce = new(sync.Once)
	wd.ctx = ctx

	wd.lockSystem = webdav.NewMemLS()
	if wd.Root == "" {
		wd.Root = "{http.vars.root}"
	}

	// A dynamic (placeholder-bearing) Root is only resolved per
	// request, so the temp directory cannot be derived from it.
	// Fail fast at provision time instead of surprising the first
	// request or silently degrading the atomicity guarantee.
	if wd.AtomicUpload && strings.Contains(wd.Root, placeholderOpen) && wd.TempFileDir == "" {
		return fmt.Errorf("atomic upload requires an explicit temp_file_dir when root contains placeholders (root: %s); "+
			"either set temp_file_dir, or leave atomic_upload disabled", wd.Root)
	}

	// Warn when temp_file_dir is set but atomic upload is disabled:
	// the setting has no effect and likely indicates a
	// misconfiguration (e.g. a typo'd or forgotten atomic_upload).
	if !wd.AtomicUpload && wd.TempFileDir != "" {
		wd.logger.Warn("temp_file_dir is set but atomic_upload is disabled; the temp directory will not be used")
	}

	return nil
}

// lazyInit performs the one-time temp directory initialization:
// resolving TempFileDir to an absolute path, creating the directory,
// and starting the periodic stale temp file cleanup. Any fatal
// failure is stored in initErr; there is intentionally no fallback
// to the system /tmp.
//
// The temp directory is resolved exactly once per process lifetime.
// Root itself is NOT resolved here: it may contain placeholders and
// is resolved per request in ServeHTTP.
func (wd *WebDAV) lazyInit(repl *caddy.Replacer) {
	tempDir := repl.ReplaceAll(wd.TempFileDir, "")
	if tempDir == "" {
		// TempFileDir unset: Root is guaranteed to be static here,
		// because Provision rejects a dynamic Root without an
		// explicit TempFileDir. Derive the default next to Root.
		root := repl.ReplaceAll(wd.Root, ".")
		absRoot, err := filepath.Abs(root)
		if err != nil {
			wd.initErr = err
			return
		}
		tempDir = filepath.Join(absRoot, defaultTempDirName)
	}
	absTempDir, err := filepath.Abs(tempDir)
	if err != nil {
		wd.initErr = err
		return
	}

	if err := os.MkdirAll(absTempDir, tempDirPerm); err != nil {
		wd.initErr = err
		return
	}
	wd.absTempDir = absTempDir

	// Log the resolved temp directory once per process lifetime so
	// operators can verify the feature is active and where uploads
	// are staged.
	wd.logger.Info("atomic upload enabled",
		zap.String("temp_dir", wd.absTempDir),
	)

	// Cleanup is best-effort and can be slow on network filesystems
	// with many leftovers; run it periodically in the background so
	// requests are never blocked.
	go wd.periodicCleanup()
}

// periodicCleanup runs the stale temp file cleanup immediately and
// then once every cleanupInterval. It exits when the provisioning
// context is canceled (config reload/unload), so instance turnover
// does not leak the goroutine. A cleanup already in flight is not
// interrupted: it is idempotent, and process exit may simply cut it
// off mid-run.
func (wd *WebDAV) periodicCleanup() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-wd.ctx.Done():
			return
		default:
		}

		// Deduplicate concurrent cleanups of the same temp directory
		// across handler instances; a concurrent caller simply shares
		// the in-flight run instead of scanning the same directory.
		_, _, _ = cleanupGroup.Do(wd.absTempDir, func() (any, error) {
			wd.cleanStaleTempFiles()
			return nil, nil
		})

		select {
		case <-wd.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// cleanStaleTempFiles removes leftover temp files whose modification
// time is older than staleTempFileMaxAge. It scans only the top level
// of the temp directory (temp files never live in subdirectories) and
// continues past per-file failures, logging them at Warn level.
// Concurrent runs for the same directory are deduplicated by the
// caller via cleanupGroup.
func (wd *WebDAV) cleanStaleTempFiles() {
	entries, err := os.ReadDir(wd.absTempDir)
	if err != nil {
		wd.logger.Warn("could not list temp directory for stale file cleanup",
			zap.String("temp_dir", wd.absTempDir),
			zap.Error(err),
		)
		return
	}

	for _, entry := range entries {
		// Subdirectories are not expected in the temp directory;
		// skip them without recursing.
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			wd.logger.Warn("could not stat temp file during cleanup, skipping",
				zap.String("temp_dir", wd.absTempDir),
				zap.String("file", entry.Name()),
				zap.Error(err),
			)
			continue
		}
		if time.Since(info.ModTime()) <= staleTempFileMaxAge {
			continue
		}
		if err := os.Remove(filepath.Join(wd.absTempDir, entry.Name())); err != nil {
			wd.logger.Warn("could not remove stale temp file, skipping",
				zap.String("temp_dir", wd.absTempDir),
				zap.String("file", entry.Name()),
				zap.Error(err),
			)
			continue
		}
	}
}

func (wd *WebDAV) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// TODO: integrate with caddy 2's existing auth features to enforce read-only?
	// read methods: GET, HEAD, OPTIONS
	// write methods: POST, PUT, PATCH, DELETE, COPY, MKCOL, MOVE, PROPPATCH

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	root := repl.ReplaceAll(wd.Root, ".")
	prefix := repl.ReplaceAll(wd.Prefix, "")

	var wdFs webdav.FileSystem

	if wd.AtomicUpload {
		// One-time lazy initialization of the temp directory. A
		// persisted initialization error fails every request fast
		// with HTTP 500.
		wd.initOnce.Do(func() { wd.lazyInit(repl) })
		if wd.initErr != nil {
			return caddyhttp.Error(http.StatusInternalServerError, wd.initErr)
		}

		// Resolve the root per request: it may contain placeholders
		// (e.g. multi-tenant roots). A resolution failure is a
		// per-request error and must not be persisted.
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return caddyhttp.Error(http.StatusInternalServerError, err)
		}

		// For PUT uploads, wrap the request body so read-side failures
		// (client disconnects, truncated transfers) become visible to
		// the atomic file's Close, which would otherwise only see
		// write-side errors and could publish a partial file.
		if r.Method == http.MethodPut {
			tracker := &bodyTracker{contentLength: r.ContentLength}
			r.Body = &trackingReadCloser{ReadCloser: r.Body, tracker: tracker}
			r = r.WithContext(context.WithValue(r.Context(), bodyTrackerCtxKey{}, tracker))
		}

		wdFs = newAtomicFS(absRoot, wd.absTempDir, wd.logger)
	} else {
		// Upstream behavior: direct writes against the per-request
		// resolved root, with no temp directory machinery involved.
		wdFs = webdav.Dir(root)
	}

	wdHandler := webdav.Handler{
		Prefix:     prefix,
		FileSystem: wdFs,
		LockSystem: wd.lockSystem,
		Logger: func(req *http.Request, err error) {
			if err == nil {
				return
			}
			// ignore errors about non-existing files
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			// log webdav request errors at debug level
			if errors.Is(err, webdav.ErrConfirmationFailed) ||
				errors.Is(err, webdav.ErrForbidden) ||
				errors.Is(err, webdav.ErrLocked) ||
				errors.Is(err, webdav.ErrNoSuchLock) {
				wd.logger.Debug("webdav request error",
					zap.Error(err),
					zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: req}),
				)
				return
			}
			// log client-side upload aborts at warn level: they are
			// routine events, not internal server errors
			if errors.Is(err, io.ErrUnexpectedEOF) {
				wd.logger.Warn("upload aborted by client",
					zap.Error(err),
					zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: req}),
				)
			} else {
				wd.logger.Error("internal handler error",
					zap.Error(err),
					zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: req}),
				)
			}
		},
	}

	// Excerpt from RFC4918, section 9.4:
	//
	//     GET, when applied to a collection, may return the contents of an
	//     "index.html" resource, a human-readable view of the contents of
	//     the collection, or something else altogether.
	//
	// GET and HEAD, when applied to a collection, will behave the same as PROPFIND method.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		info, err := wdHandler.FileSystem.Stat(context.TODO(), r.URL.Path)
		if err == nil && info.IsDir() {
			r.Method = "PROPFIND"
			if r.Header.Get("Depth") == "" {
				r.Header.Add("Depth", "1")
			}
		}
	}

	if r.Method == http.MethodHead {
		w = emptyBodyResponseWriter{w}
	}

	wdHandler.ServeHTTP(w, r)

	return nil
}

// emptyBodyResponseWriter is a response writer that does not write a body.
type emptyBodyResponseWriter struct{ http.ResponseWriter }

func (w emptyBodyResponseWriter) Write(data []byte) (int, error) { return 0, nil }

// ---------- Atomic Upload Support ----------

// atomicFS wraps webdav.Dir so that "create and truncate" writes (PUT
// uploads) are staged in a temporary file and atomically moved to the
// final path on Close(). Readers therefore only ever see a complete
// file or no file at all. All other operations pass through untouched.
type atomicFS struct {
	dir        webdav.Dir
	absRoot    string
	absTempDir string
	logger     *zap.Logger
}

func newAtomicFS(absRoot, absTempDir string, logger *zap.Logger) *atomicFS {
	return &atomicFS{
		dir:        webdav.Dir(absRoot),
		absRoot:    absRoot,
		absTempDir: absTempDir,
		logger:     logger,
	}
}

func (fsys *atomicFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return fsys.dir.Mkdir(ctx, name, perm)
}

func (fsys *atomicFS) RemoveAll(ctx context.Context, name string) error {
	return fsys.dir.RemoveAll(ctx, name)
}

func (fsys *atomicFS) Rename(ctx context.Context, oldName, newName string) error {
	return fsys.dir.Rename(ctx, oldName, newName)
}

func (fsys *atomicFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return fsys.dir.Stat(ctx, name)
}

func (fsys *atomicFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	// Only intercept "create and truncate" writes (fresh PUT uploads).
	// Appends and partial updates bypass the atomic path.
	isAtomicWrite := flag&os.O_CREATE != 0 && flag&os.O_TRUNC != 0 &&
		(flag&os.O_WRONLY != 0 || flag&os.O_RDWR != 0) &&
		flag&os.O_APPEND == 0

	if !isAtomicWrite {
		return fsys.dir.OpenFile(ctx, name, flag, perm)
	}

	// Reject paths that webdav.Dir would reject, before doing any
	// filesystem work, so the error semantics match the passthrough
	// path (open-time os.ErrNotExist instead of a late rename failure).
	finalPath := fsys.resolve(name)
	if finalPath == "" {
		return nil, os.ErrNotExist
	}

	// Stage the upload in a random-named temp file. os.CreateTemp uses
	// crypto/rand internally; no timestamp-based pseudo-random seed.
	// The temp file keeps its restrictive 0600 permission while staged;
	// the final permission bits are applied just before publishing in
	// Close().
	tmpFile, err := os.CreateTemp(fsys.absTempDir, "")
	if err != nil {
		return nil, err
	}

	// Pick up the per-request body tracker injected by ServeHTTP. It is
	// absent (nil) for non-PUT requests, in which case Close simply
	// skips the read-side checks.
	tracker, _ := ctx.Value(bodyTrackerCtxKey{}).(*bodyTracker)

	return &atomicFile{
		File:      tmpFile,
		tmpPath:   tmpFile.Name(),
		finalPath: finalPath,
		logger:    fsys.logger,
		tracker:   tracker,
	}, nil
}

// resolve converts a WebDAV slash-separated path into an absolute
// filesystem path under absRoot, mirroring webdav.Dir's resolution.
// It returns an empty string for names that webdav.Dir rejects:
// those containing NUL bytes or, on Windows, misplaced path
// separators. Cleaning is anchored at "/" so ".." segments can never
// escape absRoot.
func (fsys *atomicFS) resolve(name string) string {
	if strings.Contains(name, "\x00") ||
		(filepath.Separator != '/' && strings.IndexRune(name, filepath.Separator) >= 0) {
		return ""
	}
	name = filepath.FromSlash(strings.TrimPrefix(filepath.Clean("/"+name), "/"))
	return filepath.Join(fsys.absRoot, name)
}

// atomicFile wraps a staged temp file and publishes it to the final
// path only when Close() is called after a fully successful write.
//
// A single instance serves exactly one request: Write and Close are
// called sequentially from the same handler goroutine, so no locking
// is needed. The closed flag only guards against a hypothetical double
// Close by the handler, keeping the method idempotent.
type atomicFile struct {
	webdav.File
	tmpPath   string
	finalPath string
	logger    *zap.Logger
	tracker   *bodyTracker
	written   int64
	writeErr  error
	closed    bool
}

func (f *atomicFile) Write(p []byte) (int, error) {
	n, err := f.File.Write(p)
	f.written += int64(n)
	if err != nil {
		f.writeErr = err
	}
	return n, err
}

// Close publishes the staged file. If any earlier write failed, the
// fragment is discarded and the final path is never touched.
func (f *atomicFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true

	// A read-side failure on the request body (client disconnect,
	// truncated transfer) means the staged file is incomplete even
	// though no write ever failed; discard it without publishing.
	if f.tracker != nil && f.tracker.err != nil {
		_ = f.File.Close()
		_ = os.Remove(f.tmpPath)
		return f.tracker.err
	}

	// A declared content length that does not match the bytes written
	// means the body ended early without surfacing a read error (e.g.
	// a proxy silently truncated it); discard as well.
	if f.tracker != nil && f.tracker.contentLength >= 0 && f.written != f.tracker.contentLength {
		_ = f.File.Close()
		_ = os.Remove(f.tmpPath)
		return fmt.Errorf("incomplete upload: declared %d bytes but wrote %d bytes",
			f.tracker.contentLength, f.written)
	}

	// A failed write means the temp file is incomplete; discard it
	// without publishing. The write error is the root cause and takes
	// precedence; a secondary close failure is only logged.
	if f.writeErr != nil {
		if closeErr := f.File.Close(); closeErr != nil {
			f.logger.Warn("could not close temp file after write failure",
				zap.String("path", f.tmpPath),
				zap.Error(closeErr),
			)
		}
		_ = os.Remove(f.tmpPath)
		return f.writeErr
	}

	// Close the underlying handle before moving the file.
	if err := f.File.Close(); err != nil {
		_ = os.Remove(f.tmpPath)
		return err
	}

	// Apply the final permission bits just before publishing. An
	// existing target passes on its mode; new files get the documented
	// default (matching the common umask 022 result of a direct write).
	// os.Chmod bypasses umask, so replicating arbitrary umask semantics
	// for new files is not possible portably. Any stat failure (target
	// missing or otherwise unreadable) implicitly falls back to the
	// default permission.
	var perm os.FileMode = defaultFilePerm
	if info, err := os.Stat(f.finalPath); err == nil {
		perm = info.Mode()
	}
	if err := os.Chmod(f.tmpPath, perm); err != nil {
		_ = os.Remove(f.tmpPath)
		return err
	}

	// Publish atomically. If the temp directory and the destination
	// live on different file systems, os.Rename fails with EXDEV and
	// we fall back to a destination-side copy + rename.
	if err := os.Rename(f.tmpPath, f.finalPath); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			copyErr := f.copyFallback()
			// Remove the staged temp source; copyFallback renamed the
			// destination-side temp file onto the final path but does
			// not remove the original source.
			_ = os.Remove(f.tmpPath)
			return copyErr
		}
		_ = os.Remove(f.tmpPath)
		return err
	}
	return nil
}

// copyFallback handles the cross-device case: it copies the staged
// temp file into a second temp file in the destination directory and
// atomically renames it over the final path. The previous target (if
// any) stays completely intact until the final rename; any mid-copy
// failure only removes the destination-side temp file, so readers
// never see a partial file and the old content is never lost.
func (f *atomicFile) copyFallback() error {
	src, err := os.Open(f.tmpPath)
	if err != nil {
		return err
	}
	defer src.Close()

	// Stat the source once: the size selects the copy buffer pool, and
	// the mode is later applied to the destination-side temp file.
	srcInfo, statErr := src.Stat()

	// Stage the copy in the destination directory so the final rename
	// always happens within a single filesystem.
	dstTmp, err := os.CreateTemp(filepath.Dir(f.finalPath), "")
	if err != nil {
		return err
	}
	dstTmpPath := dstTmp.Name()

	// Pick the buffer pool by file size and always return the buffer
	// to the same pool it came from.
	pool := &copyBufPool
	if statErr == nil && srcInfo.Size() <= smallFileThreshold {
		pool = &smallCopyBufPool
	}
	buf := pool.Get().([]byte)
	_, copyErr := io.CopyBuffer(dstTmp, src, buf)
	pool.Put(buf)
	closeErr := dstTmp.Close()
	if copyErr != nil || closeErr != nil {
		// Never leave a partially-copied file behind. The previous
		// target has not been touched at all.
		_ = os.Remove(dstTmpPath)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}

	// The staged source already carries the final permission bits,
	// applied by Close before the rename attempt. If its stat failed,
	// fall back to the default permission so the published file never
	// leaks the 0600 mode that os.CreateTemp creates.
	var perm os.FileMode = defaultFilePerm
	if statErr == nil {
		perm = srcInfo.Mode()
	}
	if err := os.Chmod(dstTmpPath, perm); err != nil {
		f.logger.Warn("could not apply file permissions after cross-device copy",
			zap.String("path", f.finalPath),
			zap.Error(err),
		)
	}

	// Atomically replace the target; the old content survives every
	// failure path above.
	if err := os.Rename(dstTmpPath, f.finalPath); err != nil {
		_ = os.Remove(dstTmpPath)
		return err
	}
	return nil
}

// Interface guards
var (
	_ caddyhttp.MiddlewareHandler = (*WebDAV)(nil)
	_ caddyfile.Unmarshaler       = (*WebDAV)(nil)
)
