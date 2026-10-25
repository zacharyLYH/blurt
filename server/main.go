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

	"blurt/internal/agent"
	"blurt/internal/auth"
	"blurt/internal/capture"
	"blurt/internal/config"
	"blurt/internal/db"
	"blurt/internal/decision"
	"blurt/internal/httpapi"
	"blurt/internal/mail"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
	"blurt/internal/stt"
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
		"redis_configured", cfg.RedisURL != "",
		"bind", cfg.Bind,
		"public_origins", cfg.PublicOrigins,
		"smtp_host", cfg.SMTP.Host,
		"smtp_port", cfg.SMTP.Port,
		"smtp_from", cfg.SMTP.From,
		"vapid_subject", cfg.VAPID.Subject,
		"stt_configured", cfg.STT.URL != "",
		"stt_model", cfg.STT.Model,
		"fake_stt", cfg.FakeSTT,
		"decision_model", cfg.Decision.Model,
		"gate_on", cfg.Decision.URL != "",
		"agent_model", cfg.Agent.Model,
		"fake_agent", cfg.FakeAgent,
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
	notifier := &push.Notifier{Sessions: sessions, Sender: push.NewSender(cfg.VAPID)}
	audio, err := capture.NewStore(cfg.DataDir)
	if err != nil {
		panic(err)
	}
	jobs := store.NewJobStore(handle.Write, handle.Read)

	// Both consumer groups must exist before the first producer publishes.
	boot := context.Background()
	transcriptionQueue, err := queue.NewRedis(boot, cfg.RedisURL, queue.Config{
		Stream: "job.transcription", Group: "capture", Consumer: "capture-1", ClaimerID: "claimer-capture",
	})
	if err != nil {
		panic(err)
	}
	defer transcriptionQueue.Close()
	agentQueue, err := queue.NewRedis(boot, cfg.RedisURL, queue.Config{
		Stream: "job.agent", Group: "agent", Consumer: "agent-1", ClaimerID: "claimer-agent",
	})
	if err != nil {
		panic(err)
	}
	defer agentQueue.Close()

	// The fake is the local/e2e escape hatch, exactly like BLURT_FAKE_PIN.
	var transcriber stt.Transcriber
	if cfg.FakeSTT {
		slog.Warn("BLURT_FAKE_STT is on: transcripts are synthetic — never do this on a public host")
		transcriber = stt.NewFake(stt.FakeOK)
	} else {
		transcriber = stt.NewHTTP(cfg.STT)
	}
	// The gate is advisory, so it stays a nil interface when no decision gateway
	// is configured and the worker skips it.
	var gate decision.Decider
	if cfg.Decision.URL != "" {
		gate = decision.NewHTTP(cfg.Decision)
	}
	transcriptionWorker := &capture.TranscriptionWorker{
		Jobs: jobs, Audio: audio, STT: transcriber, Pusher: notifier, Model: cfg.STT.Model,
		Gate: gate, GateModel: cfg.Decision.Model,
		TranscriptionQueue: transcriptionQueue, AgentQueue: agentQueue,
	}

	// The model rides the same gateway as phonon and laya, so the fake is the
	// local/e2e escape hatch that keeps the AgentLoop exercisable without one.
	var model agent.Model
	var tagger agent.Tagger
	if cfg.FakeAgent {
		slog.Warn("BLURT_FAKE_AGENT is on: the model is scripted — never do this on a public host")
		model, tagger = agent.Fake{}, agent.FakeTagger{}
	} else {
		gateway := agent.NewGateway(cfg.Agent)
		// Tags are a second, single-purpose call on the same model. The records
		// schema is measured to break down when it grows, so the features never
		// ride in it.
		model, tagger = gateway, gateway
	}
	loop := &agent.AgentLoop{
		Store:      jobs,
		Recall:     agent.Reads{Reminders: store.NewReminderStore(handle.Read), Memories: store.NewMemoryStore(handle.Read)},
		Model:      model,
		Tagger:     tagger,
		Pusher:     notifier,
		AgentQueue: agentQueue,
		ModelName:  cfg.Agent.Model,
		MaxTurns:   cfg.Agent.MaxTurns,
	}
	// The fallback reader is laya, the same classifier the transcript gate uses, and only a configured
	// gateway has one. With none, the deterministic lints are the whole of PostAgentCheck.
	if gate != nil {
		loop.Fallback = agent.Laya{Decider: gate}
	}
	agentWorker := &agent.AgentWorker{Jobs: jobs, Loop: loop, AgentQueue: agentQueue}

	srv := &http.Server{
		Addr: cfg.Bind,
		Handler: httpapi.New(httpapi.Deps{
			DB:             handle.Write,
			Users:          store.NewUserStore(handle.Write, handle.Read),
			Sessions:       sessions,
			Pins:           auth.NewPinStore(5*time.Minute, fixedPIN),
			Mailer:         mailer,
			Pusher:         notifier,
			Jobs:           jobs,
			Audio:          audio,
			Queues:         &queue.Queues{TranscriptionQueue: transcriptionQueue, AgentQueue: agentQueue},
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

	go transcriptionWorker.Run(ctx)
	go agentWorker.Run(ctx)
	go (&queue.Claimer{
		Stream: transcriptionQueue, Jobs: jobs, Stage: store.TranscriptionStage,
		Fallback: capture.FallbackMessage, MinIdle: 360 * time.Second,
	}).Run(ctx)
	go (&queue.Claimer{
		Stream: agentQueue, Jobs: jobs, Stage: store.AgentStage,
		Fallback: agent.FallbackMessage, MinIdle: 1080 * time.Second,
	}).Run(ctx)
	go (&queue.Sweeper{Jobs: jobs, TranscriptionQueue: transcriptionQueue, AgentQueue: agentQueue}).Run(ctx)

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
