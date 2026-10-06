// Package testgit supplies an actual authenticated Git smart-HTTP fixture for tests.
// Git CLI is used only by the fixture's HTTP backend, never by production code.
package testgit

import (
	"bytes"
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type Server struct {
	URL, Root string
	Server    *httptest.Server
}

func New(t testing.TB) *Server {
	t.Helper()
	root := t.TempDir()
	barePath := filepath.Join(root, "resources.git")
	bare, err := gogit.PlainInit(barePath, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "README.md"), []byte("unrelated seed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	work, _ := repo.Worktree()
	if _, err = work.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err = work.Commit("seed", &gogit.CommitOptions{Author: &object.Signature{Name: "Fixture", Email: "fixture@example.invalid", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{barePath}}); err != nil {
		t.Fatal(err)
	}
	if err = repo.Push(&gogit.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), "git", "--exec-path")
	b, err := cmd.Output()
	if err != nil {
		t.Fatal("Git executable required for smart-HTTP test fixture:", err)
	}
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(b)), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	cfg, _ := bare.Config()
	cfg.Raw.Section("http").SetOption("receivepack", "true")
	if err = bare.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "bot" || p != "password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(401)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return &Server{URL: server.URL + "/resources.git", Root: barePath, Server: server}
}
func (s *Server) Read(t testing.TB, name string) []byte {
	t.Helper()
	repo, err := gogit.PlainOpen(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	f, err := commit.File(name)
	if err != nil {
		return nil
	}
	b, err := f.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return []byte(b)
}
func (s *Server) Count(t testing.TB) int {
	t.Helper()
	repo, err := gogit.PlainOpen(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	iter, err := repo.Log(&gogit.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()
	n := 0
	if err = iter.ForEach(func(*object.Commit) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}
func (s *Server) Assert(t testing.TB, path string, want []byte) {
	t.Helper()
	if b := s.Read(t, path); !bytes.Equal(b, want) {
		t.Fatalf("%s: got %q want %q", path, b, want)
	}
}
