// Package git implements isolated, normal-push operations using go-git.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

var ErrCredentialsInvalid = errors.New("git authentication or authorization failed")

var ErrRetry = errors.New("remote publication not confirmed; retry from current branch")

type Operation struct {
	URL, Branch, Path                string
	Content                          []byte
	Delete                           bool
	Credentials                      Credentials
	AuthorName, AuthorEmail, Message string
	PreviousRevision                 string
}
type Result struct {
	Revision string
	Changed  bool
}

type Publisher struct {
	// BeforePush is a narrow integration-test seam. Production leaves it nil.
	BeforePush func()
	// Push substitutes transport acknowledgment in integration tests only.
	Push func(context.Context, *gogit.Repository, *gogit.PushOptions) error
}

func clone(ctx context.Context, op Operation) (*gogit.Repository, string, error) {
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
	return repo, dir, nil
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
	equal, err := matches(dir, op)
	if err != nil {
		return Result{}, err
	}
	if equal {
		sha, err := revision(repo, op, true)
		return Result{Revision: sha}, err
	}
	file, err := safeFile(dir, op.Path)
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
	if p.Push != nil {
		_ = p.Push(ctx, repo, options)
	} else {
		_ = repo.PushContext(ctx, options)
	}
	verified, verifyDir, err := clone(ctx, op)
	if err != nil {
		return Result{}, ErrRetry
	}
	defer os.RemoveAll(verifyDir)
	equal, err = matches(verifyDir, op)
	if err != nil {
		return Result{}, err
	}
	if !equal {
		return Result{}, ErrRetry
	}
	sha, err := revision(verified, op, false)
	return Result{Revision: sha, Changed: true}, err
}
