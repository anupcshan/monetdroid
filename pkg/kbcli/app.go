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
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List files in this repo's KB",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdList(cmd)
				},
			},
			{
				Name:  "read",
				Usage: "Read a file",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "offset", Usage: "Starting line number"},
					&cli.IntFlag{Name: "limit", Usage: "Number of lines to read"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdRead(cmd)
				},
			},
			{
				Name:  "edit",
				Usage: `Edit a file (stdin: {"old": "...", "new": "..."}, --all to replace all occurrences)`,
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "all", Usage: "Replace all occurrences"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdEdit(cmd)
				},
			},
			{
				Name:  "write",
				Usage: "Write a file (content on stdin, creates/overwrites)",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdWrite(cmd)
				},
			},
			{
				Name:  "append",
				Usage: "Append to a file (content on stdin, creates if needed)",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdAppend(cmd)
				},
			},
			{
				Name:  "rm",
				Usage: "Delete a file",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdRemove(cmd)
				},
			},
			{
				Name:  "mv",
				Usage: "Move/rename a file",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return cmdMove(cmd)
				},
			},
			{
				Name:  "search",
				Usage: "Search across KB",
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
