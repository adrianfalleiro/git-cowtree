// Package git runs the real git binary and reads the bits of repository
// metadata git-cowtree needs without spawning it.
package git

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/adrianfalleiro/git-cowtree/internal/report"
)

// Bin is the real git binary: never this program (when it's installed as a
// `git` shim), and on macOS never /usr/bin/git, an xcrun trampoline that adds
// about 20ms to every call. COWTREE_REAL_GIT overrides it.
var Bin = findRealGit()

func findRealGit() string {
	if p := os.Getenv("COWTREE_REAL_GIT"); p != "" {
		return p
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		resolved, err := filepath.EvalSymlinks(filepath.Join(dir, "git"))
		if err != nil || resolved == self {
			continue
		}
		if info, err := os.Stat(resolved); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if runtime.GOOS == "darwin" && resolved == "/usr/bin/git" {
			if dev := xcodeDeveloperDir(); dev != "" {
				if _, err := os.Stat(filepath.Join(dev, "usr/bin/git")); err == nil {
					return filepath.Join(dev, "usr/bin/git")
				}
			}
		}
		return resolved
	}
	return "git"
}

// xcodeDeveloperDir finds what `xcode-select -p` would print, without spawning it.
func xcodeDeveloperDir() string {
	if d := os.Getenv("DEVELOPER_DIR"); d != "" {
		return d
	}
	if d, err := os.Readlink("/var/db/xcode_select_link"); err == nil {
		return d
	}
	return "/Library/Developer/CommandLineTools"
}

// Output runs git in dir and returns its stdout without the trailing newline.
// On failure the error includes git's stderr.
func Output(dir string, args ...string) (string, error) {
	defer report.Trace(time.Now(), "git "+strings.Join(args, " "))
	cmd := exec.Command(Bin, append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// Run runs git in dir with its output on stderr, so stdout stays clean.
func Run(dir string, args ...string) error {
	defer report.Trace(time.Now(), "git "+strings.Join(args, " "))
	cmd := exec.Command(Bin, append([]string{"-C", dir}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}

// RunInput runs git in dir with input on stdin and literal (non-glob)
// pathspecs. On failure the error is git's stderr.
func RunInput(dir, input string, args ...string) error {
	defer report.Trace(time.Now(), "git "+strings.Join(args, " "))
	cmd := exec.Command(Bin, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_LITERAL_PATHSPECS=1")
	cmd.Stdin = strings.NewReader(input)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return errors.New(strings.TrimSpace(errb.String()))
	}
	return nil
}

// LinkedWorktrees reads the worktree registry straight from
// <commonDir>/worktrees and maps each worktree id to its checkout path.
func LinkedWorktrees(commonDir string) map[string]string {
	wts := map[string]string{}
	entries, _ := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	for _, e := range entries {
		gitdir, err := os.ReadFile(filepath.Join(commonDir, "worktrees", e.Name(), "gitdir"))
		if err == nil {
			wts[e.Name()] = filepath.Dir(strings.TrimSpace(string(gitdir)))
		}
	}
	return wts
}

// BlobID is the object id git gives a file with this content, in a repository
// whose object format is "sha1" or "sha256".
func BlobID(content []byte, objectFormat string) string {
	h := sha1.New()
	if objectFormat == "sha256" {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
