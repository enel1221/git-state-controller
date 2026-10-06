// Bootstrap uses Forgejo APIs only to create the disposable fixture.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type credentials struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	ArgoUsername string `json:"argoUsername"`
	ArgoPassword string `json:"argoPassword"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	port, err := os.ReadFile(".dev/forgejo-port")
	if err != nil {
		return err
	}
	base := "http://127.0.0.1:" + strings.TrimSpace(string(port))
	var creds credentials
	b, err := os.ReadFile(".dev/credentials.json")
	if errors.Is(err, os.ErrNotExist) {
		secret := make([]byte, 24)
		if _, err = rand.Read(secret); err != nil {
			return err
		}
		creds = credentials{Username: "demo", Password: hex.EncodeToString(secret), ArgoUsername: "admin"}
		b, _ = json.MarshalIndent(creds, "", "  ")
		if err = os.WriteFile(".dev/credentials.json", b, 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err = json.Unmarshal(b, &creds); err != nil {
		return err
	}
	request := func(method, path string, body []byte) (int, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+"/api/v1"+path, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.SetBasicAuth(creds.Username, creds.Password)
		req.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			return 0, errors.New("cannot reach local Forgejo API")
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode, nil
	}
	status, err := request("GET", "/user", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", os.Getenv("KUBECONFIG"), "--context", "k3d-git-state-dev", "exec", "-i", "-n", "forgejo", "deployment/forgejo", "--", "su", "git", "-s", "/bin/sh", "-c", `read -r fixture_password; forgejo --config /data/gitea/conf/app.ini admin user create --username demo --password "$fixture_password" --email demo@example.invalid --admin --must-change-password=false`)
		command.Stdin = strings.NewReader(creds.Password + "\n")
		if err = command.Run(); err != nil {
			return errors.New("forgejo account initialization failed; preserve .dev credentials for an existing cluster, or reset with make clean")
		}
		if status, err = request("GET", "/user", nil); err != nil || status != 200 {
			return errors.New("forgejo fixture authentication failed")
		}
	}
	status, err = request("GET", "/repos/demo/resources", nil)
	if err != nil {
		return err
	}
	if status == 404 {
		body := []byte(`{"name":"resources","private":true,"auto_init":true,"default_branch":"main","readme":"Default"}`)
		status, err = request("POST", "/user/repos", body)
		if err != nil || status != 201 {
			return errors.New("cannot initialize private Forgejo repository")
		}
	} else if status != 200 {
		return errors.New("cannot inspect Forgejo repository")
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")}, &clientcmd.ConfigOverrides{CurrentContext: "k3d-git-state-dev"}).ClientConfig()
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	repoURL := "http://forgejo.forgejo.svc.cluster.local:3000/demo/resources.git"
	secrets := []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "forgejo-writer", Namespace: "git-state-system"}, Data: map[string][]byte{"username": []byte(creds.Username), "password": []byte(creds.Password)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "forgejo-resources", Namespace: "argocd", Labels: map[string]string{"argocd.argoproj.io/secret-type": "repository"}}, Data: map[string][]byte{"type": []byte("git"), "url": []byte(repoURL), "username": []byte(creds.Username), "password": []byte(creds.Password)}},
	}
	for _, secret := range secrets {
		existing := &corev1.Secret{}
		err = c.Get(ctx, client.ObjectKeyFromObject(secret), existing)
		if apierrors.IsNotFound(err) {
			err = c.Create(ctx, secret)
		} else if err == nil {
			secret.ResourceVersion = existing.ResourceVersion
			err = c.Update(ctx, secret)
		}
		if err != nil {
			return errors.New("cannot configure local repository credentials Secrets")
		}
	}
	if err = ensureApplicationSet(ctx, c, "dev/bootstrap/applicationset.yaml"); err != nil {
		return err
	}
	initial := &corev1.Secret{}
	if err = c.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: "argocd-initial-admin-secret"}, initial); err == nil {
		creds.ArgoPassword = string(initial.Data["password"])
	}
	b, _ = json.MarshalIndent(creds, "", "  ")
	return os.WriteFile(".dev/credentials.json", b, 0600)
}

// Refresh static template fields without owning or applying the live generator list.
func ensureApplicationSet(ctx context.Context, c client.Client, templatePath string) error {
	b, err := os.ReadFile(templatePath)
	if err != nil {
		return err
	}
	desired := &unstructured.Unstructured{}
	if err = yaml.Unmarshal(b, &desired.Object); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(desired.GroupVersionKind())
		err := c.Get(ctx, client.ObjectKeyFromObject(desired), current)
		if apierrors.IsNotFound(err) {
			return c.Create(ctx, desired.DeepCopy())
		}
		if err != nil {
			return err
		}
		before := current.DeepCopy()
		for _, field := range []string{"template", "goTemplate", "goTemplateOptions", "syncPolicy"} {
			value, found, err := unstructured.NestedFieldCopy(desired.Object, "spec", field)
			if err != nil {
				return err
			}
			if found {
				if err = unstructured.SetNestedField(current.Object, value, "spec", field); err != nil {
					return err
				}
			}
		}
		return c.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}
