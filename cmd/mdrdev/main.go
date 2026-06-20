// Command mdrdev runs curated monetdroid development operations. See the
// package docs for available subcommands.
package main

import (
	"os"

	"github.com/anupcshan/monetdroid/pkg/mdrdev"
)

func main() {
	os.Exit(mdrdev.Run(os.Args[1:]))
}
