package flow

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"blurt/internal/agent"
	"blurt/internal/capture"
	"blurt/internal/db"
	"blurt/internal/queue"
	"blurt/internal/store"
	"blurt/internal/stt"
)

const (
	// pollEvery keeps both worker loops responsive: they are the only thing
	// moving a Job in these tests.
	pollEvery = time.Millisecond
	// waitUntil is how long a flow may take before a test calls it stuck. Every
	// step here is in-process, so anything near this is a real failure.
	waitUntil = 5 * time.Second
)

// pinned is the anchor the loop resolves relative times against: a Saturday
// evening in Singapore, so "tomorrow at 6pm" is a fixed instant to assert on.
var pinned = time.Date(2026, 10, 24, 18, 30, 0, 0, time.FixedZone("Asia/Singapore", 8*60*60))

// pump is main.go's two workers wired the same way: one goroutine per stage,
// real SQLite, one in-memory stream per stage. Tests drive whole flows through
// it instead of calling a worker step directly.
type pump struct {
	t             *testing.T
	handle        *db.DB
	jobs          *store.JobStore
	audio         *capture.Store
	transcription queue.Stream
	agent         queue.Stream
	captureWorker *capture.TranscriptionWorker
	agentWorker   *agent.AgentWorker
	model         *scripted
	pusher        *recordingPusher
	userID        int64

	// Each stage is started and stopped on its own, so a test can kill one
	// worker mid-flow and hand its message to the claimer. The WaitGroups make a
	// stop observable, so a restarted worker never races a still-running one.
	runCtx        context.Context
	runCancel     context.CancelFunc
	captureCancel context.CancelFunc
	agentCancel   context.CancelFunc
	claimerCancel context.CancelFunc
	captureWG     sync.WaitGroup
	agentWG       sync.WaitGroup
	claimerWG     sync.WaitGroup
}

// scripted is the model port answering a fixed list of turns. It keeps every
// Input it was handed, which is how a prompt assertion reaches the loop.
// newPump builds the stack over in-memory streams, without starting it, so a
// test can swap the transcriber, gate or timeouts before either worker runs.
func newPump(t *testing.T, turns ...agent.Turn) *pump {
	return newPumpOn(t, queue.NewMock(), queue.NewMock(), turns...)
}

// newRedisPump is newPump over real Redis streams, which is the only pump that
// exercises the broker — group creation included. It skips unless
// BLURT_TEST_REDIS_URL is set.
func newRedisPump(t *testing.T, turns ...agent.Turn) *pump {
	t.Helper()
	key := "blurt:" + strings.ReplaceAll(t.Name(), "/", "-")
	return newPumpOn(t, redisStream(t, key+":tx", "capture"), redisStream(t, key+":agent", "agent"), turns...)
}

// redisStream boots one real stream, or skips when no broker is configured.
func redisStream(t *testing.T, stream, group string) *queue.Redis {
	t.Helper()
	url := os.Getenv("BLURT_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set BLURT_TEST_REDIS_URL (or run make test-redis) to exercise real Redis")
	}
	booted, err := queue.NewRedis(context.Background(), url, queue.Config{
		Stream: stream, Group: group, Consumer: group + "-1", ClaimerID: group + "-claimer"})
	if err != nil {
		t.Fatalf("boot %s: %v", stream, err)
	}
	t.Cleanup(func() { booted.Close() })
	return booted
}

func newPumpOn(t *testing.T, transcription, agentStream queue.Stream, turns ...agent.Turn) *pump {
	t.Helper()
	handle, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	audio, err := capture.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("open audio store: %v", err)
	}
	user, err := store.NewUserStore(handle.Write, handle.Read).EnsureUser(
		context.Background(), "Lam", "lam@example.com")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	jobs := store.NewJobStore(handle.Write, handle.Read)
	model, pusher := &scripted{turns: turns}, &recordingPusher{}
	loop := &agent.AgentLoop{
		Store: jobs,
		Recall: agent.Reads{
			Reminders: store.NewReminderStore(handle.Write, handle.Read),
			Memories:  store.NewMemoryStore(handle.Read),
		},
		Model: model, Pusher: pusher, AgentQueue: agentStream,
		ModelName: "scripted", MaxTurns: 3,
		Now: func() time.Time { return pinned },
	}
	captureWorker := &capture.TranscriptionWorker{
		Jobs: jobs, Audio: audio, STT: scriptedTranscriber{text: stt.FakeTranscript},
		Pusher: pusher, Model: "phonon", GateModel: "laya",
		TranscriptionQueue: transcription, AgentQueue: agentStream, Poll: pollEvery,
	}
	return &pump{
		t: t, handle: handle, jobs: jobs, audio: audio,
		transcription: transcription, agent: agentStream,
		captureWorker: captureWorker,
		agentWorker:   &agent.AgentWorker{Jobs: jobs, Loop: loop, AgentQueue: agentStream, Poll: pollEvery},
		model:         model, pusher: pusher, userID: user.UserID,
	}
}

// start runs both stages until the test ends.
//
// Configure the workers **before** calling this: the loops read their worker's
// fields — STT, the gate, the model's script, the timeouts — without a lock, so
// a write after start races the goroutine that is about to read it. The
// per-stage stop/start pair is the way to swap them mid-flow.
func (p *pump) start() *pump {
	return p.startCapture().startAgent()
}

// ensureRunCtx is the parent every stage's context hangs off.
func (p *pump) ensureRunCtx() context.Context {
	if p.runCtx == nil {
		p.runCtx, p.runCancel = context.WithCancel(context.Background())
		p.t.Cleanup(p.runCancel)
	}
	return p.runCtx
}

func (p *pump) startCapture() *pump {
	if p.captureCancel == nil {
		ctx, cancel := context.WithCancel(p.ensureRunCtx())
		p.captureCancel = cancel
		p.captureWG.Add(1)
		go func() {
			defer p.captureWG.Done()
			p.captureWorker.Run(ctx)
		}()
	}
	return p
}

// stopCapture kills the transcriber mid-flight and waits for its loop to exit, so
// a restarted worker can never race the one being replaced.
func (p *pump) stopCapture() {
	if p.captureCancel == nil {
		return
	}
	p.captureCancel()
	p.captureWG.Wait()
	p.captureCancel = nil
}

func (p *pump) startAgent() *pump {
	if p.agentCancel == nil {
		ctx, cancel := context.WithCancel(p.ensureRunCtx())
		p.agentCancel = cancel
		p.agentWG.Add(1)
		go func() {
			defer p.agentWG.Done()
			p.agentWorker.Run(ctx)
		}()
	}
	return p
}

func (p *pump) stopAgent() {
	if p.agentCancel == nil {
		return
	}
	p.agentCancel()
	p.agentWG.Wait()
	p.agentCancel = nil
}

// startClaimers runs both reclaim loops once, the way boot does. A test starts
// them only when it wants the pending-message path: the Mock cannot model a
// visibility timeout, so a claimer running beside healthy workers would reclaim
// work that is still in flight.
func (p *pump) startClaimers() *pump {
	if p.claimerCancel == nil {
		ctx, cancel := context.WithCancel(p.ensureRunCtx())
		p.claimerCancel = cancel
		claimers := []*queue.Claimer{
			{Stream: mustClaim(p.t, p.transcription), Jobs: p.jobs,
				Stage: store.TranscriptionStage, Fallback: capture.FallbackMessage},
			{Stream: mustClaim(p.t, p.agent), Jobs: p.jobs,
				Stage: store.AgentStage, Fallback: agent.FallbackMessage},
		}
		for _, claimer := range claimers {
			p.claimerWG.Add(1)
			go func(claimer *queue.Claimer) {
				defer p.claimerWG.Done()
				claimer.Run(ctx)
			}(claimer)
		}
	}
	return p
}

func (p *pump) stopClaimers() {
	if p.claimerCancel == nil {
		return
	}
	p.claimerCancel()
	p.claimerWG.Wait()
	p.claimerCancel = nil
}

// mustClaim narrows a stream to the interface the claimer and sweeper need; both
// real and in-memory streams implement it.
func mustClaim(t *testing.T, stream queue.Stream) queue.Claiming {
	t.Helper()
	claiming, ok := stream.(queue.Claiming)
	if !ok {
		t.Fatalf("%T cannot claim", stream)
	}
	return claiming
}
