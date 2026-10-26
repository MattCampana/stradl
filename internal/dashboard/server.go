package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	gosync "sync"
	"time"
)

//go:embed web
var webFS embed.FS

// ServerOptions configure a Server.
type ServerOptions struct {
	// Addr is the address to listen on; the default, 127.0.0.1:0, picks a
	// free port. Only loopback addresses are accepted.
	Addr string
}

// Server serves the dashboard on a loopback address. Every request must
// carry the random key from the URL Start returns (as ?t= or the cookie a
// first visit sets), name this server in its Host header, which stops DNS
// rebinding, and, for a POST, come from the dashboard's own page.
type Server struct {
	p    Provider
	opts ServerOptions

	token  string // the ?t= key in the URL
	cookie string // the cookie's value, a second random key
	name   string // cookie name, per port: cookies ignore ports

	mu    gosync.Mutex
	srv   *http.Server
	hosts map[string]bool // accepted Host headers

	overview flight
}

const (
	// requestTimeout bounds one provider call.
	requestTimeout = time.Minute
	// csp allows nothing but this server: no inline script or style, no
	// frames, no third-party anything.
	csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; " +
		"form-action 'none'; frame-ancestors 'none'"
)

// NewServer returns a Server for p. Call Start to listen.
func NewServer(p Provider, opts ServerOptions) *Server {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	return &Server{p: p, opts: opts}
}

// Start listens and serves in the background. It returns the URL to open,
// http://127.0.0.1:PORT/?t=KEY, which is the only way in.
func (s *Server) Start() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return "", errors.New("dashboard server already started")
	}
	host, _, err := net.SplitHostPort(s.opts.Addr)
	if err != nil {
		return "", fmt.Errorf("invalid dashboard address %q: %w", s.opts.Addr, err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("the dashboard only listens on a loopback address, not %q", s.opts.Addr)
	}
	if s.token, err = randomKey(); err != nil {
		return "", err
	}
	if s.cookie, err = randomKey(); err != nil {
		return "", err
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), portOf(s.opts.Addr)))
	if err != nil {
		return "", fmt.Errorf("failed to start the dashboard: %w", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	self := net.JoinHostPort(ip.String(), port)
	s.hosts = map[string]bool{self: true, "localhost:" + port: true, "127.0.0.1:" + port: true}
	s.name = "stradl_" + port

	s.srv = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func(srv *http.Server) { _ = srv.Serve(ln) }(s.srv)
	return "http://" + self + "/?t=" + s.token, nil
}

// Close stops the server, letting requests in flight finish for a moment.
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return srv.Close()
	}
	return nil
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	if port == "" {
		return "0"
	}
	return port
}

func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate the dashboard key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) handler() http.Handler {
	web, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // the embed directive guarantees web/
	}
	static := http.StripPrefix("/static/", http.FileServerFS(web))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, web, "index.html")
	})
	mux.HandleFunc("GET /static/{path...}", func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		if p == "index.html" {
			writeError(w, http.StatusNotFound, "not found") // served at / only
			return
		}
		if fi, err := fs.Stat(web, p); err != nil || fi.IsDir() {
			writeError(w, http.StatusNotFound, "not found") // no directory listings
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	// The same paths with any other method: a JSON 405, not the catch-all's
	// 404.
	mux.HandleFunc("/{$}", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/api/overview", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/api/sessions/{id}", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/api/sync", methodNotAllowed("POST"))
	notFound := func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "not found") }
	mux.HandleFunc("/static", notFound) // rather than a redirect to /static/
	mux.HandleFunc("/", notFound)
	return s.guard(mux)
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, r.Method+" is not allowed here")
	}
}

// guard applies the security headers and checks to every request: Host,
// then the key, then (for anything but a read) the origin.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}

		if !s.hosts[strings.ToLower(r.Host)] {
			writeError(w, http.StatusForbidden, "this dashboard only answers to 127.0.0.1")
			return
		}

		viaQuery := false
		if t := r.URL.Query().Get("t"); t != "" && equal(t, s.token) {
			viaQuery = true
		} else if c, err := r.Cookie(s.name); err != nil || !equal(c.Value, s.cookie) {
			s.unauthorized(w, r)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}

		// A path the mux would clean and redirect (/api/sessions/.., //x)
		// is simply not found: nothing here sends the browser elsewhere.
		if p := r.URL.Path; p != "/" && p != path.Clean(p) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		if viaQuery {
			http.SetCookie(w, &http.Cookie{
				Name: s.name, Value: s.cookie, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			// Trade the key in the address bar for the cookie, so it stays
			// out of history, bookmarks and screenshots.
			if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
				q := r.URL.Query()
				q.Del("t")
				u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
				http.Redirect(w, r, u.String(), http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether a state-changing request came from this
// server's own page: its Origin, or failing that its Referer, must be this
// server. A missing or "null" origin is refused.
func (s *Server) sameOrigin(r *http.Request) bool {
	self := "http://" + strings.ToLower(r.Host)
	if o := r.Header.Get("Origin"); o != "" {
		return strings.EqualFold(o, self)
	}
	ref, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Host == "" {
		return false
	}
	return strings.EqualFold(ref.Scheme+"://"+ref.Host, self)
}

// unauthorized answers a request without a valid key: JSON for the API, a
// short page saying how to get in for anything else.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusUnauthorized, "missing or expired dashboard key; reopen the dashboard from the Stradl menu or run 'stradl dashboard'")
		return
	}
	// The stylesheet is behind the key too, so this page brings its own,
	// allowed by its hash and nothing else.
	w.Header().Set("Content-Security-Policy", unauthorizedCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = fmt.Fprint(w, unauthorizedPage)
}

const unauthorizedStyle = `:root{color-scheme:light dark;--bg:#F4F4F8;--card:#fff;--ink:#0F1222;--muted:#646880;--line:#E4E4ED;--dot:#3B2DFF;--code:#EFEFF5}
@media (prefers-color-scheme:dark){:root{--bg:#0A0C17;--card:#111426;--ink:#F4F4F8;--muted:#9497B0;--line:rgba(244,244,248,.08);--dot:#8378FF;--code:#0D1020}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px 16px;background:var(--bg);color:var(--ink);font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;-webkit-font-smoothing:antialiased}
main{max-width:460px;width:100%;background:var(--card);border:1px solid var(--line);border-radius:14px;padding:28px}
.brand{display:flex;align-items:center;gap:10px;font-weight:700;font-size:20px;letter-spacing:-.02em;margin-bottom:22px}
.ink{fill:var(--ink)}.dot{fill:var(--dot)}h1{font-size:19px;line-height:1.3;margin:0 0 8px;letter-spacing:-.01em}
p{margin:0 0 10px;color:var(--muted)}code{font:13px ui-monospace,SFMono-Regular,Menlo,monospace;background:var(--code);color:var(--ink);padding:2px 6px;border-radius:5px}`

const unauthorizedPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>stradl</title><style>` + unauthorizedStyle + `</style></head><body><main>
<div class="brand"><svg width="26" height="26" viewBox="0 0 512 512" aria-hidden="true"><path class="ink" d="M48 330.717C48 304.564 48 291.487 53.09 281.498A46.698 46.698 0 0 1 73.498 261.09C83.487 256 96.564 256 122.717 256L165.52 256A90.48 90.48 0 0 0 256 346.48L256 389.283C256 415.436 256 428.513 250.91 438.502A46.698 46.698 0 0 1 230.502 458.91C220.513 464 207.436 464 181.283 464L122.717 464C96.564 464 83.487 464 73.498 458.91A46.698 46.698 0 0 1 53.09 438.502C48 428.513 48 415.436 48 389.283Z"/><path class="ink" d="M256 122.717C256 96.564 256 83.487 261.09 73.498A46.698 46.698 0 0 1 281.498 53.09C291.487 48 304.564 48 330.717 48L389.283 48C415.436 48 428.513 48 438.502 53.09A46.698 46.698 0 0 1 458.91 73.498C464 83.487 464 96.564 464 122.717L464 181.283C464 207.436 464 220.513 458.91 230.502A46.698 46.698 0 0 1 438.502 250.91C428.513 256 415.436 256 389.283 256L346.48 256A90.48 90.48 0 0 0 256 165.52Z"/><circle class="dot" cx="256" cy="256" r="74.88"/></svg>stradl</div>
<h1>This dashboard link has expired</h1>
<p>Stradl makes a new link each time it starts, so a link from an earlier run, or one without its key, no longer opens the dashboard.</p>
<p>Open it again from the Stradl menu bar icon (Open Dashboard…), or run <code>stradl dashboard</code> in Terminal.</p>
</main></body></html>
`

// unauthorizedCSP is the page CSP with the 401 page's own style allowed.
var unauthorizedCSP = func() string {
	sum := sha256.Sum256([]byte(unauthorizedStyle))
	return strings.Replace(csp, "style-src 'self'", "style-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'", 1)
}()

func equal(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.overview.do(func() (*Overview, error) {
		// Detached from any one request: callers share the result, so one
		// closing its tab must not cancel it for the rest.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), requestTimeout)
		defer cancel()
		return s.p.Overview(ctx)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

// validID matches what a session ID can be (Claude Code uses UUIDs), which
// keeps anything path-like away from the provider.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		writeError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	d, err := s.p.Session(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	case d == nil:
		writeError(w, http.StatusNotFound, ErrNotFound.Error())
	default:
		writeJSON(w, http.StatusOK, d)
	}
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if err := s.p.SyncNow(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, code int, msg string) {
	data, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(append(data, '\n'))
}

// flight shares one in-progress Overview among concurrent requests (two
// open tabs refreshing together), so the provider reads everything once.
type flight struct {
	mu   gosync.Mutex
	call *flightCall
}

type flightCall struct {
	done chan struct{}
	ov   *Overview
	err  error
}

func (f *flight) do(fn func() (*Overview, error)) (*Overview, error) {
	f.mu.Lock()
	if c := f.call; c != nil {
		f.mu.Unlock()
		<-c.done
		return c.ov, c.err
	}
	c := &flightCall{done: make(chan struct{}), err: errors.New("overview failed")}
	f.call = c
	f.mu.Unlock()

	// Deferred, so a panicking provider (net/http recovers it) can't leave
	// every later request waiting on this call.
	defer func() {
		f.mu.Lock()
		f.call = nil
		f.mu.Unlock()
		close(c.done)
	}()
	c.ov, c.err = fn()
	return c.ov, c.err
}
