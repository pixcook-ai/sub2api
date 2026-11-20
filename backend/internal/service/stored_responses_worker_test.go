package service

import (
	"context"
	"testing"
	"time"
)

type storedResponseWorkerRepository struct {
	StoredResponseRepository
	started chan int
	exited  chan struct{}
}

func (r *storedResponseWorkerRepository) Sweep(ctx context.Context, limit int) (int, error) {
	r.started <- limit
	<-ctx.Done()
	close(r.exited)
	return 0, ctx.Err()
}

func TestStoredResponsesWorkerStopCancelsAndJoinsBoundedSweep(t *testing.T) {
	repo := &storedResponseWorkerRepository{started: make(chan int, 2), exited: make(chan struct{})}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, err := NewStoredResponseService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewStoredResponsesWorker(svc)
	worker.Start()
	worker.Start()
	select {
	case limit := <-repo.started:
		if limit != 100 {
			t.Fatalf("unbounded sweep: %d", limit)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	done := make(chan struct{})
	go func() { worker.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel and join")
	}
	select {
	case <-repo.exited:
	default:
		t.Fatal("Stop returned before sweep exited")
	}
	select {
	case <-repo.started:
		t.Fatal("duplicate Start launched another worker")
	default:
	}
	worker.Stop()
}

func TestStoredResponsesWorkerDisabledStartsNoSweep(t *testing.T) {
	repo := &storedResponseWorkerRepository{started: make(chan int, 1), exited: make(chan struct{})}
	svc, err := NewStoredResponseService(repo, DefaultStoredResponseConfig())
	if err != nil {
		t.Fatal(err)
	}
	worker := NewStoredResponsesWorker(svc)
	worker.Start()
	worker.Stop()
	select {
	case <-repo.started:
		t.Fatal("disabled worker swept storage")
	default:
	}
}
