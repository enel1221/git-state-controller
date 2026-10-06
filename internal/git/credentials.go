package git

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

type Credentials struct{ Username, Password string }

func ValidateDestination(rawURL, branch, file string, allowHTTP bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
		return fmt.Errorf("repository must be a clone URL without credentials, query or fragment")
	}
	if u.Scheme != "https" && (!allowHTTP || u.Scheme != "http") {
		return fmt.Errorf("repository requires HTTPS (HTTP requires the explicit development flag)")
	}
	if branch == "" || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "refs/") || isSHA(branch) {
		return fmt.Errorf("branch must be a branch name")
	}
	if err := plumbing.NewBranchReferenceName(branch).Validate(); err != nil {
		return fmt.Errorf("invalid branch name")
	}
	if file == "" || strings.HasPrefix(file, "/") || strings.ContainsAny(file, `\:*?[]{}!`) || path.Clean(file) != file || (!strings.HasSuffix(file, ".yaml") && !strings.HasSuffix(file, ".yml")) {
		return fmt.Errorf("path must be a clean relative YAML filename without glob metacharacters")
	}
	for _, component := range strings.Split(file, "/") {
		if component == ".git" || component == ".." || component == "." || component == "" {
			return fmt.Errorf("unsafe path component")
		}
	}
	return nil
}
func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
