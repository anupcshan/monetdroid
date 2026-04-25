package kbcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/anupcshan/monetdroid/pkg/kb"
	"github.com/urfave/cli/v3"
)

func NewApp() *cli.Command {
	return &cli.Command{
		Name:  "kb",
		Usage: "Per-repo knowledge base for Claude sessions",
		Description: `A persistent, per-repo store shared across Claude sessions working in this
repo. Holds plain-text files. No tags, metadata, or structured fields.
Subdirectories are supported and created automatically on write.

EXAMPLES:
  List files:                    kb list
  Read a file:                   kb read foo.md
  Read a line range:             kb read foo.md --offset 10 --limit 20
  Search contents:               kb search "some phrase"
  Delete a file:                 kb rm topic/foo.md
  Move/rename:                   kb mv topic/foo.md topic/bar.md

  Write (creates parent dirs). Content on stdin via heredoc:

      kb write topic/foo.md <<'EOF'
      first line
      second line
      EOF

  Append (creates file if missing). Content on stdin via heredoc:

      kb append topic/foo.md <<'EOF'
      another line
      EOF

  Edit a file. First stdin line is the separator (any string not appearing
  on a line by itself in your content); old and new content follow:

      kb edit topic/foo.md <<'EOF'
      ===
      func Foo() {
          return 1
      }
      ===
      func Foo() {
          return 2
      }
      EOF
`,
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List all tracked files (no filter; use 'search' to find content)",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdList(cmd)
				},
			},
			{
				Name:      "read",
				Usage:     "Read a file (optionally --offset N --limit M for a line range)",
				ArgsUsage: "<path>",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "offset", Usage: "Starting line number, 0-indexed"},
					&cli.IntFlag{Name: "limit", Usage: "Number of lines to read, 0 = to end"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdRead(cmd)
				},
			},
			{
				Name:      "edit",
				Usage:     "Edit a file (stdin: separator, old, separator, new; fails if old is not unique unless --all)",
				ArgsUsage: "<path>",
				Description: `Replaces a literal string in a file. Input on stdin:

    <separator>
    <old content>
    <separator>
    <new content>

The first line is the separator, chosen by the caller to be any literal
string that does not appear on a line by itself in your content. No
escaping is needed. A single trailing newline from the heredoc is
stripped automatically.

By default the edit fails if <old content> does not appear exactly once
in the file. Pass --all to replace every occurrence.

Example:

    kb edit topic/foo.md <<'EOF'
    ===
    old text
    possibly spanning lines
    ===
    replacement text
    EOF
`,
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "all", Usage: "Replace all occurrences"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdEdit(cmd)
				},
			},
			{
				Name:      "write",
				Usage:     "Write a file (content on stdin; creates parent dirs, overwrites)",
				ArgsUsage: "<path>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdWrite(cmd)
				},
			},
			{
				Name:      "append",
				Usage:     "Append to a file (content on stdin; creates file and parent dirs if needed)",
				ArgsUsage: "<path>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdAppend(cmd)
				},
			},
			{
				Name:      "rm",
				Usage:     "Delete a file",
				ArgsUsage: "<path>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdRemove(cmd)
				},
			},
			{
				Name:      "mv",
				Usage:     "Move/rename a file",
				ArgsUsage: "<old> <new>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdMove(cmd)
				},
			},
			{
				Name:      "search",
				Usage:     "Search file contents with git grep (basic regex; filenames not matched)",
				ArgsUsage: "<query>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdSearch(cmd)
				},
			},
		},
	}
}

func resolveKB() (*kb.KB, error) {
	return kb.Resolve()
}

func cmdList(cmd *cli.Command) error {
	k, err := resolveKB()
	if err != nil {
		return err
	}
	files, err := k.List()
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Println(f)
	}
	return nil
}

func cmdRead(cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: kb read <path>")
	}
	k, err := resolveKB()
	if err != nil {
		return err
	}
	content, err := k.Read(cmd.Args().First(), int(cmd.Int("offset")), int(cmd.Int("limit")))
	if err != nil {
		return err
	}
	fmt.Print(content)
	return nil
}

func cmdEdit(cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: kb edit <path> [--all]")
	}
	k, err := resolveKB()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	input, err := kb.ParseEditInput(data)
	if err != nil {
		return err
	}
	return k.Edit(cmd.Args().First(), input, cmd.Bool("all"))
}

func cmdWrite(cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: kb write <path>")
	}
	k, err := resolveKB()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	return k.Write(cmd.Args().First(), string(data))
}

func cmdAppend(cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: kb append <path>")
	}
	k, err := resolveKB()
	if err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	return k.Append(cmd.Args().First(), string(data))
}

func cmdRemove(cmd *cli.Command) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("usage: kb rm <path>")
	}
	k, err := resolveKB()
	if err != nil {
		return err
	}
	return k.Remove(cmd.Args().First())
}

func cmdMove(cmd *cli.Command) error {
	if cmd.Args().Len() != 2 {
		return fmt.Errorf("usage: kb mv <old> <new>")
	}
	k, err := resolveKB()
	if err != nil {
		return err
	}
	return k.Move(cmd.Args().Get(0), cmd.Args().Get(1))
}

func cmdSearch(cmd *cli.Command) error {
	if cmd.Args().Len() == 0 {
		return fmt.Errorf("usage: kb search <query>")
	}
	k, err := resolveKB()
	if err != nil {
		return err
	}
	query := strings.Join(cmd.Args().Slice(), " ")
	result, err := k.Search(query)
	if err != nil {
		return err
	}
	fmt.Print(result)
	return nil
}
