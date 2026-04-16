package main

import (
	"context"
	"fmt"
	"os"

	"github.com/anupcshan/monetdroid/pkg/kbcli"
)

func main() {
	if err := kbcli.NewApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
