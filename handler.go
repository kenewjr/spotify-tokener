package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type tokenPayload struct {
	body    []byte
	expires time.Time
}

func (p tokenPayload) usable() bool {
	return len(p.body) > 0 && p.expires.After(time.Now().Add(5*time.Second))
}

type tokenFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	payload tokenPayload
	err     error
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/api/token" {
		http.NotFound(w, r)
		return
	}
	var cookies []*network.CookieParam
	// Invalid cookie input must not silently turn an account request into anonymous access.
	for _, header := range r.Header.Values("Cookie") {
		parsed, err := http.ParseCookie(header)
		if err != nil {
			http.Error(w, "invalid cookies", http.StatusBadRequest)
			return
		}
		for _, cookie := range parsed {
			cookies = append(cookies, &network.CookieParam{
				Name: cookie.Name, Value: cookie.Value, URL: spotifyURL,
			})
		}
	}

	payload, err := s.getToken(r.Context(), cookies)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		} else if errors.Is(err, context.Canceled) {
			status = http.StatusServiceUnavailable
		}
		var upstream tokenStatusError
		if errors.As(err, &upstream) {
			slog.Warn("Spotify token endpoint failed", slog.Int64("upstreamStatus", int64(upstream)))
		}
		// Log classifications only; browser errors may contain cookie or token data.
		slog.Warn("Token request failed", slog.Int("status", status))
		http.Error(w, "Spotify token unavailable", status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(payload.body); err != nil {
		slog.Debug("Token response client disconnected")
	}
}

func (s *server) getToken(ctx context.Context, cookies []*network.CookieParam) (tokenPayload, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return tokenPayload{}, err
	}
	if len(cookies) > 0 {
		return s.fetchToken(ctx, cookies)
	}

	s.mu.Lock()
	if s.cached.usable() && time.Now().Before(s.refreshAfter) {
		cached := s.cached
		s.mu.Unlock()
		return cached, nil
	}
	f := s.flight
	if f == nil {
		refreshCtx, stop := context.WithTimeout(s.ctx, tokenTimeout)
		f = &tokenFlight{done: make(chan struct{}), cancel: stop}
		s.flight = f
		go s.refreshToken(refreshCtx, f)
	}
	f.waiters++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		f.waiters--
		if f.waiters == 0 && s.flight == f {
			s.flight = nil
			f.cancel()
		}
	}()

	select {
	case <-ctx.Done():
		return tokenPayload{}, ctx.Err()
	case <-f.done:
		if err := ctx.Err(); err != nil {
			return tokenPayload{}, err
		}
		if f.err == nil && !f.payload.usable() {
			return tokenPayload{}, errors.New("token expired while waiting")
		}
		return f.payload, f.err
	}
}

func (s *server) refreshToken(ctx context.Context, f *tokenFlight) {
	defer f.cancel()
	started := time.Now()
	payload, err := s.fetchToken(ctx, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flight == f {
		if err == nil {
			s.cached = payload
			s.refreshAfter = payload.expires.Add(-time.Minute)
		} else if s.cached.usable() {
			payload, err = s.cached, nil
			s.refreshAfter = time.Now().Add(5 * time.Second)
			slog.Warn("Token refresh failed; using unexpired anonymous cache")
		}
		s.flight = nil
	}
	f.payload, f.err = payload, err
	close(f.done)
	slog.Info("Anonymous token refresh completed", slog.Bool("usable", err == nil), slog.Int64("durationMs", time.Since(started).Milliseconds()))
}

func (s *server) fetchToken(ctx context.Context, cookies []*network.CookieParam) (tokenPayload, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return tokenPayload{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return tokenPayload{}, err
	}
	body, err := s.fetch(ctx, cookies)
	if err != nil {
		return tokenPayload{}, err
	}
	if err := ctx.Err(); err != nil {
		return tokenPayload{}, err
	}
	return validateToken(body, len(cookies) == 0)
}

func validateToken(body []byte, anonymous bool) (tokenPayload, error) {
	var token struct {
		AccessToken string          `json:"accessToken"`
		Expiry      int64           `json:"accessTokenExpirationTimestampMs"`
		Anonymous   *bool           `json:"isAnonymous"`
		Error       json.RawMessage `json:"error"`
	}
	if len(body) > 1<<20 || json.Unmarshal(body, &token) != nil || strings.TrimSpace(token.AccessToken) == "" || (len(token.Error) > 0 && string(token.Error) != "null") {
		return tokenPayload{}, errors.New("invalid token payload")
	}
	if anonymous && token.Anonymous != nil && !*token.Anonymous {
		return tokenPayload{}, errors.New("account token refused for anonymous request")
	}
	payload := tokenPayload{body: body, expires: time.UnixMilli(token.Expiry)}
	if !payload.usable() {
		return tokenPayload{}, errors.New("token missing expiry or nearing expiration")
	}
	return payload, nil
}

type tokenStatusError int64

func (s tokenStatusError) Error() string {
	return fmt.Sprintf("Spotify token endpoint returned HTTP %d", s)
}

type tokenResponse struct {
	id     network.RequestID
	status int64
	err    error
}

func isTokenURL(raw, origin string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Scheme+"://"+u.Host != origin {
		return false
	}
	return u.Path == "/api/token" || u.Path == "/get_access_token"
}

// ListenTarget callbacks run synchronously. Never block them or close their output channel.
func tokenListener(origin string, ready chan<- tokenResponse) func(any) {
	pending := make(map[network.RequestID]tokenResponse)
	complete := false
	return func(event any) {
		if complete {
			return
		}
		var response tokenResponse
		switch ev := event.(type) {
		case *network.EventRequestWillBeSent:
			if ev.Request != nil && ev.Request.Method == http.MethodGet && isTokenURL(ev.Request.URL, origin) {
				pending[ev.RequestID] = tokenResponse{id: ev.RequestID}
			}
			return
		case *network.EventResponseReceived:
			if ev.Response != nil && isTokenURL(ev.Response.URL, origin) {
				if r, ok := pending[ev.RequestID]; ok {
					r.status = ev.Response.Status
					pending[ev.RequestID] = r
				}
			}
			return
		case *network.EventLoadingFinished:
			var ok bool
			response, ok = pending[ev.RequestID]
			if !ok {
				return
			}
		case *network.EventLoadingFailed:
			var ok bool
			response, ok = pending[ev.RequestID]
			if !ok {
				return
			}
			response.err = errors.New("token network request failed")
		default:
			return
		}
		complete = true
		select {
		case ready <- response:
		default:
		}
	}
}

func getAccessTokenPayload(rCtx, browserCtx context.Context, origin string, cookies []*network.CookieParam) ([]byte, error) {
	ctx, closeTab := chromedp.NewContext(browserCtx, chromedp.WithNewBrowserContext())
	defer closeTab()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(rCtx, cancel)
	defer stop()

	ready := make(chan tokenResponse, 1)
	chromedp.ListenTarget(ctx, tokenListener(origin, ready))
	if err := chromedp.Run(ctx, network.Enable(), chromedp.ActionFunc(func(ctx context.Context) error {
		if len(cookies) > 0 {
			if err := network.SetCookies(cookies).Do(ctx); err != nil {
				return errors.New("failed to set request cookies")
			}
		}
		// Start navigation without waiting for unrelated images/ads to finish loading.
		_, _, navigationError, err := page.Navigate(origin).Do(ctx)
		if err != nil {
			return errors.New("failed to navigate Spotify")
		}
		if navigationError != "" {
			return errors.New("Spotify navigation rejected")
		}
		return nil
	})); err != nil {
		if rCtx.Err() != nil {
			return nil, rCtx.Err()
		}
		return nil, err
	}

	var response tokenResponse
	select {
	case <-ctx.Done():
		if rCtx.Err() != nil {
			return nil, rCtx.Err()
		}
		return nil, ctx.Err()
	case response = <-ready:
	}
	if response.err != nil {
		return nil, response.err
	}
	if response.status != http.StatusOK {
		return nil, tokenStatusError(response.status)
	}
	var body []byte
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		body, err = network.GetResponseBody(response.id).Do(ctx)
		return err
	})); err != nil {
		if rCtx.Err() != nil {
			return nil, rCtx.Err()
		}
		return nil, errors.New("failed to read completed token response")
	}
	return body, nil
}

func (s *server) startWarmer(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			needRefresh := !s.cached.usable() || time.Now().After(s.refreshAfter)
			hasFlight := s.flight != nil
			s.mu.Unlock()

			if needRefresh && !hasFlight {
				slog.Debug("Warmer refreshing anonymous Spotify token in background")
				_, _ = s.getToken(ctx, nil)
			}
		}
	}
}

