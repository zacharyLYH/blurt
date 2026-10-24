// Command blurt is the Blurt backend. Startup is strict on purpose: every
// required setting is validated before anything listens, and any gap panics
// so a misconfigured deploy dies immediately instead of running half-alive.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"blurt/internal/auth"
	"blurt/internal/config"
	"blurt/internal/db"
	"blurt/internal/httpapi"
	"blurt/internal/mail"
	"blurt/internal/push"
	"blurt/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}

	handle, err := db.Open(cfg.DataDir)
	if err != nil {
		panic(err)
	}
	defer handle.Close()

	// Push and mail are the two places settings could look fine but not be:
	// the VAPID identity is logged by name only, and a bad SMTP pair only
	// shows up when a PIN is mailed.
	slog.Info("configuration loaded",
		"data_dir", cfg.DataDir,
		"bind", cfg.Bind,
		"public_origins", cfg.PublicOrigins,
		"smtp_host", cfg.SMTP.Host,
		"smtp_port", cfg.SMTP.Port,
		"smtp_from", cfg.SMTP.From,
		"vapid_subject", cfg.VAPID.Subject,
	)

	// BLURT_FAKE_PIN is the dev way to sign in without a mailbox: the PIN is
	// fixed, so nothing is mailed and no SMTP settings are needed at all.
	fixedPIN := ""
	mailer := (httpapi.PINMailer)(nil)
	if cfg.FakePIN {
		slog.Warn("BLURT_FAKE_PIN is on: login accepts the fixed dev PIN and no mail is sent — never do this on a public host")
		fixedPIN = auth.DevPIN
	} else {
		sender, err := mail.New(cfg.SMTP)
		if err != nil {
			panic(err)
		}
		mailer = sender
	}

	sessions := store.NewSessionStore(handle.Write, handle.Read)
	srv := &http.Server{
		Addr: cfg.Bind,
		Handler: httpapi.New(httpapi.Deps{
			DB:             handle.Write,
			Users:          store.NewUserStore(handle.Write, handle.Read),
			Sessions:       sessions,
			Pins:           auth.NewPinStore(5*time.Minute, fixedPIN),
			Mailer:         mailer,
			Pusher:         &push.Notifier{Sessions: sessions, Sender: push.NewSender(cfg.VAPID)},
			VAPIDPublicKey: cfg.VAPID.PublicKey,
			AllowedOrigins: cfg.PublicOrigins,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", cfg.Bind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
}
