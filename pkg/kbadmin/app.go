package kbadmin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anupcshan/monetdroid/pkg/kb"
	"github.com/urfave/cli/v3"
)

const installSnippet = "## KB (Knowledge Base)\n\n" +
	"This project has a persistent knowledge base accessible via the `kb` CLI.\n" +
	"Run `kb --help` for usage.\n"

const includeLine = "@kb.md"

func NewApp() *cli.Command {
	return &cli.Command{
		Name:  "kbadmin",
		Usage: "Administer the KB system",
		Commands: []*cli.Command{
			{
				Name:  "mode",
				Usage: "Get or set KB mode (readonly, readwrite)",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdMode(cmd)
				},
			},
			{
				Name:      "install",
				Usage:     "Print or install the KB snippet into an AGENTS.md / CLAUDE.md file",
				ArgsUsage: "[path]",
				Description: "With no arg, prints the snippet to stdout.\n" +
					"With a path arg, creates the file if missing, idempotently appends\n" +
					"an `@kb.md` include line, and writes `kb.md` with the snippet\n" +
					"adjacent to the target. Rejects symlinked targets.",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if cmd.Args().Len() == 0 {
						fmt.Println("Add the following to your AGENTS.md or CLAUDE.md:")
						fmt.Println()
						fmt.Print(installSnippet)
						return nil
					}
					return InstallToFile(cmd.Args().First())
				},
			},
		},
	}
}

// InstallToFile installs the KB snippet alongside targetPath. It writes
// `kb.md` next to targetPath with the full snippet (overwriting any prior
// kb.md), and idempotently appends `@kb.md` to targetPath. Creates
// targetPath if missing. Rejects if targetPath exists as a symlink.
func InstallToFile(targetPath string) error {
	if info, err := os.Lstat(targetPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; run install on the real file", targetPath)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", targetPath)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	kbMdPath := filepath.Join(filepath.Dir(targetPath), "kb.md")
	if err := writeAtomic(kbMdPath, []byte(installSnippet)); err != nil {
		return fmt.Errorf("write %s: %w", kbMdPath, err)
	}

	return appendIncludeLine(targetPath, includeLine)
}

func appendIncludeLine(path, line string) error {
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	for l := range strings.SplitSeq(string(content), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}

	var toAppend strings.Builder
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		toAppend.WriteString("\n")
	}
	if len(content) > 0 {
		toAppend.WriteString("\n")
	}
	toAppend.WriteString(line)
	toAppend.WriteString("\n")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(toAppend.String())
	return err
}

func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kb.md.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func cmdMode(cmd *cli.Command) error {
	k, err := kb.Resolve()
	if err != nil {
		return err
	}

	if cmd.Args().Len() == 0 {
		if k.IsReadOnly() {
			fmt.Println("readonly")
		} else {
			fmt.Println("readwrite")
		}
		return nil
	}

	switch cmd.Args().First() {
	case "readonly":
		return os.WriteFile(k.ReadOnlyPath(), []byte{}, 0644)
	case "readwrite":
		if err := os.Remove(k.ReadOnlyPath()); os.IsNotExist(err) {
			return nil
		} else {
			return err
		}
	default:
		return fmt.Errorf("unknown mode: %s (use 'readonly' or 'readwrite')", cmd.Args().First())
	}
}
