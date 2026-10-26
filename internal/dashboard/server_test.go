package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// startTest serves f and returns the server, its URL with the key, and its
// base address.
func startTest(t *testing.T, p Provider) (*Server, string, string) {
	t.Helper()
	s := NewServer(p, ServerOptions{})
	u, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname() != "127.0.0.1" || parsed.Path != "/" || parsed.Query().Get("t") == "" {
		t.Fatalf("unexpected dashboard URL %q", u)
	}
	return s, u, "http://" + parsed.Host
}

// noRedirect is a client that reports redirects instead of following them.
var noRedirect = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func do(t *testing.T, method, u string, header map[string]string, cookie *http.Cookie) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

// login trades the URL's key for the session cookie.
func login(t *testing.T, u string) *http.Cookie {
	t.Helper()
	resp, _ := do(t, http.MethodGet, u, nil, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("key visit: status %d, Location %q; want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("key visit set %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie %+v: want HttpOnly, SameSite=Strict, Path=/", c)
	}
	if strings.Contains(u, c.Value) {
		t.Fatal("the cookie reuses the URL key; it should be a separate secret")
	}
	return c
}

func TestServerOnlyListensOnLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.0.2.1:0", ":0", "example.com:0"} {
		if _, err := NewServer(NewFake(time.Now()), ServerOptions{Addr: addr}).Start(); err == nil {
			t.Errorf("Start(%q) succeeded; want a loopback-only error", addr)
		}
	}
	s := NewServer(NewFake(time.Now()), ServerOptions{Addr: "localhost:0"})
	u, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("localhost URL = %q, want 127.0.0.1", u)
	}
	if _, err := s.Start(); err == nil {
		t.Fatal("a second Start succeeded")
	}
}

func TestServerAuth(t *testing.T) {
	f := NewFake(time.Now())
	_, u, base := startTest(t, f)

	// No key: a page that says how to get in, or a JSON error for the API.
	resp, body := do(t, http.MethodGet, base+"/", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "stradl dashboard") {
		t.Fatalf("GET / without key: %d %q", resp.StatusCode, body)
	}
	// That page's inline style is allowed by its hash, and only it.
	style := regexp.MustCompile(`(?s)<style>(.*)</style>`).FindStringSubmatch(body)
	if style == nil {
		t.Fatal("the 401 page has no style")
	}
	sum := sha256.Sum256([]byte(style[1]))
	if p := resp.Header.Get("Content-Security-Policy"); !strings.Contains(p, "style-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") ||
		strings.Contains(p, "unsafe-inline") || !strings.Contains(p, "default-src 'self'") || !strings.Contains(p, "script-src 'self'") {
		t.Errorf("401 page CSP %q does not allow exactly its own style", p)
	}
	for _, path := range []string{"/api/overview", "/api/sessions/x", "/static/app.js"} {
		if resp, _ := do(t, http.MethodGet, base+path, nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without key: %d, want 401", path, resp.StatusCode)
		}
	}
	resp, body = do(t, http.MethodGet, base+"/api/overview", nil, nil)
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") || !strings.Contains(body, `"error"`) {
		t.Errorf("API 401 is not a JSON error: %s %q", resp.Header.Get("Content-Type"), body)
	}
	if resp, _ := do(t, http.MethodGet, base+"/?t=wrong", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong key: %d, want 401", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, base+"/", nil, &http.Cookie{Name: "stradl_" + strings.TrimPrefix(base, "http://127.0.0.1:"), Value: "forged"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged cookie: %d, want 401", resp.StatusCode)
	}

	// The key works for the API directly too (curl), without a redirect.
	key := strings.SplitN(u, "?t=", 2)[1]
	if resp, _ := do(t, http.MethodGet, base+"/api/overview?t="+key, nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("API with ?t=: %d, want 200", resp.StatusCode)
	}

	c := login(t, u)
	resp, body = do(t, http.MethodGet, base+"/", nil, c)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<script src="/static/app.js"`) {
		t.Fatalf("GET / with cookie: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, base+"/api/overview", nil, c); resp.StatusCode != http.StatusOK {
		t.Fatalf("overview with cookie: %d", resp.StatusCode)
	}

	// DNS rebinding: a page on another name that resolves here gets nothing.
	port := strings.TrimPrefix(base, "http://127.0.0.1:")
	for _, host := range []string{"evil.example:" + port, "evil.example", "127.0.0.1:1", "0.0.0.0:" + port} {
		if resp, _ := do(t, http.MethodGet, base+"/api/overview", map[string]string{"Host": host}, c); resp.StatusCode != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403", host, resp.StatusCode)
		}
	}
	if resp, _ := do(t, http.MethodGet, base+"/api/overview", map[string]string{"Host": "localhost:" + port}, c); resp.StatusCode != http.StatusOK {
		t.Errorf("Host localhost: %d, want 200", resp.StatusCode)
	}

	// CSRF: a POST needs to come from the dashboard's own page.
	for name, h := range map[string]map[string]string{
		"no origin":      nil,
		"other origin":   {"Origin": "http://evil.example"},
		"null origin":    {"Origin": "null"},
		"other port":     {"Origin": "http://127.0.0.1:1"},
		"other referer":  {"Referer": "http://evil.example/page"},
		"https referrer": {"Referer": "https://127.0.0.1:" + port + "/"},
	} {
		if resp, _ := do(t, http.MethodPost, base+"/api/sync", h, c); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST with %s: %d, want 403", name, resp.StatusCode)
		}
	}
	if f.SyncCalls() != 0 {
		t.Fatalf("refused POSTs reached the provider %d times", f.SyncCalls())
	}
	if resp, _ := do(t, http.MethodPost, base+"/api/sync", map[string]string{"Origin": base}, c); resp.StatusCode != http.StatusAccepted {
		t.Errorf("same-origin POST: %d, want 202", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPost, base+"/api/sync", map[string]string{"Referer": base + "/"}, c); resp.StatusCode != http.StatusAccepted {
		t.Errorf("same-origin POST by Referer: %d, want 202", resp.StatusCode)
	}
	if f.SyncCalls() != 2 {
		t.Fatalf("SyncNow calls = %d, want 2", f.SyncCalls())
	}
	// Without the cookie, even a same-origin POST is refused.
	if resp, _ := do(t, http.MethodPost, base+"/api/sync", map[string]string{"Origin": base}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST without cookie: %d, want 401", resp.StatusCode)
	}
}

func TestServerHeaders(t *testing.T) {
	_, u, base := startTest(t, NewFake(time.Now()))
	c := login(t, u)
	for _, path := range []string{"/", "/api/overview", "/static/app.css", "/nope"} {
		resp, _ := do(t, http.MethodGet, base+path, nil, c)
		h := resp.Header
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing nosniff or no-referrer", path)
		}
		for k := range h {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("%s: CORS header %s", path, k)
			}
		}
		if strings.HasPrefix(path, "/api/") && h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", path, h.Get("Cache-Control"))
		}
	}
}

func TestServerHandlers(t *testing.T) {
	f := NewFake(time.Now())
	_, u, base := startTest(t, f)
	c := login(t, u)

	resp, body := do(t, http.MethodGet, base+"/api/overview", nil, c)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("overview: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var ov Overview
	if err := json.Unmarshal([]byte(body), &ov); err != nil {
		t.Fatal(err)
	}
	if len(ov.Devices) != 3 || !ov.Devices[0].This || len(ov.Days) != 7 || len(ov.Sessions) == 0 || ov.Totals.Sessions != len(ov.Sessions) {
		t.Fatalf("overview shape: %d devices, %d days, %d sessions", len(ov.Devices), len(ov.Days), len(ov.Sessions))
	}
	// The field names the page reads.
	var raw map[string]any
	_ = json.Unmarshal([]byte(body), &raw)
	for _, k := range []string{"generatedAt", "thisDevice", "agent", "devices", "totals", "days", "sessions", "pending"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("overview lacks %q", k)
		}
	}

	id := ov.Sessions[0].ID
	resp, body = do(t, http.MethodGet, base+"/api/sessions/"+id, nil, c)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session: %d %s", resp.StatusCode, body)
	}
	var d SessionDetail
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != id || len(d.Timeline) == 0 || d.Timeline[0].Kind != "prompt" {
		t.Fatalf("session detail: id %q, %d timeline entries", d.ID, len(d.Timeline))
	}

	for _, path := range []string{"/api/sessions/does-not-exist", "/api/sessions/..", "/api/sessions/a%2Fb", "/api/sessions/" + strings.Repeat("a", 200)} {
		resp, body := do(t, http.MethodGet, base+path, nil, c)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"error"`) {
			t.Errorf("GET %s: %d %q, want a 404 JSON error", path, resp.StatusCode, body)
		}
	}

	resp, body = do(t, http.MethodPost, base+"/api/sync", map[string]string{"Origin": base}, c)
	if resp.StatusCode != http.StatusAccepted || f.SyncCalls() != 1 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("sync: %d %q, %d calls", resp.StatusCode, body, f.SyncCalls())
	}
	// The pretend sync shows as running, then finished.
	resp, body = do(t, http.MethodGet, base+"/api/overview", nil, c)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"syncing":true`) {
		t.Errorf("overview right after a sync: %q, want syncing", body[:min(len(body), 200)])
	}
	f.SyncErr = errors.New("not set up: run stradl init")
	resp, body = do(t, http.MethodPost, base+"/api/sync", map[string]string{"Origin": base}, c)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "stradl init") {
		t.Errorf("failing sync: %d %q", resp.StatusCode, body)
	}
	// Known paths with the wrong method: a JSON 405 naming the right one.
	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodGet, "/api/sync", "POST"},
		{http.MethodPost, "/api/overview", "GET, HEAD"},
		{http.MethodDelete, "/api/sessions/" + id, "GET, HEAD"},
		{http.MethodPost, "/", "GET, HEAD"},
	} {
		resp, body := do(t, tc.method, base+tc.path, map[string]string{"Origin": base}, c)
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != tc.allow || !strings.Contains(body, `"error"`) {
			t.Errorf("%s %s: %d Allow %q %q, want a 405 allowing %s", tc.method, tc.path, resp.StatusCode, resp.Header.Get("Allow"), body, tc.allow)
		}
	}

	for path, want := range map[string]int{
		"/static/app.css":    http.StatusOK,
		"/static/app.js":     http.StatusOK,
		"/static/mark.svg":   http.StatusOK,
		"/static":            http.StatusNotFound,
		"/static/":           http.StatusNotFound,
		"/static/index.html": http.StatusNotFound,
		"/static/nope.js":    http.StatusNotFound,
		"/static/../api/x":   http.StatusNotFound,
		"//evil.example/":    http.StatusNotFound,
		"/nope":              http.StatusNotFound,
		"/index.html":        http.StatusNotFound,
	} {
		resp, body := do(t, http.MethodGet, base+path, nil, c)
		if resp.StatusCode != want || resp.Header.Get("Location") != "" {
			t.Errorf("GET %s: %d (Location %q), want %d", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
		if want == http.StatusNotFound && !strings.Contains(body, `"error"`) {
			t.Errorf("GET %s: %q, want a JSON error", path, body)
		}
	}
	// The key never turns into a redirect off this server.
	key := strings.SplitN(u, "?t=", 2)[1]
	if resp, _ := do(t, http.MethodGet, base+"//evil.example/?t="+key, nil, nil); resp.Header.Get("Location") != "" {
		t.Errorf("key visit to //evil.example redirected to %q", resp.Header.Get("Location"))
	}
	resp, _ = do(t, http.MethodGet, base+"/static/app.js", nil, c)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type %q", ct)
	}
}

type failingProvider struct{ *Fake }

func (failingProvider) Overview(context.Context) (*Overview, error) {
	return nil, errors.New("cannot read ~/.claude")
}

func TestServerProviderError(t *testing.T) {
	_, u, base := startTest(t, &failingProvider{})
	c := login(t, u)
	resp, body := do(t, http.MethodGet, base+"/api/overview", nil, c)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "cannot read") {
		t.Fatalf("provider error: %d %q", resp.StatusCode, body)
	}
}

// slowProvider counts Overview calls and takes a moment over each.
type slowProvider struct {
	Fake
	calls chan struct{}
}

func (p *slowProvider) Overview(ctx context.Context) (*Overview, error) {
	p.calls <- struct{}{}
	time.Sleep(150 * time.Millisecond)
	return &Overview{GeneratedAt: time.Now()}, nil
}

// Two tabs refreshing together share one read of everything.
func TestServerSharesConcurrentOverviews(t *testing.T) {
	p := &slowProvider{calls: make(chan struct{}, 10)}
	_, u, base := startTest(t, p)
	c := login(t, u)
	done := make(chan int, 3)
	for range 3 {
		go func() {
			resp, _ := do(t, http.MethodGet, base+"/api/overview", nil, c)
			done <- resp.StatusCode
		}()
	}
	for range 3 {
		if code := <-done; code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	}
	if n := len(p.calls); n >= 3 {
		t.Fatalf("%d provider calls for 3 concurrent requests; want them shared", n)
	}
}

func TestServerClose(t *testing.T) {
	s, u, _ := startTest(t, NewFake(time.Now()))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := noRedirect.Get(u); err == nil {
		t.Fatal("server still answering after Close")
	}
	if err := NewServer(NewFake(time.Now()), ServerOptions{}).Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
}

// The page must work offline and under the CSP: nothing from the network,
// no inline script, style or event handlers.
func TestWebAssetsSelfContained(t *testing.T) {
	urlRE := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}[^\s"'<>)]*`)
	allowed := map[string]bool{"http://www.w3.org/2000/svg": true}
	err := fs.WalkDir(webFS, "web", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := webFS.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, m := range urlRE.FindAllString(text, -1) {
			if !allowed[m] {
				t.Errorf("%s references %q; the dashboard must not load anything from the network", path, m)
			}
		}
		if strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".js") {
			for _, bad := range []*regexp.Regexp{
				regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`), // inline script body
				regexp.MustCompile(`\sstyle\s*=\s*["']`),            // style attribute
				regexp.MustCompile(`\son[a-z]+\s*=\s*["']`),         // inline event handler
				regexp.MustCompile(`<style`),
				regexp.MustCompile(`\.innerHTML\s*=|insertAdjacentHTML|document\.write`),
				regexp.MustCompile(`\beval\(|new Function\(`),
			} {
				if loc := bad.FindStringIndex(text); loc != nil {
					t.Errorf("%s: %q is blocked by the CSP or unsafe", path, text[loc[0]:min(loc[1]+20, len(text))])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// index.html is well formed, and every element app.js looks up by ID exists.
func TestIndexHTML(t *testing.T) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity
	var stack []string
	ids := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("index.html: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			stack = append(stack, el.Name.Local)
			for _, a := range el.Attr {
				if a.Name.Local == "id" {
					if ids[a.Value] {
						t.Errorf("duplicate id %q", a.Value)
					}
					ids[a.Value] = true
				}
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != el.Name.Local {
				t.Fatalf("index.html: unexpected </%s> (open: %v)", el.Name.Local, stack)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		t.Fatalf("index.html: unclosed elements %v", stack)
	}

	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	ref := regexp.MustCompile(`\$\('#([\w-]+)'|getElementById\('([\w-]+)'\)|href', '#' \+ id|#(i-[\w-]+)`)
	for _, m := range ref.FindAllStringSubmatch(string(js), -1) {
		for _, id := range m[1:] {
			if id != "" && !ids[id] {
				t.Errorf("app.js uses #%s, which index.html lacks", id)
			}
		}
	}
	icon := regexp.MustCompile(`icon\('(i-[\w-]+)'|icon: '(i-[\w-]+)'`)
	for _, m := range icon.FindAllStringSubmatch(string(js), -1) {
		for _, id := range m[1:] {
			if id != "" && !ids[id] {
				t.Errorf("app.js uses icon %s, which the sprite in index.html lacks", id)
			}
		}
	}
}

func TestFakeIsConsistent(t *testing.T) {
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.Local)
	f := NewFake(now)
	ov, err := f.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]bool{}
	for i, s := range ov.Sessions {
		states[s.SyncState] = true
		if i > 0 && s.LastActivityAt.After(ov.Sessions[i-1].LastActivityAt) {
			t.Errorf("sessions not newest first at %d", i)
		}
		if _, err := f.Session(context.Background(), s.ID); err != nil {
			t.Errorf("no detail for %s", s.ID)
		}
		if s.LastActivityAt.After(now) {
			t.Errorf("%q ends in the future", s.Title)
		}
	}
	for _, st := range []string{"synced", "pending-upload", "pending-download", "merging", "held", "conflict", "not-synced"} {
		if !states[st] {
			t.Errorf("sample data lacks a %s session", st)
		}
	}
	if ov.Days[0].Label != "Today" || ov.Days[1].Label != "Yesterday" || ov.Days[0].Date != "2026-10-25" {
		t.Errorf("days start %q %q %q", ov.Days[0].Label, ov.Days[1].Label, ov.Days[0].Date)
	}
	empty := 0
	for _, d := range ov.Days {
		if len(d.Sessions) == 0 {
			empty++
		}
		for _, ds := range d.Sessions {
			if len(ds.Prompts) > 3 || len(ds.FilesTouched) > 8 {
				t.Errorf("day session %q exceeds the contract's limits", ds.Title)
			}
		}
	}
	if empty == 0 {
		t.Error("sample week has no empty day")
	}
	if _, err := f.Session(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
}

// Today's sample sessions stay in the past and on today's date at any hour,
// so a demo just after midnight still makes sense.
func TestFakeTodayStaysToday(t *testing.T) {
	day0 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	for _, hm := range [][2]int{{0, 3}, {0, 40}, {7, 0}, {12, 0}, {23, 59}} {
		now := day0.Add(time.Duration(hm[0])*time.Hour + time.Duration(hm[1])*time.Minute)
		ov, err := NewFake(now).Overview(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(ov.Days[0].Sessions) == 0 {
			t.Errorf("at %02d:%02d, nothing today", hm[0], hm[1])
		}
		for _, ds := range ov.Days[0].Sessions {
			if ds.FirstAt.Before(day0) || ds.LastAt.After(now) || ds.LastAt.Before(ds.FirstAt) {
				t.Errorf("at %02d:%02d, %q runs %v to %v, outside today so far", hm[0], hm[1], ds.Title, ds.FirstAt, ds.LastAt)
			}
		}
	}
}
