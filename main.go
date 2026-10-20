package main

import (
	"embed"
	"fmt"
	"os"

	"wmail/internal/cli"
)

// Only these files are served; nothing else in the working directory is reachable over HTTP.
//
//go:embed index.html login.html settings.html assets
var web embed.FS

//go:embed worker/worker.js
var workerJS []byte

func main() {
	if err := cli.Run(os.Args[1:], cli.Assets{Web: web, WorkerJS: workerJS}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
