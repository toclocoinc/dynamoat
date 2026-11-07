package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// version and commit are stamped at build time with
//
//	-ldflags "-X main.version=... -X main.commit=..."
//
// and default to "dev" and "unknown" for local builds.
var (
	version = "dev"
	commit  = "unknown"
)

// buildCommit returns the commit stamped at build time, falling back to the
// VCS revision recorded in the binary's build info when it was not set.
func buildCommit() string {
	if commit != "unknown" {
		return commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return commit
}

// versionString is what --version prints.
func versionString() string {
	return fmt.Sprintf("dynamoat %s (%s, %s)", version, buildCommit(), runtime.Version())
}

const usage = `usage: dynamoat [--version]

dynamoat is a DynamoDB API-compatible server. Configuration is by environment
variable (see docs/running.md); there are no other command-line flags.

  --version  print the version, commit and Go version, then exit
`

// handleArgs handles the command line. It returns a non-negative exit code
// when the process should stop (for --version, or an unknown argument), and
// -1 to continue starting the server.
func handleArgs(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return -1
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, versionString())
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}
