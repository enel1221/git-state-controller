package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/inelson/git-state-controller/internal/testgit"
)

func operation(s *testgit.Server, path string) Operation {
	return Operation{URL: s.URL, Branch: "main", Path: path, Content: []byte("kind: ConfigMap\n"), Credentials: Credentials{"bot", "password"}, AuthorName: "test", AuthorEmail: "test@example.invalid", Message: "publish"}
}
func TestPublicationRecoveryAndDelete(t *testing.T) {
	s := testgit.New(t)
	p := &Publisher{}
	op := operation(s, "shared/one.yaml")
	first, err := p.Attempt(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Revision) != 40 || !first.Changed {
		t.Fatal(first)
	}
	s.Assert(t, op.Path, op.Content)
	other := operation(s, "shared/two.yaml")
	if _, err = p.Attempt(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	op.PreviousRevision = first.Revision
	count := s.Count(t)
	same, err := p.Attempt(context.Background(), op)
	if err != nil || same.Changed || same.Revision != first.Revision || s.Count(t) != count {
		t.Fatalf("no-op advanced revision: %+v %v", same, err)
	}
	op.Content = []byte("kind: ConfigMap\ndata: {value: updated}\n")
	uncertain := &Publisher{Push: func(ctx context.Context, r *gogit.Repository, o *gogit.PushOptions) error {
		if err := r.PushContext(ctx, o); err != nil {
			return err
		}
		return errors.New("lost acknowledgment")
	}}
	if _, err = uncertain.Attempt(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	count = s.Count(t)
	if _, err = p.Attempt(context.Background(), op); err != nil || s.Count(t) != count {
		t.Fatal("duplicate recovery commit", err)
	}
	op.Delete = true
	if _, err = p.Attempt(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if s.Read(t, op.Path) != nil {
		t.Fatal("file retained")
	}
	s.Assert(t, "shared/two.yaml", other.Content)
	s.Assert(t, "README.md", []byte("unrelated seed\n"))
	count = s.Count(t)
	if result, err := p.Attempt(context.Background(), op); err != nil || result.Changed || s.Count(t) != count {
		t.Fatal("duplicate delete", err)
	}
}
func TestForcedBranchRace(t *testing.T) {
	s := testgit.New(t)
	ready := make(chan struct{}, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release := make(chan struct{})
	p := &Publisher{BeforePush: func() { ready <- struct{}{}; <-release }}
	var wg sync.WaitGroup
	wg.Add(2)
	errorsOut := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := p.Attempt(ctx, operation(s, fmt.Sprintf("race/%d.yaml", i)))
			errorsOut <- err
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-ctx.Done():
			close(release)
			wg.Wait()
			t.Fatal("operations failed to reach branch-race barrier")
		}
	}
	close(release)
	wg.Wait()
	close(errorsOut)
	rejected := 0
	for err := range errorsOut {
		if errors.Is(err, ErrRetry) {
			rejected++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if rejected != 1 {
		t.Fatalf("expected one branch rejection, got %d", rejected)
	}
	for i := 0; i < 2; i++ {
		op := operation(s, fmt.Sprintf("race/%d.yaml", i))
		if _, err := (&Publisher{}).Attempt(context.Background(), op); err != nil {
			t.Fatal(err)
		}
		s.Assert(t, op.Path, op.Content)
	}
	if s.Count(t) != 3 {
		t.Fatal("unexpected history", s.Count(t))
	}
}
func TestDestinationValidation(t *testing.T) {
	for _, path := range []string{"/a.yaml", "../a.yaml", "a/../b.yaml", ".git/a.yaml", "a//b.yaml", "a*.yaml", "a[1].yaml", "a{b}.yaml", "C:/a.yaml", "a.txt"} {
		if ValidateDestination("https://example.invalid/repo.git", "main", path, false) == nil {
			t.Fatal("accepted", path)
		}
	}
	for _, url := range []string{"http://example.invalid/repo.git", "https://u:p@example.invalid/repo.git", "ssh://example.invalid/repo.git", "https://example.invalid/repo.git?token=x"} {
		if ValidateDestination(url, "main", "a.yaml", false) == nil {
			t.Fatal("accepted", url)
		}
	}
	for _, branch := range []string{"refs/heads/main", "a..b", "a:b", "main.lock", "0123456789012345678901234567890123456789"} {
		if ValidateDestination("https://example.invalid/repo.git", branch, "a.yaml", false) == nil {
			t.Fatal("accepted", branch)
		}
	}
	if err := ValidateDestination("http://forgejo/repo.git", "feature/demo", "x/a.yml", true); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeFile(root, "escape/a.yaml"); err == nil {
		t.Fatal("symlink accepted")
	}
}
