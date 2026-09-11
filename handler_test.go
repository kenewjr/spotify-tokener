package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func testToken(value string, expiry time.Time) []byte {
	body, _ := json.Marshal(map[string]any{"accessToken": value, "accessTokenExpirationTimestampMs": expiry.UnixMilli()})
	return body
}

func TestTokenValidation(t *testing.T) {
	valid := testToken("fresh", time.Now().Add(time.Hour))
	for name, body := range map[string][]byte{
		"empty": {}, "missing": []byte(`{}`), "html": []byte(`<html>blocked</html>`),
		"null": []byte(`null`), "type": []byte(`{"accessToken":42}`),
		"expired":      testToken("expired", time.Now().Add(-time.Second)),
		"near expiry":  testToken("old", time.Now().Add(time.Second)),
		"blank":        testToken("  ", time.Now().Add(time.Hour)),
		"oversize":     []byte(strings.Repeat("x", 1<<20+1)),
		"string error": []byte(strings.TrimSuffix(string(valid), "}") + `,"error":"access_denied"}`),
		"object error": []byte(strings.TrimSuffix(string(valid), "}") + `,"error":{"code":403}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateToken(body, true); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	if _, err := validateToken(valid, true); err != nil {
		t.Fatal(err)
	}
	nullError := []byte(strings.TrimSuffix(string(valid), "}") + `,"error":null}`)
	if _, err := validateToken(nullError, true); err != nil {
		t.Fatal("valid token with null error rejected:", err)
	}
	account := []byte(strings.TrimSuffix(string(valid), "}") + `,"isAnonymous":false}`)
	if _, err := validateToken(account, true); err == nil {
		t.Fatal("account token cached as anonymous")
	}
	if _, err := validateToken(account, false); err != nil {
		t.Fatal(err)
	}
}

func TestTokenURL(t *testing.T) {
	for _, raw := range []string{spotifyURL + "/api/token?reason=transport", spotifyURL + "/get_access_token"} {
		if !isTokenURL(raw, spotifyURL) {
			t.Fatal("valid token URL rejected")
		}
	}
	for _, raw := range []string{
		spotifyURL + "/api/token-evil", spotifyURL + "/api/token/extra", "http://open.spotify.com/api/token",
		"https://open.spotify.com.evil/api/token", "https://user@open.spotify.com/api/token", "%invalid",
	} {
		if isTokenURL(raw, spotifyURL) {
			t.Fatal("unexpected token URL accepted")
		}
	}
}

func beginToken(listener func(any), id network.RequestID, status int64) {
	listener(&network.EventRequestWillBeSent{RequestID: id, Request: &network.Request{URL: spotifyURL + "/api/token", Method: "GET"}})
	listener(&network.EventResponseReceived{RequestID: id, Response: &network.Response{URL: spotifyURL + "/api/token", Status: status}})
}

func TestTokenListener(t *testing.T) {
	ready := make(chan tokenResponse, 1)
	listener := tokenListener(spotifyURL, ready)
	beginToken(listener, "token", 200)
	if len(ready) != 0 {
		t.Fatal("body requested before LoadingFinished")
	}
	listener(&network.EventLoadingFinished{RequestID: "unrelated"})
	listener(&network.EventLoadingFinished{RequestID: "token"})
	for i := 0; i < 100; i++ {
		beginToken(listener, "duplicate", 200)
		listener(&network.EventLoadingFinished{RequestID: "duplicate"})
	}
	if len(ready) != 1 {
		t.Fatal("completion not delivered exactly once")
	}
	if result := <-ready; result.id != "token" || result.status != 200 || result.err != nil {
		t.Fatal("wrong completed response")
	}
	// A full output channel cannot block the CDP dispatcher.
	ready <- tokenResponse{}
	listener = tokenListener(spotifyURL, ready)
	beginToken(listener, "full", 200)
	listener(&network.EventLoadingFinished{RequestID: "full"})
	<-ready
	listener = tokenListener(spotifyURL, ready)
	beginToken(listener, "failed", 503)
	listener(&network.EventLoadingFailed{RequestID: "failed"})
	if result := <-ready; result.err == nil {
		t.Fatal("network failure lost")
	}
}

func TestAnonymousCacheAndCookies(t *testing.T) {
	s := newServer(context.Background())
	calls := 0
	s.fetch = func(ctx context.Context, cookies []*network.CookieParam) ([]byte, error) {
		calls++
		value := "anonymous"
		if len(cookies) > 0 {
			value = cookies[0].Value
		}
		return testToken(value, time.Now().Add(time.Hour)), nil
	}
	for _, account := range []string{"", "", "account-a", "account-b", ""} {
		var cookies []*network.CookieParam
		if account != "" {
			cookies = []*network.CookieParam{{Name: "sp_dc", Value: account, URL: spotifyURL}}
		}
		payload, err := s.getToken(context.Background(), cookies)
		if err != nil {
			t.Fatal(err)
		}
		want := account
		if want == "" {
			want = "anonymous"
		}
		if !strings.Contains(string(payload.body), want) {
			t.Fatal("cookie request mixed with anonymous cache")
		}
	}
	if calls != 3 {
		t.Fatalf("fetch calls = %d, want 3", calls)
	}
}

func TestRefreshFallbackAndExpiry(t *testing.T) {
	s := newServer(context.Background())
	s.cached = tokenPayload{body: testToken("cached", time.Now().Add(45*time.Second)), expires: time.Now().Add(45 * time.Second)}
	calls := 0
	s.fetch = func(context.Context, []*network.CookieParam) ([]byte, error) {
		calls++
		return nil, errors.New("upstream failure")
	}
	for i := 0; i < 2; i++ {
		if p, err := s.getToken(context.Background(), nil); err != nil || !p.usable() {
			t.Fatal("valid cache not used")
		}
	}
	if calls != 1 {
		t.Fatal("failed proactive refresh hammered upstream")
	}
	s.cached.expires = time.Now().Add(-time.Second)
	if _, err := s.getToken(context.Background(), nil); err == nil {
		t.Fatal("expired fallback served")
	}
	s.fetch = func(context.Context, []*network.CookieParam) ([]byte, error) {
		calls++
		return testToken("renewed", time.Now().Add(time.Hour)), nil
	}
	if _, err := s.getToken(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("subsequent request did not recover")
	}
}

func waitForWaiters(t *testing.T, s *server, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		ready := s.flight != nil && s.flight.waiters == n
		s.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("waiters did not join refresh")
		}
	}
}

func TestConcurrentRefreshAndCancellation(t *testing.T) {
	s := newServer(context.Background())
	var calls atomic.Int32
	release := make(chan struct{})
	s.fetch = func(ctx context.Context, _ []*network.CookieParam) ([]byte, error) {
		calls.Add(1)
		select {
		case <-release:
			return testToken("shared", time.Now().Add(time.Hour)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan error, 1)
	go func() { _, err := s.getToken(ctx, nil); cancelled <- err }()
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() { _, err := s.getToken(context.Background(), nil); results <- err }()
	}
	waitForWaiters(t, s, 17)
	cancel()
	if !errors.Is(<-cancelled, context.Canceled) {
		t.Fatal("cancelled client still waiting")
	}
	close(release)
	for i := 0; i < 16; i++ {
		if err := <-results; err != nil {
			t.Fatal("cancelled client killed shared refresh:", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("anonymous requests not coalesced")
	}
}

func TestLastWaiterCancelsBrowserWork(t *testing.T) {
	s := newServer(context.Background())
	stopped := make(chan struct{})
	s.fetch = func(ctx context.Context, _ []*network.CookieParam) ([]byte, error) {
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := s.getToken(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline not returned")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("abandoned browser job leaked")
	}
}

func TestChromeConcurrencyLimit(t *testing.T) {
	s := newServer(context.Background())
	started, release := make(chan struct{}, 2), make(chan struct{})
	s.fetch = func(ctx context.Context, _ []*network.CookieParam) ([]byte, error) {
		started <- struct{}{}
		select {
		case <-release:
			return testToken("account", time.Now().Add(time.Hour)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	cookies := []*network.CookieParam{{Name: "sp_dc", Value: "account"}}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.getToken(context.Background(), cookies) }()
		<-started
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.getToken(ctx, cookies); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("queue ignored deadline")
	}
	if len(started) != 0 {
		t.Fatal("more than two Chrome jobs started")
	}
	close(release)
	wg.Wait()
}

func TestHTTPTokenResponses(t *testing.T) {
	for _, tt := range []struct {
		name, method, cookie string
		body                 []byte
		err                  error
		status               int
	}{
		{"success", "GET", "", testToken("valid", time.Now().Add(time.Hour)), nil, 200},
		{"method", "POST", "", nil, nil, 405},
		{"cookie", "GET", `sp_dc="unterminated`, nil, nil, 400},
		{"cookie invalid sep", "GET", `sp_dc=valid; =malformed`, nil, nil, 400},
		{"invalid", "GET", "", []byte(`{"error":"secret-body"}`), nil, 502},
		{"timeout", "GET", "", nil, context.DeadlineExceeded, 504},
		{"unavailable", "GET", "", nil, context.Canceled, 503},
		{"upstream", "GET", "", nil, errors.New("secret-error"), 502},
		{"upstream status", "GET", "", nil, tokenStatusError(500), 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(context.Background())
			s.fetch = func(context.Context, []*network.CookieParam) ([]byte, error) { return tt.body, tt.err }
			r := httptest.NewRequest(tt.method, "/api/token", nil)
			if tt.cookie != "" {
				r.Header.Set("Cookie", tt.cookie)
			}
			w := httptest.NewRecorder()
			s.handleToken(w, r)
			if w.Code != tt.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d cache=%q", w.Code, w.Header().Get("Cache-Control"))
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("upstream data leaked")
			}
		})
	}
}

func TestBrowserTokenCapture(t *testing.T) {
	chrome := os.Getenv("SPOTIFY_TOKENER_TEST_CHROME")
	if chrome == "" {
		t.Skip("set SPOTIFY_TOKENER_TEST_CHROME for isolated browser fixture")
	}
	opts := append([]chromedp.ExecAllocatorOption(nil), chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.ExecPath(chrome))
	alloc, stop := chromedp.NewExecAllocator(context.Background(), opts...)
	defer stop()
	browser, closeBrowser := chromedp.NewContext(alloc, chromedp.WithErrorf(func(string, ...any) {}))
	defer closeBrowser()
	startup := time.AfterFunc(20*time.Second, closeBrowser)
	err := chromedp.Run(browser)
	startup.Stop()
	if err != nil {
		t.Fatal(err)
	}
	var mode atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = fmt.Fprint(w, `<script>fetch('/api/token').then(r=>r.json());</script>`)
			return
		}
		if r.URL.Path != "/api/token" {
			http.NotFound(w, r)
			return
		}
		if mode.Load() == 1 {
			http.Error(w, "upstream-secret", 429)
			return
		}
		if mode.Load() == 2 {
			<-r.Context().Done()
			return
		}
		value := "anonymous"
		if c, err := r.Cookie("sp_dc"); err == nil {
			value = c.Value
		}
		body := testToken(value, time.Now().Add(time.Hour))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		select {
		case <-time.After(80 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(body[len(body)/2:])
	}))
	defer fixture.Close()
	for _, value := range []string{"account-a", "account-b", "anonymous"} {
		var cookies []*network.CookieParam
		if value != "anonymous" {
			cookies = []*network.CookieParam{{Name: "sp_dc", Value: value, URL: fixture.URL}}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		body, err := getAccessTokenPayload(ctx, browser, fixture.URL, cookies)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateToken(body, false); err != nil || !strings.Contains(string(body), value) {
			t.Fatal("partial response or leaked browser cookies")
		}
	}
	mode.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = getAccessTokenPayload(ctx, browser, fixture.URL, nil)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "upstream-secret") {
		t.Fatal("upstream status not preserved safely")
	}
	mode.Store(2)
	ctx, cancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
	_, err = getAccessTokenPayload(ctx, browser, fixture.URL, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("browser request ignored deadline:", err)
	}
}
