package main

import (
	"github.com/anupcshan/monetdroid/tools/analyzers/exportedmutex"

	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(exportedmutex.Analyzer)
}
