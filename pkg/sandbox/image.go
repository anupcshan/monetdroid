package sandbox

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sync"
)

const sandboxImage = "localhost/monetdroid-sandbox"

//go:embed Dockerfile
var dockerfile string

//go:embed initcmd/main.go
var initSource string

type hostUser struct {
	uid  int
	gid  int
	name string
	home string
}

func hostIdentity() (hostUser, error) {
	u, err := user.Current()
	if err != nil {
		return hostUser{}, fmt.Errorf("host user: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return hostUser{}, fmt.Errorf("host home: %w", err)
	}
	if !filepath.IsAbs(home) {
		return hostUser{}, fmt.Errorf("host home %q is not absolute", home)
	}
	return hostUser{uid: os.Getuid(), gid: os.Getgid(), name: u.Username, home: home}, nil
}

var buildMu sync.Mutex

// buildImage builds the sandbox image. Podman caches the layers, so every
// session builds and a changed Dockerfile or init source is picked up without
// a version to track.
func buildImage(ctx context.Context, podman string, user hostUser) error {
	buildMu.Lock()
	defer buildMu.Unlock()

	dir, err := os.MkdirTemp("", "monetdroid-image-")
	if err != nil {
		return fmt.Errorf("image build context: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("image build context: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(initSource), 0o644); err != nil {
		return fmt.Errorf("image build context: %w", err)
	}
	cmd := exec.CommandContext(ctx, podman, "build",
		"-t", sandboxImage,
		"--build-arg", fmt.Sprintf("USER_ID=%d", user.uid),
		"--build-arg", fmt.Sprintf("GROUP_ID=%d", user.gid),
		"--build-arg", "USER_NAME="+user.name,
		"--build-arg", "HOME_PATH="+user.home,
		"-f", filepath.Join(dir, "Dockerfile"),
		dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman build: %w\n%s", err, out)
	}
	return nil
}
