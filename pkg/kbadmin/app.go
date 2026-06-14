package kbadmin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anupcshan/monetdroid/pkg/kb"
	"github.com/anupcshan/monetdroid/pkg/kbcli"
	"github.com/urfave/cli/v3"
)

const installSnippet = "## KB (Knowledge Base)\n\n" +
	"This project has a persistent knowledge base accessible via the `kb` CLI.\n" +
	"\n" +
	"### When to use it\n" +
	"\n" +
	"- **Resuming work.** If the user refers to a project by name, first\n" +
	"  `kb search` or `kb list` to find an existing entry under\n" +
	"  `projects/<slug>.md`, then `kb read` it to recover context before\n" +
	"  doing anything else.\n" +
	"- **Starting new work.** When the user asks you to plan or build\n" +
	"  something new, create `projects/<slug>.md` with a short plan and\n" +
	"  current status. Checkpoint meaningful progress with `kb append`\n" +
	"  or `kb edit` (enough detail that a future session can resume).\n" +
	"\n" +
	"### Conventions\n" +
	"\n" +
	"- One file per project at `projects/<slug>.md`.\n" +
	"- Entries should include a *Status* section listing what's done and\n" +
	"  what's next, so \"resume\" calls can pick up where work left off.\n" +
	"- **Keep kb current as work progresses.** When a phase ships, a\n" +
	"  finding changes, or a decision solidifies, update the relevant kb\n" +
	"  documents immediately. Don't wait to be asked. Stale kb is as\n" +
	"  harmful as stale code.\n" +
	"- Use kb to record project plans and progress. Do not use Claude\n" +
	"  Code's built-in plan mode (EnterPlanMode / ExitPlanMode) for\n" +
	"  project tracking; write the plan directly to `projects/<slug>.md`.\n"

const includeLine = "@kb.md"

// snippetWithHelp returns the kb.md content: the usage guide followed by the
// full `kb --help`. Embedding it keeps the command reference in context
// (kb.md is @-included every session), so the help does not need running at
// the prompt, where truncation once hid the `edit` subcommand.
func snippetWithHelp() string {
	return installSnippet +
		"\n### Command reference\n\n" +
		"Full `kb --help` output:\n\n" +
		"```\n" + renderKbHelp() + "\n```\n"
}

// renderKbHelp renders the `kb` root help to a string by pointing the
// command's writer at a buffer and driving the same "--help" path the `kb`
// binary uses (Run resolves the command tree and flags, so the output
// matches `kb --help` exactly, including the COMMANDS table).
func renderKbHelp() string {
	app := kbcli.NewApp()
	var buf bytes.Buffer
	app.Writer = &buf
	app.ErrWriter = &buf
	_ = app.Run(context.Background(), []string{"kb", "--help"})
	return strings.TrimRight(buf.String(), "\n")
}

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
						fmt.Print(snippetWithHelp())
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
	if err := writeAtomic(kbMdPath, []byte(snippetWithHelp())); err != nil {
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
