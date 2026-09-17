package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	spotifyURL   = "https://open.spotify.com"
	tokenTimeout = 25 * time.Second
)

var (
	addr       = os.Getenv("SPOTIFY_TOKENER_ADDR")
	chromePath = os.Getenv("SPOTIFY_TOKENER_CHROME_PATH")
	logLevel   = os.Getenv("SPOTIFY_TOKENER_LOG_LEVEL")
)

func main() {
	if err := run(); err != nil {
		slog.Error("Server stopped", slog.Any("err", err))
		os.Exit(1)
	}
}

func run() error {
	if addr == "" {
		addr = "0.0.0.0:8080"
	}
	if logLevel != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(logLevel)); err != nil {
			return fmt.Errorf("invalid log level: %w", err)
		}

		slog.SetLogLoggerLevel(level)
		slog.Info("Log level set", slog.String("level", logLevel))
	}

	execAllocatorOptions := append([]chromedp.ExecAllocatorOption(nil), chromedp.DefaultExecAllocatorOptions[:]...)
	if chromePath != "" {
		execAllocatorOptions = append(execAllocatorOptions, chromedp.ExecPath(chromePath))
	}
	// Use Chrome's actual user agent, not a hard-coded obsolete mobile version.
	// execAllocatorOptions = append(execAllocatorOptions, chromedp.Flag("headless", false))

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), execAllocatorOptions...)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(string, ...any) {
		// CDP errors may contain response data; never log token/cookie payloads.
		slog.Warn("Chrome protocol error")
	}))
	defer cancel()

	// A cancelled first Run context also kills Chrome, so stop the timer on success.
	startup := time.AfterFunc(tokenTimeout, cancel)
	err := chromedp.Run(ctx)
	startup.Stop()
	if err != nil {
		return fmt.Errorf("failed to start Chrome: %w", err)
	}

	s := newServer(ctx)
	go s.startWarmer(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", s.handleToken)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		select {
		case <-ctx.Done():
			http.Error(w, "browser unavailable", http.StatusServiceUnavailable)
		case <-chromedp.FromContext(ctx).Browser.LostConnection:
			http.Error(w, "browser unavailable", http.StatusServiceUnavailable)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		}
	})

	s.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      tokenTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := s.server.Shutdown(shutdown); err != nil {
			_ = s.server.Close()
		}
	}()

	sig, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- s.server.Serve(listener) }()
	slog.Info("Server started", slog.String("address", listener.Addr().String()))
	defer slog.Info("Server stopped")
	select {
	case <-sig.Done():
		return nil
	case <-ctx.Done():
		return errors.New("browser stopped")
	case <-chromedp.FromContext(ctx).Browser.LostConnection:
		return errors.New("browser disconnected")
	case err := <-errorsCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type server struct {
	ctx          context.Context
	server       *http.Server
	fetch        func(context.Context, []*network.CookieParam) ([]byte, error)
	slots        chan struct{}
	mu           sync.Mutex
	cached       tokenPayload
	refreshAfter time.Time
	flight       *tokenFlight
}

func newServer(ctx context.Context) *server {
	return &server{
		ctx: ctx,
		// ponytail: two Chrome jobs per process; scale replicas if sustained demand exceeds this.
		slots: make(chan struct{}, 2),
		fetch: func(requestCtx context.Context, cookies []*network.CookieParam) ([]byte, error) {
			return getAccessTokenPayload(requestCtx, ctx, spotifyURL, cookies)
		},
	}
}
