// Package git implements isolated, normal-push operations using go-git.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inelson/git-state-controller/internal/manifest"
	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

var ErrCredentialsInvalid = errors.New("git authentication or authorization failed")

var ErrRetry = errors.New("remote publication not confirmed; retry from current branch")

var ErrBranchChanged = fmt.Errorf("%w: remote branch advanced", ErrRetry)

var ErrPathAlreadyExists = errors.New("PathAlreadyExists: existing file is not owned by this GitResource; explicit adoption required")
var ErrRecoveryRequired = errors.New("RecoveryRequired: cannot verify the last owned publication")
var ErrPaused = errors.New("reconciliation paused")
var gitDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "git_state_git_operation_seconds", Help: "Actual Git transport durations", Buckets: prometheus.DefBuckets}, []string{"operation", "outcome"})
var rejectedPush = prometheus.NewCounter(prometheus.CounterOpts{Name: "git_state_rejected_push_retries_total", Help: "Pushes requiring refreshed branch retry"})

func init() { metrics.Registry.MustRegister(gitDuration, rejectedPush) }

type Operation struct {
	OwnershipGuard                            func(context.Context, string, string, string) error
	URL, Branch, Path                         string
	Content                                   []byte
	Delete                                    bool
	Credentials                               Credentials
	AuthorName, AuthorEmail, Message          string
	PreviousRevision                          string
	PreviousHash                              string
	OwnerUID, SourceNamespace, SourceName     string
	AllowAdopt, AllowLegacy, ReadOnly, Orphan bool
	Guard                                     func(context.Context) error
	Access                                    func(operation string, success bool)
	FlushAccess                               func()
}
type Recovery struct {
	Revision   string
	Content    []byte
	Generation int64
}
type Result struct {
	Recovery *Recovery
	Revision string
	Changed  bool
	Hash     string
	Missing  bool
	Unowned  bool
}

type Publisher struct {
	// BeforePush is a narrow integration-test seam. Production leaves it nil.
	BeforePush func()
	// Push substitutes transport acknowledgment in integration tests only.
	Push func(context.Context, *gogit.Repository, *gogit.PushOptions) error
}

func recordOperation(ctx context.Context, operation string, start time.Time, success bool) {
	outcome := "succeeded"
	if !success {
		outcome = "failed"
	}
	duration := time.Since(start).Seconds()
	gitDuration.WithLabelValues(operation, outcome).Observe(duration)
	ctrl.LoggerFrom(ctx).Info("Git operation", "operation", operation, "outcome", outcome, "durationSeconds", duration)
}

func clone(ctx context.Context, op Operation) (*gogit.Repository, string, error) {
	start := time.Now()
	success := false
	defer func() {
		recordOperation(ctx, "Clone", start, success)
		if op.Access != nil {
			op.Access("Fetch", success)
		}
	}()
	dir, err := os.MkdirTemp("", "git-state-")
	if err != nil {
		return nil, "", fmt.Errorf("create temporary worktree: %w", err)
	}
	repo, err := gogit.PlainCloneContext(ctx, dir, false, &gogit.CloneOptions{
		URL: op.URL, ReferenceName: plumbing.NewBranchReferenceName(op.Branch), SingleBranch: true,
		Auth: &http.BasicAuth{Username: op.Credentials.Username, Password: op.Credentials.Password},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
			return nil, "", ErrCredentialsInvalid
		}
		return nil, "", errors.New("clone failed; check repository, branch, credentials and connectivity")
	}
	success = true
	return repo, dir, nil
}

// refresh verifies remote state in the isolated worktree without a second clone.
// The force refspec refreshes only our local tracking ref; pushes remain normal.
func refresh(ctx context.Context, repo *gogit.Repository, op Operation) (err error) {
	start := time.Now()
	defer func() {
		recordOperation(ctx, "Fetch", start, err == nil)
		if op.Access != nil {
			op.Access("Fetch", err == nil)
		}
	}()
	ref := plumbing.ReferenceName("refs/remotes/origin/" + op.Branch)
	err = repo.FetchContext(ctx, &gogit.FetchOptions{
		RemoteName: "origin", RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/" + op.Branch + ":" + ref.String())},
		Auth: &http.BasicAuth{Username: op.Credentials.Username, Password: op.Credentials.Password},
	})
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		return ErrCredentialsInvalid
	}
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return errors.New("fetch failed; check repository, branch, credentials and connectivity")
	}
	remote, err := repo.Reference(ref, true)
	if err != nil {
		return err
	}
	work, err := repo.Worktree()
	if err != nil {
		return err
	}
	return work.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: remote.Hash()})
}

// safeFile rejects symlinks in every existing component, even links pointing inside the tree.
func safeFile(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name {
		return "", errors.New("unsafe file path")
	}
	current := root
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == ".git" || part == "." || part == "" {
			return "", errors.New("unsafe file path")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink in managed file path")
		}
	}
	return current, nil
}
func matches(dir string, op Operation) (bool, error) {
	file, err := safeFile(dir, op.Path)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return op.Delete, nil
	}
	if err != nil {
		return false, err
	}
	return !op.Delete && bytes.Equal(b, op.Content), nil
}
func revision(repo *gogit.Repository, op Operation, preserve bool) (string, error) {
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	if preserve && op.PreviousRevision != "" && !op.Delete {
		commit, err := repo.CommitObject(plumbing.NewHash(op.PreviousRevision))
		if err == nil {
			file, err := commit.File(op.Path)
			if err == nil {
				content, err := file.Contents()
				if err == nil && bytes.Equal([]byte(content), op.Content) {
					return op.PreviousRevision, nil
				}
			}
		}
	}
	if preserve && op.OwnerUID != "" && !op.Delete {
		iterator, e := repo.Log(&gogit.LogOptions{FileName: &op.Path})
		if e == nil {
			defer iterator.Close()
			for {
				commit, e := iterator.Next()
				if e != nil {
					break
				}
				content, e := fileAt(repo, commit.Hash.String(), op.Path)
				if e == nil && bytes.Equal(content, op.Content) && strings.Contains(commit.Message, "\nGitResource-UID: "+op.OwnerUID+"\n") {
					return commit.Hash.String(), nil
				}
			}
		}
	}
	return head.Hash().String(), nil
}

// Attempt performs one isolated attempt. The reconciler refreshes the CR before retrying.
// Every push, including an uncertain acknowledgment, is followed by remote verification.
func (p *Publisher) Attempt(ctx context.Context, op Operation) (Result, error) {
	repo, dir, err := clone(ctx, op)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	if !op.ReadOnly {
		if err := refresh(ctx, repo, op); err != nil {
			return Result{}, err
		}
	}
	baseHead, err := repo.Head()
	if err != nil {
		return Result{}, err
	}
	file, err := safeFile(dir, op.Path)
	if err != nil {
		return Result{}, err
	}
	content, readErr := os.ReadFile(file)
	missing := os.IsNotExist(readErr)
	if readErr != nil && !missing {
		return Result{}, readErr
	}
	if op.ReadOnly {
		return Result{Revision: op.PreviousRevision, Hash: manifest.Hash(content), Missing: missing}, nil
	}
	ownerNS, ownerName, owner, _ := manifest.Ownership(content)
	if op.AllowAdopt && owner != "" && owner != op.OwnerUID && op.OwnershipGuard != nil {
		if err := op.OwnershipGuard(ctx, ownerNS, ownerName, owner); err != nil {
			return Result{}, err
		}
	}
	legacy := false
	if owner == "" && op.AllowLegacy && op.PreviousRevision != "" {
		previous, e := fileAt(repo, op.PreviousRevision, op.Path)
		legacy = e == nil && manifest.Hash(previous) == op.PreviousHash
	}
	if op.OwnerUID != "" && !missing && owner != op.OwnerUID && !legacy && !op.AllowAdopt {
		if op.Orphan && op.PreviousRevision == "" {
			return Result{Unowned: true}, nil
		}
		if op.Delete {
			if owner != "" || op.PreviousRevision == "" {
				return Result{Unowned: true}, nil
			}
			return Result{}, ErrRecoveryRequired
		}
		return Result{}, ErrPathAlreadyExists
	}
	var recovery *Recovery
	if op.Orphan {
		if missing && op.PreviousRevision == "" {
			return Result{Unowned: true}, nil
		}
		published, recoveredSHA, generation, e := recoverPublished(repo, op)
		if e != nil {
			return Result{}, e
		}
		recovery = &Recovery{Revision: recoveredSHA, Content: published, Generation: generation}
		op.Content, _, err = manifest.Orphan(published, op.SourceNamespace, op.SourceName, op.OwnerUID)
		if err != nil {
			return Result{}, err
		}
		op.Delete = false
	}
	if op.Guard != nil {
		if err := op.Guard(ctx); err != nil {
			return Result{}, err
		}
	}
	equal, err := matches(dir, op)
	if err != nil {
		return Result{}, err
	}
	if equal {
		sha, err := revision(repo, op, true)
		return Result{Revision: sha, Hash: manifest.Hash(op.Content), Missing: op.Delete, Recovery: recovery}, err
	}
	file, err = safeFile(dir, op.Path)
	if err != nil {
		return Result{}, err
	}
	work, err := repo.Worktree()
	if err != nil {
		return Result{}, err
	}
	if op.Delete {
		if _, err = work.Remove(op.Path); err != nil {
			return Result{}, errors.New("stage deletion failed")
		}
	} else {
		if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return Result{}, err
		}
		if err = os.WriteFile(file, op.Content, 0600); err != nil {
			return Result{}, err
		}
		if _, err = work.Add(op.Path); err != nil {
			return Result{}, errors.New("stage file failed")
		}
	}
	_, err = work.Commit(op.Message, &gogit.CommitOptions{Author: &object.Signature{Name: op.AuthorName, Email: op.AuthorEmail, When: time.Now()}})
	if err != nil {
		return Result{}, errors.New("commit failed")
	}
	if p.BeforePush != nil {
		p.BeforePush()
	}
	options := &gogit.PushOptions{RemoteName: "origin", Auth: &http.BasicAuth{Username: op.Credentials.Username, Password: op.Credentials.Password}, RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/" + op.Branch + ":refs/heads/" + op.Branch)}}
	if op.Guard != nil {
		if err := op.Guard(ctx); err != nil {
			return Result{}, err
		}
	}
	start := time.Now()
	var pushErr error
	if p.Push != nil {
		pushErr = p.Push(ctx, repo, options)
	} else {
		pushErr = repo.PushContext(ctx, options)
	}
	recordOperation(ctx, "Push", start, pushErr == nil)
	branchChanged := pushErr != nil && strings.HasPrefix(pushErr.Error(), "non-fast-forward update: ")
	defer func() {
		if op.Access != nil && !branchChanged {
			op.Access("Push", pushErr == nil)
		}
	}()
	err = refresh(ctx, repo, op)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if errors.Is(err, ErrCredentialsInvalid) {
			return Result{}, err
		}
		return Result{}, ErrRetry
	}
	equal, err = matches(dir, op)
	if err != nil {
		return Result{}, err
	}
	if !equal {
		if errors.Is(pushErr, transport.ErrAuthenticationRequired) || errors.Is(pushErr, transport.ErrAuthorizationFailed) {
			return Result{}, ErrCredentialsInvalid
		}
		remoteHead, headErr := repo.Head()
		if headErr != nil {
			return Result{}, headErr
		}
		branchChanged = branchChanged || remoteHead.Hash() != baseHead.Hash()
		if branchChanged {
			rejectedPush.Inc()
			ctrl.LoggerFrom(ctx).Info("Git push requires retry")
			return Result{}, ErrBranchChanged
		}
		ctrl.LoggerFrom(ctx).Info("Git publication requires verification retry")
		return Result{}, ErrRetry
	}
	pushErr = nil // Remote content confirms success even after a lost acknowledgment.
	sha, err := revision(repo, op, false)
	return Result{Revision: sha, Changed: true, Hash: manifest.Hash(op.Content), Missing: op.Delete, Recovery: recovery}, err
}

func fileAt(repo *gogit.Repository, sha, path string) ([]byte, error) {
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, err
	}
	file, err := commit.File(path)
	if err != nil {
		return nil, err
	}
	content, err := file.Contents()
	return []byte(content), err
}
func commitGeneration(repo *gogit.Repository, sha string) int64 {
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(commit.Message, "\n") {
		if strings.HasPrefix(line, "GitResource-Generation: ") {
			n, _ := strconv.ParseInt(strings.TrimPrefix(line, "GitResource-Generation: "), 10, 64)
			return n
		}
	}
	return 0
}
func recoverPublished(repo *gogit.Repository, op Operation) ([]byte, string, int64, error) {
	if op.PreviousRevision != "" {
		content, err := fileAt(repo, op.PreviousRevision, op.Path)
		if err == nil {
			ns, name, uid, _ := manifest.Ownership(content)
			if uid == op.OwnerUID && ns == op.SourceNamespace && name == op.SourceName || uid == "" && op.AllowLegacy && manifest.Hash(content) == op.PreviousHash {
				return content, op.PreviousRevision, commitGeneration(repo, op.PreviousRevision), nil
			}
		}
		return nil, "", 0, ErrRecoveryRequired
	}
	// Narrow path-scoped recovery of this UID's verified publication after a lost status write.
	iterator, err := repo.Log(&gogit.LogOptions{FileName: &op.Path})
	if err != nil {
		return nil, "", 0, ErrRecoveryRequired
	}
	defer iterator.Close()
	for {
		commit, err := iterator.Next()
		if err != nil {
			break
		}
		if !strings.Contains(commit.Message, "\nGitResource-UID: "+op.OwnerUID+"\n") {
			continue
		}
		content, err := fileAt(repo, commit.Hash.String(), op.Path)
		if err != nil {
			continue
		}
		ns, name, uid, _ := manifest.Ownership(content)
		if uid == op.OwnerUID && ns == op.SourceNamespace && name == op.SourceName {
			return content, commit.Hash.String(), commitGeneration(repo, commit.Hash.String()), nil
		}
	}
	return nil, "", 0, ErrRecoveryRequired
}
