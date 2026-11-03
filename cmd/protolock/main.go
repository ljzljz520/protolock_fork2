package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nilslice/protolock"
)

const info = `Track your .proto files and prevent changes to messages and services which impact API compatibility.

Copyright Steve Manuel <nilslice@gmail.com>
Released under the BSD-3-Clause license.
`

const usage = `
Usage:
	protolock <command> [options]

Commands:
	-h, --help, help	display the usage information for protolock
	init			initialize a proto.lock file from current tree
	status			check for breaking changes and report conflicts
	commit			rewrite proto.lock file with current tree if no conflicts (--force to override)

Options:
	--strict [true]		enable strict mode and enforce all built-in rules
	--debug	[false]		enable debug mode and output debug messages
	--ignore 		comma-separated list of filepaths to ignore
	--force [false]		forces commit to rewrite proto.lock file and disregards warnings
	--plugins 		comma-separated list of executable protolock plugin names
	--lockdir [.]		directory of proto.lock file
	--protoroot [.]		root of directory tree containing proto files
	--uptodate [false]	enforce that proto.lock file is up-to-date with proto files
`

var (
	options   = flag.NewFlagSet("options", flag.ExitOnError)
	debug     = options.Bool("debug", false, "toggle debug mode for verbose output")
	strict    = options.Bool("strict", true, "enable strict mode and enforce all built-in rules")
	ignore    = options.String("ignore", "", "comma-separated list of filepaths to ignore")
	force     = options.Bool("force", false, "force commit to rewrite proto.lock file and disregard warnings")
	plugins   = options.String("plugins", "", "comma-separated list of executable protolock plugin names")
	lockDir   = options.String("lockdir", ".", "directory of proto.lock file")
	protoRoot = options.String("protoroot", ".", "root of directory tree containing proto files")
	upToDate  = options.Bool("uptodate", false, "enforce that proto.lock file is up-to-date with proto files")
)

func main() {
	// exit if no command (i.e. help, -h, --help, init, status, or commit)
	if len(os.Args) < 2 {
		fmt.Print(info + usage)
		os.Exit(0)
	}

	// parse and set options flags
	options.Parse(os.Args[2:])
	protolock.SetDebug(*debug)
	protolock.SetStrict(*strict)

	cfg, err := protolock.NewConfig(
		*lockDir,
		*protoRoot,
		*ignore,
		*upToDate,
		*debug,
	)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// switch through known commands
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)

	case "init":
		r, err := protolock.Init(*cfg)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		err = saveToLockFile(*cfg, r)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

	case "commit":
		// if force option is false (default), then disallow commit unless
		// the exact same status gate used by the `status` command passes:
		// all built-in rules and every plugin succeed with no conflicts.
		if !*force {
			code, err := runStatus(cfg)
			if err != nil {
				fmt.Println(logPrefix, "error:", err)
			}
			if code != 0 || err != nil {
				if code != 0 {
					os.Exit(code)
				}
				os.Exit(1)
			}
		}

		r, err := protolock.Commit(*cfg)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		err = saveToLockFile(*cfg, r)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

	case "status":
		code, err := runStatus(cfg)
		if err != nil {
			fmt.Println(logPrefix, "error:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
		if err != nil {
			os.Exit(1)
		}

	default:
		os.Exit(0)
	}
}

// runStatus is the single pass/fail gate shared by the `status` command and a
// non-forced `commit`. It only returns code 0 with a nil error when every
// built-in rule and every specified plugin completes successfully with no
// conflicts. Plugin failures and warnings are reported together: warnings
// (built-in and from plugins that succeeded) are written first, so they are
// not lost when another plugin fails.
func runStatus(cfg *protolock.Config) (int, error) {
	report, err := protolock.Status(*cfg)
	if err == protolock.ErrOutOfDate {
		fmt.Println(logPrefix, "error:", err, "run 'protolock commit'")
		// only exit if flag provided for backwards compatibility
		if cfg.UpToDate {
			return 2, nil
		}
		// don't report the error twice
		err = nil
	}
	if err != protolock.ErrWarningsFound && err != nil {
		return 1, err
	}

	// if plugins are provided, attempt to execute each as an executable
	// located in the user's OS executable path as reported by stdlib's
	// exec.LookPath func, or as a ".wasm" Extism plugin at the given path.
	// runPlugins appends every plugin's warnings and returns an error if any
	// plugin could not be run, timed out, produced unusable output, or
	// reported its own error.
	if *plugins != "" {
		report, err = runPlugins(*plugins, report, *debug)
	}

	// write all conflicts (built-in and plugin) before returning, so they
	// remain visible even when a plugin error is also returned
	code, err := protolock.HandleReport(report, os.Stdout, err)
	if err != protolock.ErrWarningsFound && err != nil {
		return code, err
	}

	return code, nil
}

func saveToLockFile(cfg protolock.Config, r io.Reader) error {
	lockfile, err := os.Create(cfg.LockFilePath())
	if err != nil {
		return err
	}
	defer lockfile.Close()

	_, err = io.Copy(lockfile, r)
	if err != nil {
		return err
	}

	return nil
}
