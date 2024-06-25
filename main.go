package main

import (
	"os"
	"runtime/debug"

	"github.com/magnusbaeck/logstash-filter-verifier/v2/internal/app"
)

var GitSummary = "(unknown)"

func main() {
	info, _ := debug.ReadBuildInfo()
	for _, kv := range info.Settings {
		switch kv.Key {
		case "vcs":
			GitSummary = kv.Value + " "
		case "vcs.revision":
			GitSummary += kv.Value + " "
		case "vcs.time":
			GitSummary += kv.Value + " "
		case "vcs.modified":
			if kv.Value == "true" {
				GitSummary += " dirty"
			}
		}
	}
	exitCode := app.Execute(GitSummary, os.Stdout, os.Stderr)
	os.Exit(exitCode)
}
