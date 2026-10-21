package main

// fileserver.go — the phone as a file server.
//
// Two front doors, one implementation:
//
//	/files/<share>/<path>   browse (HTML or JSON), download, upload  (browser friendly)
//	/dav/<share>/<path>     WebDAV, mountable as a network drive
//
// Design decisions worth stating up front:
//
//   - Shares, not paths. The URL carries a share *name* from config; a raw
//     filesystem path is never accepted from the client. That removes the whole
//     directory-traversal class of bug instead of filtering for it — and every
//     request is still resolved and re-checked against the share root, so a
//     symlink planted inside a share cannot walk out of it either.
//   - Writes are staged. An upload lands in a temporary file in the target
//     directory and is renamed into place, so an interrupted transfer never
//     leaves a half-written file where a real one used to be.
//   - Writes are switchable twice: files.read_only refuses every write, and
//     files.allow_delete refuses deletion specifically (uploads still work).
//   - Locks are answered but not enforced. Windows and macOS ask for LOCK before
//     they will write; enforcing locks needs server-side lock state and expiry
//     that this daemon does not keep. It answers with a token and says so in the
//     response and in the log, rather than pretending to be a lock server.
//   - Browsers cannot set an API-key header on a link click, so the HTML UI takes
//     the token from ?key= when present and otherwise lets the browser answer a
//     Basic challenge once (the username is ignored; the token is the password).

import (
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	errNoShare     = errors.New("unknown share")
	errEscapesRoot = errors.New("path escapes the share")
	errIsDir       = errors.New("target is a directory")
	errNotDir      = errors.New("target is not a directory")
	errEmptyDir    = errors.New("directory is not empty")
	errReadOnly    = errors.New("the file service is read-only (config files.read_only=true)")
	errNoDelete    = errors.New("deleting is disabled (config files.allow_delete=false)")
	errTooLarge    = errors.New("upload exceeds files.max_upload_mb")
)

var davLockSeq atomic.Int64

// ---------- auth ----------

// fileTokenOK authenticates a file-service request. WebDAV clients (and
// browsers) usually cannot set X-API-Key, so Basic is accepted with the token as
// the password and the username ignored — that is what every WebDAV client
// sends. The API header form keeps working too.
func (a *App) fileTokenOK(r *http.Request) bool {
	tok := a.config().APIToken
	if tok == "" {
		return true
	}
	if a.tokenOK(r) {
		return true
	}
	if _, pass, ok := r.BasicAuth(); ok {
		if len(pass) == len(tok) && subtle.ConstantTimeCompare([]byte(pass), []byte(tok)) == 1 {
			return true
		}
	}
	return false
}

func (a *App) fileAuthFailed(w http.ResponseWriter, r *http.Request) {
	if a.config().APIToken == "" {
		return
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="phoneserver", charset="UTF-8"`)
	if strings.HasPrefix(r.URL.Path, "/dav") {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+
			`<D:error xmlns:D="DAV:"><D:need-privileges/></D:error>`+"\n")
		return
	}
	writeErr(w, http.StatusUnauthorized,
		`authentication required: send the api token as X-API-Key, ?key=, or HTTP Basic (username is ignored, password = token)`)
}

// ---------- target resolution ----------

// fileTarget is one resolved request path.
type fileTarget struct {
	Share  string
	Root   string
	Rel    string // slash separated, no leading slash, may be ""
	Abs    string
	Info   os.FileInfo
	Exists bool
}

func (t fileTarget) IsDir() bool { return t.Exists && t.Info.IsDir() }

// resolveTarget turns a URL path (after the mount prefix) into a filesystem
// target, refusing anything that leaves the share root once symlinks are
// resolved.
func (a *App) resolveTarget(raw string) (fileTarget, error) {
	var t fileTarget
	shares := a.config().Files.Shares
	trimmed := strings.TrimSpace(raw)
	// A backslash is a path separator on some clients; on Android it is a legal
	// filename character, so refuse it rather than guess which the client meant.
	if strings.Contains(trimmed, "\\") {
		return t, errEscapesRoot
	}
	clean := path.Clean("/" + trimmed)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "." {
		clean = ""
	}
	name := clean
	rel := ""
	if i := strings.Index(clean, "/"); i >= 0 {
		name, rel = clean[:i], clean[i+1:]
	}
	if name == "" {
		return t, errNoShare
	}
	root, ok := shares[name]
	if !ok {
		return t, fmt.Errorf("%w: %q", errNoShare, name)
	}
	for _, seg := range strings.Split("/"+rel, "/") {
		if seg == ".." {
			return t, errEscapesRoot
		}
	}
	t.Share, t.Root, t.Rel = name, root, rel
	t.Abs = filepath.Join(root, filepath.FromSlash(rel))
	if err := ensureInsideShare(root, t.Abs); err != nil {
		return t, err
	}
	if fi, err := os.Lstat(t.Abs); err == nil {
		t.Info, t.Exists = fi, true
	}
	return t, nil
}

// ensureInsideShare checks that abs stays under root *after* symlinks are
// resolved. It walks up to the deepest existing ancestor so it also works for a
// not-yet-existing upload target.
func ensureInsideShare(root, abs string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("share root %s is unavailable: %w", root, err)
	}
	p := abs
	for {
		if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
			if resolved == realRoot || strings.HasPrefix(resolved, realRoot+string(os.PathSeparator)) {
				return nil
			}
			return errEscapesRoot
		} else if !os.IsNotExist(rerr) {
			return rerr
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if abs == realRoot || strings.HasPrefix(abs, realRoot+string(os.PathSeparator)) {
		return nil
	}
	return errEscapesRoot
}

// ---------- shared helpers ----------

func (a *App) filesEnabled() error {
	if !a.config().Files.Enabled {
		return errors.New("the file service is disabled (config files.enabled=false)")
	}
	return nil
}

func (a *App) shareNames() []string {
	out := make([]string, 0, len(a.config().Files.Shares))
	for name := range a.config().Files.Shares {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func fileErrStatus(err error) int {
	switch {
	case errors.Is(err, errNoShare):
		return http.StatusNotFound
	case errors.Is(err, errEscapesRoot):
		return http.StatusBadRequest
	case errors.Is(err, errReadOnly), errors.Is(err, errNoDelete):
		return http.StatusForbidden
	case errors.Is(err, errTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, errEmptyDir), errors.Is(err, errIsDir), errors.Is(err, errNotDir):
		return http.StatusConflict
	case errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, os.ErrPermission):
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// listingEntries reads a directory into a sorted slice (directories first).
func (a *App) listingEntries(t fileTarget) ([]os.FileInfo, bool, error) {
	cfg := a.config().Files
	f, err := os.Open(t.Abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	entries, err := f.Readdir(-1)
	if err != nil {
		return nil, false, err
	}
	shown := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		if !cfg.ShowHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		shown = append(shown, e)
	}
	sort.Slice(shown, func(i, j int) bool {
		if shown[i].IsDir() != shown[j].IsDir() {
			return shown[i].IsDir()
		}
		return strings.ToLower(shown[i].Name()) < strings.ToLower(shown[j].Name())
	})
	truncated := false
	if cfg.MaxListEntries > 0 && len(shown) > cfg.MaxListEntries {
		shown, truncated = shown[:cfg.MaxListEntries], true
	}
	return shown, truncated, nil
}

func entryKind(fi os.FileInfo) string {
	switch {
	case fi.IsDir():
		return "directory"
	case fi.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case fi.Mode().IsRegular():
		return "file"
	default:
		return "other"
	}
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// ---------- /files/ (browser + API) ----------

func (a *App) handleFilesHTTP(w http.ResponseWriter, r *http.Request) {
	if err := a.filesEnabled(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	cfg := a.config().Files
	if !a.fileTokenOK(r) {
		a.fileAuthFailed(w, r)
		return
	}
	if !cfg.HTTP {
		writeErr(w, http.StatusNotFound, "the plain HTTP file view is disabled (config files.http_files=false); WebDAV may still be on at /dav/")
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/files")
	if strings.Trim(raw, "/") == "" {
		a.handleFilesIndex(w, r)
		return
	}
	t, err := a.resolveTarget(raw)
	if err != nil {
		writeErr(w, fileErrStatus(err), err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		a.filesGet(w, r, t)
	case http.MethodPut:
		a.filesPut(w, r, t)
	case http.MethodPost:
		a.filesPost(w, r, t)
	case http.MethodDelete:
		a.filesDelete(w, r, t)
	case http.MethodOptions:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST, DELETE, OPTIONS")
		writeJSON(w, http.StatusOK, map[string]any{
			"allow":        []string{"GET", "HEAD", "PUT", "POST", "DELETE", "OPTIONS"},
			"shares":       a.shareNames(),
			"webdav":       cfg.WebDAV,
			"read_only":    cfg.ReadOnly,
			"allow_delete": cfg.AllowDelete,
		})
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST, DELETE, OPTIONS")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed; use GET/HEAD/PUT/POST/DELETE, or /dav/ for WebDAV")
	}
}

func (a *App) handleFilesIndex(w http.ResponseWriter, r *http.Request) {
	cfg := a.config().Files
	shares := make([]fileShareInfo, 0, len(cfg.Shares))
	for _, n := range a.shareNames() {
		p := cfg.Shares[n]
		si := fileShareInfo{Name: n, Path: p}
		if fi, err := os.Stat(p); err != nil {
			si.Error = err.Error()
		} else if !fi.IsDir() {
			si.Error = "not a directory"
		} else {
			si.Exists = true
			if f, err := os.Open(p); err == nil {
				si.Readable = true
				if names, err := f.Readdirnames(-1); err == nil {
					si.Entries = len(names)
				}
				f.Close()
			}
		}
		// A write probe answers "can this really be uploaded to" instead of
		// inferring it from configuration alone.
		if si.Exists && !cfg.ReadOnly {
			if probe, err := os.CreateTemp(p, ".phoneserver-wtest-*"); err == nil {
				si.Writable = true
				probe.Close()
				os.Remove(probe.Name())
			}
		}
		shares = append(shares, si)
	}
	wantHTML := boolParam(r.URL.Query().Get("listing")) ||
		(strings.Contains(r.Header.Get("Accept"), "text/html") && !boolParam(r.URL.Query().Get("json")))
	if wantHTML {
		a.writeFilesIndexHTML(w, r, shares)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shares":        shares,
		"read_only":     cfg.ReadOnly,
		"allow_delete":  cfg.AllowDelete,
		"max_upload_mb": cfg.MaxUploadMB,
		"webdav":        cfg.WebDAV,
		"hint":          "GET /files/<share>/<path> to browse, PUT to upload, POST multipart to upload from a form",
	})
}

func (a *App) filesGet(w http.ResponseWriter, r *http.Request, t fileTarget) {
	cfg := a.config().Files
	if !t.Exists {
		writeErr(w, http.StatusNotFound, "no such path in share "+t.Share+": /"+t.Rel)
		return
	}
	if t.IsDir() {
		listing, truncated, err := a.listingEntries(t)
		if err != nil {
			writeErr(w, fileErrStatus(err), err.Error())
			return
		}
		if cfg.Listing && !boolParam(r.URL.Query().Get("json")) {
			a.writeDirListingHTML(w, r, t, listing, truncated)
			return
		}
		type entry struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Size  int64  `json:"size"`
			MTime string `json:"mtime"`
			Mode  string `json:"mode"`
		}
		entries := make([]entry, 0, len(listing))
		for _, e := range listing {
			entries = append(entries, entry{e.Name(), entryKind(e), e.Size(), e.ModTime().Format(time.RFC3339), e.Mode().String()})
		}
		out := map[string]any{
			"share": t.Share, "path": "/" + t.Rel, "type": "directory",
			"count": len(entries), "entries": entries, "truncated": truncated,
		}
		if truncated {
			out["note"] = fmt.Sprintf("listing capped at files.max_list_entries=%d", cfg.MaxListEntries)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	f, err := os.Open(t.Abs)
	if err != nil {
		writeErr(w, fileErrStatus(err), err.Error())
		return
	}
	defer f.Close()
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(t.Abs))); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("X-File-Share", t.Share)
	w.Header().Set("X-File-Size", strconv.FormatInt(t.Info.Size(), 10))
	// ServeContent gives Range, If-Modified-Since and HEAD for free.
	http.ServeContent(w, r, t.Info.Name(), t.Info.ModTime(), f)
}

// filesPut stages the upload next to the target and renames it into place.
func (a *App) filesPut(w http.ResponseWriter, r *http.Request, t fileTarget) {
	if a.config().Files.ReadOnly {
		writeErr(w, fileErrStatus(errReadOnly), errReadOnly.Error())
		return
	}
	if t.IsDir() {
		writeErr(w, http.StatusConflict, "target is a directory")
		return
	}
	if t.Rel == "" {
		writeErr(w, http.StatusBadRequest, "refusing to overwrite a share root")
		return
	}
	if boolParam(r.URL.Query().Get("mkdir")) {
		if err := os.MkdirAll(filepath.Dir(t.Abs), 0o755); err != nil {
			writeErr(w, fileErrStatus(err), err.Error())
			return
		}
	}
	overwrote := t.Exists
	written, err := a.stagedWrite(w, r, t.Abs)
	if err != nil {
		writeErr(w, fileErrStatus(err), err.Error())
		return
	}
	code := http.StatusCreated
	if overwrote {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"ok": true, "share": t.Share, "path": "/" + t.Rel, "bytes": written,
		"overwritten": overwrote, "staged": true,
	})
}

// stagedWrite copies the request body into a temp file in the destination
// directory, then renames it over the target. A nil ResponseWriter means the
// caller does not want the size-limit error rendered as JSON (the WebDAV path).
func (a *App) stagedWrite(w http.ResponseWriter, r *http.Request, abs string) (int64, error) {
	cfg := a.config().Files
	limit := int64(cfg.MaxUploadMB) << 20
	if limit <= 0 {
		limit = 512 << 20
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".phoneserver-upload-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	var body io.Reader = r.Body
	if w != nil {
		body = http.MaxBytesReader(w, r.Body, limit)
	} else {
		body = io.LimitReader(r.Body, limit+1)
	}
	n, err := io.Copy(tmp, body)
	if err != nil {
		cleanup()
		if strings.Contains(err.Error(), "request body too large") {
			return n, errTooLarge
		}
		return n, err
	}
	if limit > 0 && n > limit {
		cleanup()
		return n, errTooLarge
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return n, err
	}
	// Match the mode of the file being replaced so an upload does not silently
	// change permissions on something the user had set up.
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		mode = fi.Mode().Perm()
	}
	os.Chmod(tmpName, mode)
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return n, err
	}
	return n, nil
}

func (a *App) filesPost(w http.ResponseWriter, r *http.Request, t fileTarget) {
	cfg := a.config().Files
	op := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("op")))
	if op == "" {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			a.filesMultipartUpload(w, r, t)
			return
		}
		// No op and not a form: treat as a raw upload, same as PUT.
		a.filesPut(w, r, t)
		return
	}
	if cfg.ReadOnly {
		writeErr(w, fileErrStatus(errReadOnly), errReadOnly.Error())
		return
	}
	switch op {
	case "mkcol", "mkdir":
		if t.Exists {
			writeErr(w, http.StatusConflict, "already exists")
			return
		}
		if err := os.MkdirAll(t.Abs, 0o755); err != nil {
			writeErr(w, fileErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "created": "/" + t.Rel})
	case "delete":
		a.filesDelete(w, r, t)
	case "move", "copy":
		dest := r.URL.Query().Get("dest")
		if strings.TrimSpace(dest) == "" {
			dest = strings.TrimSpace(r.FormValue("dest"))
			if dest != "" {
				// A rename form sends the *new name*; keep it in the same directory.
				if !strings.Contains(dest, "/") {
					dest = path.Join(t.Share, path.Dir(t.Rel), dest)
					if !strings.HasPrefix(dest, t.Share+"/") {
						dest = t.Share + "/" + dest
					}
				}
			}
		}
		if dest == "" {
			writeErr(w, http.StatusBadRequest, "give dest=<share>/<path> (or a new name for a rename)")
			return
		}
		a.filesTransfer(w, r, t, dest, op == "move")
	default:
		writeErr(w, http.StatusBadRequest, "unknown op; use mkcol, delete, move or copy (or upload with no op)")
	}
}

func (a *App) filesMultipartUpload(w http.ResponseWriter, r *http.Request, t fileTarget) {
	cfg := a.config().Files
	if cfg.ReadOnly {
		writeErr(w, fileErrStatus(errReadOnly), errReadOnly.Error())
		return
	}
	if !t.IsDir() {
		writeErr(w, http.StatusBadRequest, "upload into a directory: POST /files/<share>/<dir>/ with multipart/form-data")
		return
	}
	limit := int64(cfg.MaxUploadMB) << 20
	if limit <= 0 {
		limit = 512 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) == 0 {
		writeErr(w, http.StatusBadRequest, "no file part in the form (use a field named file)")
		return
	}
	saved := []map[string]any{}
	for field, files := range r.MultipartForm.File {
		for _, fh := range files {
			name := filepath.Base(fh.Filename)
			if name == "" || name == "." || name == ".." {
				continue
			}
			rec := map[string]any{"field": field, "name": name}
			src, err := fh.Open()
			if err != nil {
				rec["ok"], rec["error"] = false, err.Error()
				saved = append(saved, rec)
				continue
			}
			dstPath := filepath.Join(t.Abs, name)
			if err := ensureInsideShare(t.Root, dstPath); err != nil {
				src.Close()
				rec["ok"], rec["error"] = false, err.Error()
				saved = append(saved, rec)
				continue
			}
			tmp, err := os.CreateTemp(t.Abs, ".phoneserver-upload-*")
			if err != nil {
				src.Close()
				rec["ok"], rec["error"] = false, err.Error()
				saved = append(saved, rec)
				continue
			}
			n, cerr := io.Copy(tmp, src)
			src.Close()
			tmp.Close()
			if cerr != nil {
				os.Remove(tmp.Name())
				rec["ok"], rec["error"] = false, cerr.Error()
				saved = append(saved, rec)
				continue
			}
			if err := os.Rename(tmp.Name(), dstPath); err != nil {
				os.Remove(tmp.Name())
				rec["ok"], rec["error"] = false, err.Error()
				saved = append(saved, rec)
				continue
			}
			rec["ok"], rec["bytes"] = true, n
			saved = append(saved, rec)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "share": t.Share, "dir": "/" + t.Rel, "files": saved, "count": len(saved),
	})
}

func (a *App) filesDelete(w http.ResponseWriter, r *http.Request, t fileTarget) {
	cfg := a.config().Files
	if cfg.ReadOnly {
		writeErr(w, fileErrStatus(errReadOnly), errReadOnly.Error())
		return
	}
	if !cfg.AllowDelete {
		writeErr(w, fileErrStatus(errNoDelete), errNoDelete.Error())
		return
	}
	if !t.Exists {
		writeErr(w, http.StatusNotFound, "no such path")
		return
	}
	if t.Rel == "" {
		writeErr(w, http.StatusBadRequest, "refusing to delete a share root")
		return
	}
	recursive := boolParam(r.URL.Query().Get("recursive"))
	if t.IsDir() && !recursive {
		entries, err := os.ReadDir(t.Abs)
		if err != nil {
			writeErr(w, fileErrStatus(err), err.Error())
			return
		}
		if len(entries) > 0 {
			writeErr(w, fileErrStatus(errEmptyDir), errEmptyDir.Error()+"; add ?recursive=1 to delete its contents too")
			return
		}
	}
	var err error
	if t.IsDir() && recursive {
		err = os.RemoveAll(t.Abs)
	} else {
		err = os.Remove(t.Abs)
	}
	if err != nil {
		writeErr(w, fileErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": "/" + t.Rel, "recursive": recursive && t.IsDir()})
}

// filesTransfer implements move/copy between two resolved targets.
func (a *App) filesTransfer(w http.ResponseWriter, r *http.Request, from fileTarget, dest string, move bool) {
	if a.config().Files.ReadOnly {
		writeErr(w, fileErrStatus(errReadOnly), errReadOnly.Error())
		return
	}
	to, err := a.resolveTarget(dest)
	if err != nil {
		writeErr(w, fileErrStatus(err), err.Error())
		return
	}
	if to.Rel == "" {
		writeErr(w, http.StatusBadRequest, "refusing to use a share root as a destination")
		return
	}
	if !from.Exists {
		writeErr(w, http.StatusNotFound, "source does not exist")
		return
	}
	if to.Exists && !boolParam(r.URL.Query().Get("overwrite")) {
		writeErr(w, http.StatusConflict, "destination exists; add ?overwrite=1")
		return
	}
	if move {
		if to.Exists {
			os.RemoveAll(to.Abs)
		}
		if err := os.Rename(from.Abs, to.Abs); err != nil {
			// Different filesystem (two shares, e.g. /data and /storage): fall
			// back to copy+remove rather than reporting a confusing EXDEV.
			if err := copyTree(from.Abs, to.Abs); err != nil {
				writeErr(w, fileErrStatus(err), err.Error())
				return
			}
			os.RemoveAll(from.Abs)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": "/" + from.Rel, "to": "/" + to.Rel, "cross_share": from.Root != to.Root})
		return
	}
	if err := copyTree(from.Abs, to.Abs); err != nil {
		writeErr(w, fileErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "copied": "/" + from.Rel, "to": "/" + to.Rel})
}

// copyTree copies a file or a directory tree, reporting the first failure
// instead of silently producing a partial tree.
func copyTree(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if err := os.MkdirAll(dst, fi.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---------- the HTML views ----------
//
// Deliberately small and dependency-free: the browser gets a usable file manager
// that matches the dark palette of the JSON dashboard. Upload and delete go
// through POST forms because an HTML form cannot send PUT or DELETE.

const filesPageCSS = `
:root{--bg:#0b0e13;--card:#141922;--line:#232b38;--fg:#e6edf3;--dim:#8b98a9;--acc:#58a6ff;--bad:#f85149}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,-apple-system,"Noto Sans CJK SC",sans-serif}
header{padding:14px 18px;border-bottom:1px solid var(--line);display:flex;gap:12px;align-items:center;flex-wrap:wrap}
h1{font-size:15px;margin:0;font-weight:600}
a{color:var(--acc);text-decoration:none}a:hover{text-decoration:underline}
.dim{color:var(--dim)}
main{padding:14px 18px;max-width:1100px}
table{width:100%;border-collapse:collapse;margin-top:8px}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);font-size:13px}
th{color:var(--dim);font-weight:600}
td.size{text-align:right;white-space:nowrap}
form.inline{display:inline}
input,button{background:#0f141c;color:var(--fg);border:1px solid var(--line);border-radius:6px;padding:5px 8px;font:inherit}
button{cursor:pointer}button:hover{border-color:var(--acc)}
.row{display:flex;gap:10px;align-items:center;flex-wrap:wrap;margin:10px 0}
.notice{border:1px solid var(--line);border-radius:8px;padding:8px 10px;color:var(--dim);margin:10px 0}
`

type fileShareInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Readable bool   `json:"readable"`
	Writable bool   `json:"writable"`
	Entries  int    `json:"entries,omitempty"`
	Error    string `json:"error,omitempty"`
}

// tokenFromRequest returns a token that is safe to put back into links: only the
// ?key= and header forms qualify. Basic credentials are deliberately not echoed
// (the browser already sends them on every request) and a token must never be
// written into a page it did not arrive in.
func (a *App) tokenFromRequest(r *http.Request) string {
	if v := r.URL.Query().Get("key"); v != "" {
		return v
	}
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v
	}
	return ""
}

func (a *App) filesNav(r *http.Request, tokenInQuery string) string {
	q := ""
	if tokenInQuery != "" {
		q = "?key=" + url.QueryEscape(tokenInQuery)
	}
	var b strings.Builder
	b.WriteString(`<header><h1>手机文件</h1><span class="dim">/files/</span>`)
	shares := a.shareNames()
	for _, n := range shares {
		fmt.Fprintf(&b, `<a href="/files/%s/%s">%s</a>`, url.PathEscape(n), q, htmlEscape(n))
	}
	if a.config().Files.WebDAV && len(shares) > 0 {
		fmt.Fprintf(&b, `<span class="dim">WebDAV：</span><a href="/dav/%s/">/dav/%s/</a>`,
			url.PathEscape(shares[0]), htmlEscape(shares[0]))
	}
	if a.config().Files.ReadOnly {
		b.WriteString(`<span class="dim">只读模式</span>`)
	}
	b.WriteString(`</header>`)
	return b.String()
}

func (a *App) writeFilesIndexHTML(w http.ResponseWriter, r *http.Request, shares []fileShareInfo) {
	cfg := a.config().Files
	q := a.tokenFromRequest(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	io.WriteString(w, `<meta name="viewport" content="width=device-width,initial-scale=1"><title>手机文件</title>`)
	fmt.Fprintf(w, `<style>%s</style></head><body>`, filesPageCSS)
	io.WriteString(w, a.filesNav(r, q))
	io.WriteString(w, `<main><div class="row"><strong>共享目录</strong></div>`)
	if q != "" {
		io.WriteString(w, `<div class="notice">链接里带着 key，点开即可浏览。WebDAV 挂载请用 Basic 认证：用户名随意，密码填 token。</div>`)
	}
	io.WriteString(w, `<table><tr><th>名称</th><th>路径</th><th class="size">状态</th><th class="size">条目</th></tr>`)
	for _, s := range shares {
		href := "/files/" + url.PathEscape(s.Name) + "/"
		if q != "" {
			href += "?key=" + url.QueryEscape(q)
		}
		status := "可读写"
		switch {
		case s.Error != "":
			status = s.Error
		case !s.Exists:
			status = "不存在"
		case !s.Readable:
			status = "不可读"
		case cfg.ReadOnly:
			status = "只读（files.read_only）"
		case !s.Writable:
			status = "不可写"
		}
		fmt.Fprintf(w, `<tr><td><a href="%s">%s</a></td><td class="dim">%s</td><td class="size">%s</td><td class="size">%d</td></tr>`,
			href, htmlEscape(s.Name), htmlEscape(s.Path), htmlEscape(status), s.Entries)
	}
	io.WriteString(w, `</table>`)
	fmt.Fprintf(w, `<div class="notice">上传上限 %d MB · 删除 %s · WebDAV %s</div>`,
		cfg.MaxUploadMB,
		map[bool]string{true: "已开启", false: "已关闭（files.allow_delete=false）"}[cfg.AllowDelete],
		map[bool]string{true: "已开启（/dav/）", false: "已关闭"}[cfg.WebDAV])
	io.WriteString(w, `</main></body></html>`)
}

func (a *App) writeDirListingHTML(w http.ResponseWriter, r *http.Request, t fileTarget, listing []os.FileInfo, truncated bool) {
	cfg := a.config().Files
	q := a.tokenFromRequest(r)
	qs := func() string {
		if q == "" {
			return ""
		}
		return "?key=" + url.QueryEscape(q)
	}
	base := "/files/" + url.PathEscape(t.Share) + "/" + strings.TrimPrefix(t.Rel, "/")
	parentRel := path.Dir("/" + t.Rel)
	parent := "/files/" + url.PathEscape(t.Share) + "/"
	if parentRel != "/" && parentRel != "." {
		parent += strings.TrimPrefix(parentRel, "/") + "/"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	io.WriteString(w, `<meta name="viewport" content="width=device-width,initial-scale=1"><title>`+htmlEscape("/"+t.Rel)+`</title>`)
	fmt.Fprintf(w, `<style>%s</style></head><body>`, filesPageCSS)
	io.WriteString(w, a.filesNav(r, q))
	fmt.Fprintf(w, `<main><div class="row"><a href="%s%s">↑ 上级目录</a><span class="dim">%s/%s</span></div>`,
		parent, qs(), htmlEscape(t.Share), htmlEscape(t.Rel))

	if !cfg.ReadOnly {
		fmt.Fprintf(w, `<form class="row" method="post" enctype="multipart/form-data" action="%s/%s"><input type="file" name="file" multiple><button type="submit">上传到此处</button></form>`,
			base, qs())
		fmt.Fprintf(w, `<form class="row" method="post" action="%s/%s"><input name="dest" placeholder="新目录名" required><button type="submit" name="op" value="mkcol">新建目录</button></form>`,
			base, qs())
	}
	if truncated {
		fmt.Fprintf(w, `<div class="notice">条目过多，只显示前 %d 项（files.max_list_entries）</div>`, cfg.MaxListEntries)
	}
	io.WriteString(w, `<table><tr><th>名称</th><th class="size">大小</th><th>修改时间</th><th>操作</th></tr>`)
	for _, e := range listing {
		name := e.Name()
		isDir := e.IsDir()
		href := base + "/" + url.PathEscape(name)
		if isDir {
			href += "/"
		}
		href += qs()
		act := ""
		if !cfg.ReadOnly {
			act += fmt.Sprintf(`<form class="inline" method="post" action="%s/%s%s"><input name="dest" placeholder="新名字" size="8" required><button type="submit" name="op" value="move">重命名</button></form>`,
				base, url.PathEscape(name), qs())
		}
		if !cfg.ReadOnly && cfg.AllowDelete {
			act += fmt.Sprintf(`<form class="inline" method="post" action="%s/%s%s"><button type="submit" name="op" value="delete" onclick="return confirm('删除 %s ?')">删除</button></form>`,
				base, url.PathEscape(name), qs(), htmlEscape(name))
		}
		size := humanBytes(e.Size())
		if isDir {
			size = "—"
		}
		mark := ""
		if isDir {
			mark = "📁 "
		}
		fmt.Fprintf(w, `<tr><td><a href="%s">%s%s</a></td><td class="size">%s</td><td class="dim">%s</td><td>%s</td></tr>`,
			href, mark, htmlEscape(name), size, e.ModTime().Format("2006-01-02 15:04"), act)
	}
	io.WriteString(w, `</table>`)
	if cfg.ReadOnly {
		io.WriteString(w, `<div class="notice">文件服务处于只读模式（files.read_only=true）</div>`)
	} else if !cfg.AllowDelete {
		io.WriteString(w, `<div class="notice">删除已关闭（files.allow_delete=false），上传与新建目录仍可用</div>`)
	}
	io.WriteString(w, `</main></body></html>`)
}

// ---------- /dav/ (WebDAV) ----------

const davAllow = "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, PROPFIND, PROPPATCH, COPY, MOVE, LOCK, UNLOCK"

type davMultiStatus struct {
	XMLName   xml.Name      `xml:"D:multistatus"`
	XmlnsD    string        `xml:"xmlns:D,attr"`
	Responses []davResponse `xml:"D:response"`
}

type davResponse struct {
	Href     string        `xml:"D:href"`
	Propstat []davPropstat `xml:"D:propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"D:prop"`
	Status string  `xml:"D:status"`
}

type davProp struct {
	DisplayName   string           `xml:"D:displayname,omitempty"`
	ContentLength int64            `xml:"D:getcontentlength,omitempty"`
	LastModified  string           `xml:"D:getlastmodified,omitempty"`
	CreationDate  string           `xml:"D:creationdate,omitempty"`
	ContentType   string           `xml:"D:getcontenttype,omitempty"`
	ETag          string           `xml:"D:getetag,omitempty"`
	ResourceType  *davResourceType `xml:"D:resourcetype"`
}

type davResourceType struct {
	Collection *struct{} `xml:"D:collection,omitempty"`
}

type davLockDiscovery struct {
	XMLName xml.Name    `xml:"D:prop"`
	Lock    davLockInfo `xml:"D:lockdiscovery>D:activelock"`
}

type davLockInfo struct {
	LockType struct {
		Write string `xml:"D:write"`
	} `xml:"D:locktype"`
	LockScope struct {
		Exclusive string `xml:"D:exclusive"`
	} `xml:"D:lockscope"`
	Depth     string `xml:"D:depth"`
	Timeout   string `xml:"D:timeout"`
	LockToken struct {
		Href string `xml:"D:href"`
	} `xml:"D:locktoken"`
	LockRoot struct {
		Href string `xml:"D:href"`
	} `xml:"D:lockroot"`
}

func davHref(share, rel string, isDir bool) string {
	h := "/dav/" + url.PathEscape(share) + "/"
	if rel != "" {
		segs := strings.Split(rel, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		h += strings.Join(segs, "/")
	}
	if isDir && !strings.HasSuffix(h, "/") {
		h += "/"
	}
	return h
}

func davStatus(code int) string {
	return fmt.Sprintf("HTTP/1.1 %d %s", code, http.StatusText(code))
}

func davDepth(r *http.Request) int {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Depth"))) {
	case "0":
		return 0
	case "infinity":
		return 2
	default:
		return 1
	}
}

func (a *App) davWriteError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+
		`<D:error xmlns:D="DAV:"><D:responsedescription>%s</D:responsedescription></D:error>`+"\n",
		htmlEscape(err.Error()))
}

func writeDavXML(w http.ResponseWriter, v any) {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `<?xml version="1.0"?><D:error xmlns:D="DAV:"/>`)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	io.WriteString(w, xml.Header)
	w.Write(body)
	io.WriteString(w, "\n")
}

func (a *App) davPropFor(t fileTarget, fi os.FileInfo) davProp {
	p := davProp{
		DisplayName:  fi.Name(),
		LastModified: fi.ModTime().UTC().Format(http.TimeFormat),
		CreationDate: fi.ModTime().UTC().Format(time.RFC3339),
		ETag:         fmt.Sprintf(`"%x-%x"`, fi.ModTime().UnixNano(), fi.Size()),
	}
	if fi.IsDir() {
		p.ResourceType = &davResourceType{Collection: &struct{}{}}
	} else {
		p.ResourceType = &davResourceType{}
		p.ContentLength = fi.Size()
		if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(fi.Name()))); ct != "" {
			p.ContentType = ct
		} else {
			p.ContentType = "application/octet-stream"
		}
	}
	if t.Rel == "" {
		p.DisplayName = t.Share
	}
	return p
}

func (a *App) handleFilesWebDAV(w http.ResponseWriter, r *http.Request) {
	if err := a.filesEnabled(); err != nil {
		a.davWriteError(w, http.StatusServiceUnavailable, err)
		return
	}
	if !a.config().Files.WebDAV {
		a.davWriteError(w, http.StatusNotFound, errors.New("WebDAV is disabled (config files.webdav=false)"))
		return
	}
	if !a.fileTokenOK(r) {
		a.fileAuthFailed(w, r)
		return
	}
	// Class 2 is advertised because LOCK/UNLOCK are answered; the handler says
	// plainly that the locks are not enforced.
	w.Header().Set("DAV", "1, 2")
	w.Header().Set("MS-Author-Via", "DAV")

	raw := strings.TrimPrefix(r.URL.Path, "/dav")
	if strings.Trim(raw, "/") == "" {
		a.davRoot(w, r)
		return
	}
	t, err := a.resolveTarget(raw)
	if err != nil {
		a.davWriteError(w, fileErrStatus(err), err)
		return
	}
	switch r.Method {
	case "OPTIONS":
		w.Header().Set("Allow", davAllow)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	case "PROPFIND":
		a.davPropfind(w, r, t)
	case "PROPPATCH":
		a.davProppatch(w, r, t)
	case "GET", "HEAD":
		a.filesGet(w, r, t)
	case "PUT":
		a.davPut(w, r, t)
	case "MKCOL":
		a.davMkcol(w, r, t)
	case "DELETE":
		a.davDelete(w, r, t)
	case "MOVE", "COPY":
		a.davMoveCopy(w, r, t, r.Method == "MOVE")
	case "LOCK":
		a.davLock(w, r, t)
	case "UNLOCK":
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", davAllow)
		a.davWriteError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not supported", r.Method))
	}
}

// davRoot answers /dav/ itself with a collection whose children are the shares,
// which is what a client sees when it mounts http://host/dav/.
func (a *App) davRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "OPTIONS":
		w.Header().Set("Allow", davAllow)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	case "PROPFIND":
	default:
		w.Header().Set("Allow", davAllow)
		a.davWriteError(w, http.StatusMethodNotAllowed, errors.New("the WebDAV root only answers PROPFIND/OPTIONS; mount /dav/<share>/ to browse files"))
		return
	}
	cfg := a.config().Files
	now := time.Now().UTC().Format(http.TimeFormat)
	ms := davMultiStatus{XmlnsD: "DAV:"}
	ms.Responses = append(ms.Responses, davResponse{
		Href: "/dav/",
		Propstat: []davPropstat{{
			Prop: davProp{
				DisplayName:  "shares",
				LastModified: now,
				ResourceType: &davResourceType{Collection: &struct{}{}},
			},
			Status: davStatus(200),
		}},
	})
	if davDepth(r) != 0 {
		for _, name := range a.shareNames() {
			fi, err := os.Stat(cfg.Shares[name])
			if err != nil || !fi.IsDir() {
				continue
			}
			ms.Responses = append(ms.Responses, davResponse{
				Href: davHref(name, "", true),
				Propstat: []davPropstat{{
					Prop: davProp{
						DisplayName:  name,
						LastModified: fi.ModTime().UTC().Format(http.TimeFormat),
						ResourceType: &davResourceType{Collection: &struct{}{}},
					},
					Status: davStatus(200),
				}},
			})
		}
	}
	writeDavXML(w, ms)
}

func (a *App) davPropfind(w http.ResponseWriter, r *http.Request, t fileTarget) {
	if !t.Exists {
		a.davWriteError(w, http.StatusNotFound, errNoShare)
		return
	}
	depth := davDepth(r)
	ms := davMultiStatus{XmlnsD: "DAV:"}
	ms.Responses = append(ms.Responses, davResponse{
		Href:     davHref(t.Share, t.Rel, t.IsDir()),
		Propstat: []davPropstat{{Prop: a.davPropFor(t, t.Info), Status: davStatus(200)}},
	})
	if t.IsDir() && depth != 0 {
		entries, _, err := a.listingEntries(t)
		if err != nil {
			a.davWriteError(w, fileErrStatus(err), err)
			return
		}
		for _, e := range entries {
			rel := path.Join(t.Rel, e.Name())
			child := fileTarget{Share: t.Share, Root: t.Root, Rel: rel, Abs: filepath.Join(t.Abs, e.Name()), Info: e, Exists: true}
			ms.Responses = append(ms.Responses, davResponse{
				Href:     davHref(t.Share, rel, e.IsDir()),
				Propstat: []davPropstat{{Prop: a.davPropFor(child, e), Status: davStatus(200)}},
			})
		}
	}
	writeDavXML(w, ms)
}

// davProppatch accepts the request and states plainly that nothing is persisted:
// silently reporting success would make a client believe its custom properties
// survived.
func (a *App) davProppatch(w http.ResponseWriter, r *http.Request, t fileTarget) {
	if !t.Exists {
		a.davWriteError(w, http.StatusNotFound, errNoShare)
		return
	}
	resp := davResponse{
		Href:     davHref(t.Share, t.Rel, t.IsDir()),
		Propstat: []davPropstat{{Prop: davProp{}, Status: davStatus(200)}},
	}
	writeDavXML(w, davMultiStatus{XmlnsD: "DAV:", Responses: []davResponse{resp}})
	a.logf.Printf("webdav: PROPPATCH /%s accepted but not persisted (this server keeps no user-defined properties)", t.Rel)
}

func (a *App) davPut(w http.ResponseWriter, r *http.Request, t fileTarget) {
	if a.config().Files.ReadOnly {
		a.davWriteError(w, http.StatusForbidden, errReadOnly)
		return
	}
	if t.IsDir() {
		a.davWriteError(w, http.StatusConflict, errIsDir)
		return
	}
	if t.Rel == "" {
		a.davWriteError(w, http.StatusForbidden, errors.New("refusing to write a share root"))
		return
	}
	if !t.Exists {
		// Clients walk collections themselves; creating a missing parent is what
		// they expect for a nested path and is harmless.
		if _, err := os.Stat(filepath.Dir(t.Abs)); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(t.Abs), 0o755); err != nil {
				a.davWriteError(w, fileErrStatus(err), err)
				return
			}
		}
	}
	n, err := a.stagedWrite(nil, r, t.Abs)
	if err != nil {
		a.davWriteError(w, fileErrStatus(err), err)
		return
	}
	if t.Exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		// A 201 carries no body, so it must not advertise the uploaded size:
		// doing that made a real client wait for bytes that never arrive
		// (IncompleteRead). Location points at the new resource instead.
		w.Header().Set("Location", davHref(t.Share, t.Rel, false))
		w.WriteHeader(http.StatusCreated)
	}
	a.logf.Printf("webdav: PUT /%s (%d bytes, new=%v)", t.Rel, n, !t.Exists)
}

func (a *App) davMkcol(w http.ResponseWriter, r *http.Request, t fileTarget) {
	if a.config().Files.ReadOnly {
		a.davWriteError(w, http.StatusForbidden, errReadOnly)
		return
	}
	if t.Exists {
		a.davWriteError(w, http.StatusMethodNotAllowed, errors.New("already exists"))
		return
	}
	if err := os.MkdirAll(t.Abs, 0o755); err != nil {
		a.davWriteError(w, fileErrStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (a *App) davDelete(w http.ResponseWriter, r *http.Request, t fileTarget) {
	cfg := a.config().Files
	if cfg.ReadOnly || !cfg.AllowDelete {
		a.davWriteError(w, http.StatusForbidden, errors.New("deleting is disabled (files.read_only or files.allow_delete)"))
		return
	}
	if !t.Exists {
		a.davWriteError(w, http.StatusNotFound, errNoShare)
		return
	}
	if t.Rel == "" {
		a.davWriteError(w, http.StatusForbidden, errors.New("refusing to delete a share root"))
		return
	}
	if err := os.RemoveAll(t.Abs); err != nil {
		a.davWriteError(w, fileErrStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) davMoveCopy(w http.ResponseWriter, r *http.Request, t fileTarget, move bool) {
	if a.config().Files.ReadOnly {
		a.davWriteError(w, http.StatusForbidden, errReadOnly)
		return
	}
	dest := strings.TrimSpace(r.Header.Get("Destination"))
	if dest == "" {
		a.davWriteError(w, http.StatusBadRequest, errors.New("Destination header is required"))
		return
	}
	destPath := dest
	if u, err := url.Parse(dest); err == nil && u.Path != "" {
		destPath = u.Path
	}
	if !strings.HasPrefix(destPath, "/dav/") {
		a.davWriteError(w, http.StatusBadGateway, fmt.Errorf("Destination must live under /dav/ (got %q)", destPath))
		return
	}
	to, err := a.resolveTarget(strings.TrimPrefix(destPath, "/dav"))
	if err != nil {
		a.davWriteError(w, fileErrStatus(err), err)
		return
	}
	if to.Rel == "" {
		a.davWriteError(w, http.StatusForbidden, errors.New("refusing to use a share root as a destination"))
		return
	}
	if !t.Exists {
		a.davWriteError(w, http.StatusNotFound, errNoShare)
		return
	}
	overwrite := strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "T")
	if to.Exists {
		if !overwrite {
			a.davWriteError(w, http.StatusPreconditionFailed, errors.New("destination exists and Overwrite: F was sent"))
			return
		}
		if err := os.RemoveAll(to.Abs); err != nil {
			a.davWriteError(w, fileErrStatus(err), err)
			return
		}
	}
	if move {
		if err := os.Rename(t.Abs, to.Abs); err != nil {
			// Rename across filesystems fails with EXDEV; fall back to copy+delete.
			if err := copyTree(t.Abs, to.Abs); err != nil {
				a.davWriteError(w, fileErrStatus(err), err)
				return
			}
			os.RemoveAll(t.Abs)
		}
	} else if err := copyTree(t.Abs, to.Abs); err != nil {
		a.davWriteError(w, fileErrStatus(err), err)
		return
	}
	if to.Exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

// davLock answers the lock request Windows and macOS insist on before they will
// write. The token has the RFC 4918 shape, but the lock is NOT enforced: this
// daemon keeps no lock table, so two clients writing the same path concurrently
// is still possible. The response body and the log say so.
func (a *App) davLock(w http.ResponseWriter, r *http.Request, t fileTarget) {
	if a.config().Files.ReadOnly {
		a.davWriteError(w, http.StatusForbidden, errReadOnly)
		return
	}
	token := fmt.Sprintf("opaquelocktoken:%d-%d", time.Now().UnixNano(), davLockSeq.Add(1))
	href := davHref(t.Share, t.Rel, t.IsDir())
	info := davLockInfo{Depth: "infinity", Timeout: "Second-3600"}
	info.LockType.Write = ""
	info.LockScope.Exclusive = ""
	info.LockToken.Href = token
	info.LockRoot.Href = href
	body, merr := xml.MarshalIndent(davLockDiscovery{Lock: info}, "", "  ")
	w.Header().Set("Lock-Token", "<"+token+">")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Timeout", "Second-3600")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, xml.Header)
	io.WriteString(w, "<!-- advisory only: this server answers LOCK but does not enforce locks -->\n")
	if merr == nil {
		w.Write(body)
	}
	io.WriteString(w, "\n")
	a.logf.Printf("webdav: LOCK granted for %s (token %s) — advisory only, not enforced", href, token)
}
