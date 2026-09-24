package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestAuthorizeAndRevoke(t *testing.T) {
	s, _ := openTestStore(t)

	if ok, err := s.IsAuthorized(42); err != nil || ok {
		t.Fatalf("IsAuthorized(42) = (%v, %v), want (false, nil)", ok, err)
	}

	added, err := s.Authorize(42, 1)
	if err != nil || !added {
		t.Fatalf("Authorize(42) = (%v, %v), want (true, nil)", added, err)
	}

	// Re-authorizing an existing ID reports no change.
	if added, err := s.Authorize(42, 1); err != nil || added {
		t.Errorf("second Authorize(42) = (%v, %v), want (false, nil)", added, err)
	}

	// Either the chat or the user being authorized is enough.
	if ok, err := s.IsAuthorized(-100, 42); err != nil || !ok {
		t.Errorf("IsAuthorized(-100, 42) = (%v, %v), want (true, nil)", ok, err)
	}

	removed, err := s.Revoke(42)
	if err != nil || !removed {
		t.Fatalf("Revoke(42) = (%v, %v), want (true, nil)", removed, err)
	}
	if removed, err := s.Revoke(42); err != nil || removed {
		t.Errorf("second Revoke(42) = (%v, %v), want (false, nil)", removed, err)
	}
	if ok, err := s.IsAuthorized(42); err != nil || ok {
		t.Errorf("IsAuthorized(42) after revoke = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestChannelJobsQueueOnceAndSurviveReopen(t *testing.T) {
	s, path := openTestStore(t)
	job := Job{OwnerID: 1, Kind: "torrent", ItemID: 725, Name: "Film"}

	if added, err := s.EnqueueJob(job); err != nil || !added {
		t.Fatalf("EnqueueJob = (%v, %v), want (true, nil)", added, err)
	}
	// The same download is never queued, and so never posted, twice.
	if added, err := s.EnqueueJob(job); err != nil || added {
		t.Errorf("second EnqueueJob = (%v, %v), want (false, nil)", added, err)
	}
	if _, err := s.EnqueueJob(Job{OwnerID: 1, Kind: "usenet", ItemID: 725}); err != nil {
		t.Fatalf("EnqueueJob usenet: %v", err)
	}

	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	pending, err := reopened.PendingJobs()
	if err != nil || len(pending) != 2 || pending[0] != job || pending[1].Name != "Unknown" {
		t.Fatalf("PendingJobs = %+v, %v", pending, err)
	}

	if err := reopened.FinishJob(job, JobSuccess); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	if err := reopened.FinishJob(job, "maybe"); err == nil {
		t.Error("FinishJob accepted an unknown status")
	}
	pending, _ = reopened.PendingJobs()
	if len(pending) != 1 || pending[0].Kind != "usenet" {
		t.Errorf("PendingJobs after finish = %+v", pending)
	}
}
