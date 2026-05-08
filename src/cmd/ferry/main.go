// Command ferry is the entry point for the ferry CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/horsenuggets/ferry/src/server"
	"github.com/horsenuggets/ferry/src/version"
)

const usage = `ferry - fault-tolerant chunked file uploader

Usage:
  ferry [flags]
  ferry <command> [args]

Commands:
  listen    Run the ferry receiver
  upload    Upload a file to a ferry receiver (not implemented yet)
  status    Query the status of an upload (not implemented yet)

Flags:
  --help       Show this help message
  --version    Print version and exit

Listen flags:
  --config <path>  Path to config JSON (default /etc/ferry/config.json)
`

const defaultConfigPath = "/etc/ferry/config.json"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ferry", flag.ContinueOnError)
	// Silence the stdlib flag package's automatic help output; we render our own.
	fs.SetOutput(io.Discard)
	showVersion := fs.Bool("version", false, "print version and exit")
	showHelp := fs.Bool("help", false, "show help and exit")

	// Split flags from subcommand: anything starting with "-" is a flag.
	var flagArgs []string
	var rest []string
	for i, a := range args {
		if len(a) > 0 && a[0] == '-' {
			flagArgs = append(flagArgs, a)
			continue
		}
		rest = args[i:]
		break
	}

	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "ferry: %v\n\n", err)
		fmt.Fprint(stderr, usage)
		return 1
	}

	if *showHelp {
		fmt.Fprint(stdout, usage)
		return 0
	}

	if *showVersion {
		fmt.Fprintln(stdout, version.Version)
		return 0
	}

	if len(rest) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	switch rest[0] {
	case "listen":
		return runListen(rest[1:], stderr)
	case "upload", "status":
		fmt.Fprintf(stderr, "ferry %s: not implemented yet\n", rest[0])
		return 1
	default:
		fmt.Fprintf(stderr, "ferry: unknown command %q\n\n", rest[0])
		fmt.Fprint(stderr, usage)
		return 1
	}
}

func runListen(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("ferry listen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultConfigPath, "path to config JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "ferry listen: %v\n", err)
		return 1
	}

	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "ferry listen: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	srv, err := server.New(cfg, version.Version, logger)
	if err != nil {
		fmt.Fprintf(stderr, "ferry listen: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx, version.Version); err != nil {
		fmt.Fprintf(stderr, "ferry listen: %v\n", err)
		return 1
	}
	return 0
}
