package devstack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "hack"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dev-stack.sh", "k3d-runtime.sh", "versions.env"} {
		b, err := os.ReadFile("../../hack/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "hack", name), b, 0700); err != nil {
			t.Fatal(err)
		}
	}
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	body := `#!/bin/sh
if [ "$1 $2" = "cluster list" ]; then
 printf 'git-state-dev\nunrelated-cluster\n'
elif [ "$1 $2" = "cluster delete" ]; then
 printf '%s\n' "$3" >> "$FAKE_ACTIONS"
elif [ "$1" = "version" ]; then
 echo 'k3d version v5.9.0'
fi
`
	if err := os.WriteFile(filepath.Join(tools, "k3d"), []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"docker": "#!/bin/sh\nexit 0\n", "skaffold": "#!/bin/sh\necho v2.25.0\n"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root, tools
}

func TestPodmanImageImportUsesDirectMode(t *testing.T) {
	root, tools := fixture(t)
	binary := filepath.Join(tools, "k3d")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"image", "import", "--cluster", "git-state-dev", "localhost/controller:dev"}, "image\nimport\n--mode=direct\n--cluster\ngit-state-dev\nlocalhost/controller:dev\n"},
		{[]string{"image", "import", "--mode=tools", "image:dev"}, "image\nimport\n--mode=tools\nimage:dev\n"},
		{[]string{"cluster", "list"}, "cluster\nlist\n"},
	} {
		cmd := exec.Command("bash", append([]string{filepath.Join(root, "hack/k3d-runtime.sh")}, tc.args...)...)
		cmd.Env = append(os.Environ(), "CONTAINER_RUNTIME=podman", "GIT_STATE_K3D_BINARY="+binary)
		b, err := cmd.CombinedOutput()
		if err != nil || string(b) != tc.want {
			t.Fatalf("args %v: %q %v", tc.args, b, err)
		}
	}
}
func run(root, tools, action string) (string, error) {
	cmd := exec.Command("bash", filepath.Join(root, "hack/dev-stack.sh"), action)
	cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "FAKE_ACTIONS="+filepath.Join(root, "actions"))
	b, err := cmd.CombinedOutput()
	return string(b), err
}
func TestCleanupIsOwnedAndRepeatable(t *testing.T) {
	root, tools := fixture(t)
	if b, err := run(root, tools, "clean"); err != nil {
		t.Fatal(b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "actions")); !os.IsNotExist(err) {
		t.Fatal("deleted cluster without ownership marker")
	}
	if err := os.MkdirAll(filepath.Join(root, ".dev"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dev/owned"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// A partially created stack has no kubeconfig, credentials, or running helpers.
	if b, err := run(root, tools, "clean"); err != nil {
		t.Fatal(b, err)
	}
	if b, err := run(root, tools, "clean"); err != nil {
		t.Fatal(b, err)
	}
	b, err := os.ReadFile(filepath.Join(root, "actions"))
	if err != nil || string(b) != "git-state-dev\n" {
		t.Fatalf("cleanup touched unexpected clusters: %q %v", b, err)
	}
}
func TestMissingStackCannotSkipE2E(t *testing.T) {
	root, tools := fixture(t)
	b, err := run(root, tools, "check")
	if err == nil || !strings.Contains(b, "make up or make dev") {
		t.Fatal(b, err)
	}
}
func TestPreexistingClusterCannotBeAdopted(t *testing.T) {
	root, tools := fixture(t)
	b, err := run(root, tools, "up")
	if err == nil || !strings.Contains(b, "refusing to adopt") {
		t.Fatal(b, err)
	}
}

func TestGlobalKubeconfigRegistrationAndCleanup(t *testing.T) {
	root, tools := fixture(t)
	t.Setenv("HOME", filepath.Join(root, "home"))
	if err := os.MkdirAll(filepath.Join(root, ".dev"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dev/owned"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	body := `#!/bin/sh
case "$1 $2" in
 "version ") echo 'k3d version v5.9.0' ;;
 "cluster list") echo git-state-dev ;;
 "kubeconfig get") echo 'apiVersion: v1' ;;
 "kubeconfig merge"|"cluster delete") printf '%s\n%s\n' "$KUBECONFIG" "$*" >> "$FAKE_ACTIONS" ;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "k3d"), []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	// Stop after registration, before bootstrap can contact any real cluster.
	if err := os.WriteFile(filepath.Join(tools, "kubectl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$KUBECONFIG\" > \"$FAKE_ACTIONS.client\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if b, err := run(root, tools, "up"); err == nil {
		t.Fatal("expected fixture kubectl to stop bootstrap", b)
	}
	if b, err := run(root, tools, "clean"); err != nil {
		t.Fatal(b, err)
	}
	global := filepath.Join(root, "home/.kube/config")
	b, err := os.ReadFile(filepath.Join(root, "actions"))
	want := global + "\nkubeconfig merge git-state-dev --kubeconfig-merge-default --kubeconfig-switch-context\n" + global + "\ncluster delete git-state-dev\n"
	if err != nil || string(b) != want {
		t.Fatalf("wrong global kubeconfig operations: %q %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(root, "actions.client"))
	if err != nil || string(b) != filepath.Join(root, ".dev/kubeconfig")+"\n" {
		t.Fatalf("helper lost its explicit kubeconfig: %q %v", b, err)
	}
}

func TestExplicitPodmanUsesCompatibleSocket(t *testing.T) {
	root, tools := fixture(t)
	t.Setenv("CONTAINER_RUNTIME", "podman")
	t.Setenv("DOCKER_HOST", "unix:///fixture/podman.sock")
	t.Setenv("DOCKER_SOCK", "")
	if err := os.WriteFile(filepath.Join(tools, "podman"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(tools, "k3d"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(original), "if [ \"$1 $2\" = \"cluster list\" ]; then", "if [ \"$1 $2\" = \"cluster list\" ]; then\n printf '%s\\n%s\\n' \"$DOCKER_HOST\" \"$DOCKER_SOCK\" > \"$FAKE_ACTIONS\"", 1)
	if err = os.WriteFile(filepath.Join(tools, "k3d"), []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	if b, err := run(root, tools, "up"); err == nil || !strings.Contains(b, "refusing to adopt") {
		t.Fatal(b, err)
	}
	b, err := os.ReadFile(filepath.Join(root, "actions"))
	if err != nil || string(b) != "unix:///fixture/podman.sock\n/fixture/podman.sock\n" {
		t.Fatalf("wrong Podman API settings: %q %v", b, err)
	}
}
