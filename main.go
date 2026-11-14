// dropbin — a tiny drag-and-drop uploader for a media library.
// Single binary, no dependencies outside the Go standard library.
package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// ---------- config (all from environment) ----------

type config struct {
	Root       string
	Port       string
	Title      string
	Password   string
	AllowedExt map[string]bool // empty = allow everything
	MaxBytes   int64           // 0 = unlimited
	Overwrite  bool
	URLFetch   bool
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func loadConfig() config {
	c := config{
		Root:      env("LIBRARY_ROOT", "/library"),
		Port:      env("PORT", "8080"),
		Title:     env("TITLE", "dropbin"),
		Password:  env("PASSWORD", ""),
		Overwrite: env("OVERWRITE", "false") == "true",
		URLFetch:  env("ENABLE_URL_FETCH", "true") == "true",
	}
	c.AllowedExt = map[string]bool{}
	exts := env("ALLOWED_EXTENSIONS", "mp4,mkv,avi,mov,m4v,webm,wmv,flv,ts,m2ts,mpg,mpeg,srt,ass,ssa,vtt,sub,idx")
	if exts != "*" {
		for _, e := range strings.Split(exts, ",") {
			e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
			if e != "" {
				c.AllowedExt[e] = true
			}
		}
	}
	if gb, err := strconv.ParseFloat(env("MAX_UPLOAD_GB", "0"), 64); err == nil && gb > 0 {
		c.MaxBytes = int64(gb * 1024 * 1024 * 1024)
	}
	return c
}

var cfg config

// ---------- path helpers ----------

// cleanRel turns user input like "Show/Season 01" into a safe relative path,
// rejecting anything that tries to climb out of the library.
func cleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(p, "/ ")
	if p == "" {
		return "", nil
	}
	for _, part := range strings.Split(p, "/") {
		part = strings.TrimSpace(part)
		if part == ".." || part == "." || strings.HasPrefix(part, ".") {
			return "", errors.New("invalid folder name")
		}
	}
	return filepath.Clean(p), nil
}

func cleanName(n string) (string, error) {
	n = strings.ReplaceAll(n, "\\", "/")
	n = strings.TrimSpace(path.Base(n))
	if n == "" || n == "." || n == ".." || n == "/" || strings.HasPrefix(n, ".") {
		return "", errors.New("invalid file name")
	}
	if len(cfg.AllowedExt) > 0 {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(n), "."))
		if !cfg.AllowedExt[ext] {
			return "", fmt.Errorf("file type .%s is not allowed", ext)
		}
	}
	return n, nil
}

// targetDir resolves destination + subfolder to an absolute dir inside Root.
func targetDir(dest, sub string) (string, error) {
	d, err := cleanRel(dest)
	if err != nil || d == "" || strings.Contains(d, string(os.PathSeparator)) {
		return "", errors.New("pick a destination")
	}
	if fi, err := os.Stat(filepath.Join(cfg.Root, d)); err != nil || !fi.IsDir() {
		return "", errors.New("unknown destination")
	}
	s, err := cleanRel(sub)
	if err != nil {
		return "", err
	}
	full := filepath.Join(cfg.Root, d, s)
	if !strings.HasPrefix(full+string(os.PathSeparator), filepath.Clean(cfg.Root)+string(os.PathSeparator)) {
		return "", errors.New("invalid path")
	}
	return full, nil
}

// uniquePath returns dir/name, or "name (1).ext" etc. if it already exists.
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if cfg.Overwrite {
		return p
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

// saveStream writes r to dir/name via a hidden .part file, renaming on success.
func saveStream(dir, name string, r io.Reader, progress func(int64)) (string, int64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	final := uniquePath(dir, name)
	tmp := filepath.Join(dir, "."+filepath.Base(final)+".part")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	var w io.Writer = f
	if progress != nil {
		w = &progressWriter{w: f, fn: progress}
	}
	n, err := io.Copy(w, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", n, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", n, err
	}
	return final, n, nil
}

type progressWriter struct {
	w  io.Writer
	n  int64
	fn func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	p.fn(p.n)
	return n, err
}

func rel(full string) string {
	r, _ := filepath.Rel(cfg.Root, full)
	return filepath.ToSlash(r)
}

// ---------- handlers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// GET /api/config — destinations and settings for the UI.
func handleConfig(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(cfg.Root)
	if err != nil {
		fail(w, 500, "library root not readable: "+err.Error())
		return
	}
	dests := []string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(cfg.Root, e.Name())); err == nil && fi.IsDir() {
				dests = append(dests, e.Name())
			}
		}
	}
	exts := []string{}
	for e := range cfg.AllowedExt {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	writeJSON(w, 200, map[string]any{
		"title":        cfg.Title,
		"destinations": dests,
		"extensions":   exts,
		"maxBytes":     cfg.MaxBytes,
		"urlFetch":     cfg.URLFetch,
	})
}

// GET /api/folders?dest=Movies — existing subfolders (2 levels) for autocomplete.
func handleFolders(w http.ResponseWriter, r *http.Request) {
	base, err := targetDir(r.URL.Query().Get("dest"), "")
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	out := []string{}
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == base {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			rp, _ := filepath.Rel(base, p)
			out = append(out, filepath.ToSlash(rp))
			if strings.Count(rp, string(os.PathSeparator)) >= 1 {
				return filepath.SkipDir
			}
		}
		return nil
	})
	sort.Strings(out)
	if len(out) > 2000 {
		out = out[:2000]
	}
	writeJSON(w, 200, out)
}

// PUT /api/upload?dest=&sub=&name= — raw file body, streamed to disk.
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	q := r.URL.Query()
	dir, err := targetDir(q.Get("dest"), q.Get("sub"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	name, err := cleanName(q.Get("name"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if cfg.MaxBytes > 0 {
		if r.ContentLength > cfg.MaxBytes {
			fail(w, 413, "file is larger than the upload limit")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxBytes)
	}
	final, n, err := saveStream(dir, name, r.Body, nil)
	if err != nil {
		log.Printf("upload failed %s: %v", name, err)
		fail(w, 500, "upload failed: "+err.Error())
		return
	}
	log.Printf("uploaded %s (%d bytes)", rel(final), n)
	writeJSON(w, 200, map[string]any{"path": rel(final), "bytes": n})
}

// ---------- URL fetch jobs ----------

type job struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Name  string `json:"name"`
	Total int64  `json:"total"`
	Done  int64  `json:"done"`
	State string `json:"state"` // running | done | error
	Error string `json:"error,omitempty"`
	Path  string `json:"path,omitempty"`
	Start int64  `json:"start"`
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*job{}
)

func nameFromResponse(resp *http.Response, raw string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil && params["filename"] != "" {
			return params["filename"]
		}
	}
	if u, err := url.Parse(resp.Request.URL.String()); err == nil {
		if b, err := url.PathUnescape(path.Base(u.Path)); err == nil && b != "/" && b != "." {
			return b
		}
	}
	return ""
}

// POST /api/fetch {url, dest, sub, name} — server downloads a direct file link.
func handleFetch(w http.ResponseWriter, r *http.Request) {
	if !cfg.URLFetch {
		fail(w, 403, "URL fetching is disabled")
		return
	}
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	var in struct{ URL, Dest, Sub, Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		fail(w, 400, "enter an http(s) link")
		return
	}
	dir, err := targetDir(in.Dest, in.Sub)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if in.Name != "" {
		if _, err := cleanName(in.Name); err != nil {
			fail(w, 400, err.Error())
			return
		}
	}

	j := &job{ID: strconv.FormatInt(time.Now().UnixNano(), 36), URL: u.String(), Name: in.Name, State: "running", Start: time.Now().UnixMilli()}
	jobsMu.Lock()
	jobs[j.ID] = j
	jobsMu.Unlock()

	go func() {
		set := func(f func()) { jobsMu.Lock(); f(); jobsMu.Unlock() }
		failJob := func(e error) {
			set(func() { j.State, j.Error = "error", e.Error() })
			log.Printf("fetch failed %s: %v", j.URL, e)
		}

		resp, err := http.Get(j.URL)
		if err != nil {
			failJob(err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			failJob(fmt.Errorf("server replied %s", resp.Status))
			return
		}
		name := in.Name
		if name == "" {
			name = nameFromResponse(resp, j.URL)
		}
		name, err = cleanName(name)
		if err != nil {
			failJob(fmt.Errorf("%v — set a file name ending in a video extension", err))
			return
		}
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			failJob(errors.New("that link is a web page, not a video file"))
			return
		}
		if cfg.MaxBytes > 0 && resp.ContentLength > cfg.MaxBytes {
			failJob(errors.New("file is larger than the upload limit"))
			return
		}
		set(func() { j.Name, j.Total = name, resp.ContentLength })
		var body io.Reader = resp.Body
		if cfg.MaxBytes > 0 {
			body = io.LimitReader(resp.Body, cfg.MaxBytes)
		}
		final, n, err := saveStream(dir, name, body, func(n int64) { set(func() { j.Done = n }) })
		if err != nil {
			failJob(err)
			return
		}
		set(func() { j.State, j.Done, j.Path = "done", n, rel(final) })
		log.Printf("fetched %s (%d bytes)", rel(final), n)
	}()

	writeJSON(w, 202, j)
}

// GET /api/jobs — status of URL fetches (finished ones are kept for an hour).
func handleJobs(w http.ResponseWriter, r *http.Request) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	out := []job{}
	cutoff := time.Now().Add(-time.Hour).UnixMilli()
	for id, j := range jobs {
		if j.State != "running" && j.Start < cutoff {
			delete(jobs, id)
			continue
		}
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Start > out[b].Start })
	writeJSON(w, 200, out)
}

// ---------- wiring ----------

func auth(next http.Handler) http.Handler {
	if cfg.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(cfg.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+cfg.Title+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	cfg = loadConfig()
	if fi, err := os.Stat(cfg.Root); err != nil || !fi.IsDir() {
		log.Fatalf("LIBRARY_ROOT %q is not a directory — mount your library there", cfg.Root)
	}

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/folders", handleFolders)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/fetch", handleFetch)
	mux.HandleFunc("/api/jobs", handleJobs)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           auth(mux),
		ReadHeaderTimeout: 15 * time.Second,
		// No Read/Write timeouts: large uploads can take a long time.
	}
	log.Printf("%s listening on :%s, library at %s", cfg.Title, cfg.Port, cfg.Root)
	log.Fatal(srv.ListenAndServe())
}
