package kbadmin

import (
	"context"
	"fmt"
	"os"

	"github.com/anupcshan/monetdroid/pkg/kb"
	"github.com/urfave/cli/v3"
)

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
				Name:  "install",
				Usage: "Print a snippet to add to AGENTS.md or CLAUDE.md",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					fmt.Println(`Add the following to your AGENTS.md or CLAUDE.md:

## KB (Knowledge Base)

This project has a persistent knowledge base accessible via the ` + "`kb`" + ` CLI.
Run ` + "`kb --help`" + ` for usage.`)
					return nil
				},
			},
		},
	}
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
