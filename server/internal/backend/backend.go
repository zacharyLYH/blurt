// Package backend assembles Blurt's runtime: the HTTP surface and the workers that move a
// Job between its stages. main boots it from .env; a test boots the same thing with its own
// loop, streams and clock, so what a test exercises is production's wiring rather than a
// second one written for the test.
package backend

import (
	"context"
	"net/http"
	"sync"
	"time"

	"blurt/internal/agent"
	"blurt/internal/auth"
	"blurt/internal/capture"
	"blurt/internal/db"
	"blurt/internal/emitter"
	"blurt/internal/httpapi"
	"blurt/internal/push"
	"blurt/internal/queue"
	"blurt/internal/store"
	"blurt/internal/stt"
)

// Deps is the whole runtime, one field per collaborator. Every store is passed rather than
// rebuilt here, because the caller already opened the one database they share.
type Deps struct {
	DB        *db.DB
	Users     *store.UserStore
	Jobs      *store.JobStore
	Sessions  *store.SessionStore
	Reminders *store.ReminderStore
	Audio     *capture.Store
	Queues    *queue.Queues
	// Loop is the agent's own, built by the caller: main resolves the user's model there,
	// and a test hands it a pinned clock.
	Loop *agent.AgentLoop
	STT  stt.Transcriber
	// STTModel is the id the transcription Trace reports.
	STTModel string
	Pins     *auth.PinStore
	// Mailer may be nil: dev mode issues a fixed PIN and sends nothing.
	Mailer httpapi.PINMailer
	Pusher push.Notifier
	// Emitter may be nil: a caller that does not want reminders firing under it omits it.
	Emitter *emitter.Emitter
	// Poll is how often both stage workers look for work; zero means the worker default.
	Poll           time.Duration
	EncryptionKey  []byte
	VAPIDPublicKey string
	AllowedOrigins []string
}

// Backend is the wired stack. New touches nothing that runs; the work starts at Start or
// Run, so a caller can finish configuring before the first poll.
type Backend struct {
	Deps
	transcription *capture.TranscriptionWorker
	agent         *queue.Worker
	running       sync.WaitGroup
}

// New binds the two stage workers to the given loop and streams. It is the same three lines
// main used to hold, kept here because a test needs them too.
func New(deps Deps) *Backend {
	return &Backend{
		Deps: deps,
		transcription: &capture.TranscriptionWorker{
			Jobs: deps.Jobs, Audio: deps.Audio, STT: deps.STT, Pusher: deps.Pusher,
			Model:              deps.STTModel,
			TranscriptionQueue: deps.Queues.TranscriptionQueue,
			AgentQueue:         deps.Queues.AgentQueue,
			Poll:               deps.Poll,
		},
		agent: &queue.Worker{
			Stream: deps.Queues.AgentQueue, Jobs: deps.Jobs, Step: deps.Loop.Step, Poll: deps.Poll,
		},
	}
}

// Handler is the whole HTTP API over this stack: the ingress a browser posts to, the Job
// the thread polls, and the reply that continues one.
func (b *Backend) Handler() http.Handler {
	return httpapi.New(httpapi.Deps{
		DB:             b.DB.Write,
		Users:          b.Users,
		Sessions:       b.Sessions,
		Pins:           b.Pins,
		Mailer:         b.Mailer,
		Pusher:         b.Pusher,
		Jobs:           b.Jobs,
		Audio:          b.Audio,
		Queues:         b.Queues,
		Reminders:      b.Reminders,
		VAPIDPublicKey: b.VAPIDPublicKey,
		AllowedOrigins: b.AllowedOrigins,
		EncryptionKey:  b.EncryptionKey,
	})
}

// Start runs the two stage workers until ctx ends. It is what a test needs when it drives
// the ingress and reads the rows back: no claimer, sweeper or emitter changing state behind
// the assertion.
func (b *Backend) Start(ctx context.Context) {
	b.running.Add(2)
	go func() {
		defer b.running.Done()
		b.transcription.Run(ctx)
	}()
	go func() {
		defer b.running.Done()
		b.agent.Run(ctx)
	}()
}

// Wait blocks until the two stage workers have stopped, so a caller can close the database
// it handed in without racing a query that was still in flight.
func (b *Backend) Wait() { b.running.Wait() }

// Run starts every worker main starts and blocks until ctx ends: the two stages, the two
// claimers that hand back stranded messages, the sweeper that republishes a Job whose
// publish never landed, and the emitter that fires what has come due.
func (b *Backend) Run(ctx context.Context) {
	b.Start(ctx)
	claimers := []*queue.Claimer{
		claimer(b.Queues.TranscriptionQueue, b.Jobs, store.TranscriptionStage,
			capture.FallbackMessage, 360*time.Second),
		claimer(b.Queues.AgentQueue, b.Jobs, store.AgentStage,
			agent.FallbackMessage, 1080*time.Second),
	}
	for _, claimer := range claimers {
		if claimer != nil {
			go claimer.Run(ctx)
		}
	}
	go (&queue.Sweeper{
		Jobs: b.Jobs, TranscriptionQueue: b.Queues.TranscriptionQueue, AgentQueue: b.Queues.AgentQueue,
	}).Run(ctx)
	if b.Emitter != nil {
		go b.Emitter.Run(ctx)
	}
	<-ctx.Done()
}

// claimer narrows a stream to the one interface the claim path needs. A stream that cannot
// claim (nothing ships one) simply gets no claimer rather than a panic at boot.
func claimer(stream queue.Stream, jobs *store.JobStore, stage, fallback string, idle time.Duration) *queue.Claimer {
	claiming, ok := stream.(queue.Claiming)
	if !ok {
		return nil
	}
	return &queue.Claimer{Stream: claiming, Jobs: jobs, Stage: stage, Fallback: fallback, MinIdle: idle}
}
