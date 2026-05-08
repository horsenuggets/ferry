// Command ferry is the entry point for the ferry CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/horsenuggets/ferry/src/client"
	"github.com/horsenuggets/ferry/src/server"
	"github.com/horsenuggets/ferry/src/version"
)

const usage = `ferry - fault-tolerant chunked file uploader

Usage:
  ferry [flags]
  ferry <command> [args]

Commands:
  listen    Run the ferry receiver
  upload    Upload a file to a ferry receiver
  status    Query the status of an upload

Flags:
  --help       Show this help message
  --version    Print version and exit

Run "ferry <command> --help" for command-specific flags.
`

const uploadUsage = `ferry upload - upload a file to a ferry receiver

Usage:
  ferry upload <file> [flags]

Flags:
  --to <url>             Peer URL (overrides FERRY_URL / config default_url)
  --as <name>            Remote filename (becomes Upload-Metadata "filename")
  --namespace <ns>       Server namespace (overrides FERRY_NAMESPACE)
  --token <bearer>       Bearer token (overrides FERRY_TOKEN)
  --config <path>        Config path (overrides FERRY_CONFIG; default ~/.config/ferry/config.json)
  --json                 Emit JSON progress + completion to stdout
  --idempotency-key <k>  Optional Idempotency-Key for the create POST
  --chunk-size <bytes>   PATCH body size (default 4194304, max 67108864)
  --checksum <algo>      Per-chunk Upload-Checksum: crc32c (default), sha256, or none
  --no-checksum          Shortcut for --checksum=none
  --help                 Show this help
`

const statusUsage = `ferry status - query the status of an upload

Usage:
  ferry status [flags]

Flags:
  --to <url>             Peer URL (overrides FERRY_URL)
  --upload <id>          Upload id (required)
  --namespace <ns>       Server namespace (overrides FERRY_NAMESPACE)
  --token <bearer>       Bearer token (overrides FERRY_TOKEN)
  --config <path>        Config path (overrides FERRY_CONFIG)
  --json                 Emit JSON to stdout
  --help                 Show this help
`

const defaultListenConfigPath = "/etc/ferry/config.json"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ferry", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	showVersion := fs.Bool("version", false, "print version and exit")
	showHelp := fs.Bool("help", false, "show help and exit")

	var flagArgs, rest []string
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
	case "upload":
		return runUpload(rest[1:], stdout, stderr)
	case "status":
		return runStatus(rest[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ferry: unknown command %q\n\n", rest[0])
		fmt.Fprint(stderr, usage)
		return 1
	}
}

func runListen(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("ferry listen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultListenConfigPath, "path to config JSON")
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

// resolveClientConfig reads --config (or $FERRY_CONFIG, or the default path)
// and returns the parsed Config plus a Resolved struct after layering env
// vars and CLI flags.
func resolveClientConfig(flagURL, flagNS, flagToken, flagConfig string, flagChunk int64) (client.Resolved, error) {
	cfgPath := flagConfig
	if cfgPath == "" {
		cfgPath = os.Getenv("FERRY_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = client.DefaultConfigPath()
	}
	cfg, err := client.LoadConfig(cfgPath)
	if err != nil {
		return client.Resolved{}, err
	}
	return client.Resolve(client.ResolveInput{
		FlagURL:       flagURL,
		FlagNamespace: flagNS,
		FlagToken:     flagToken,
		FlagChunkSize: flagChunk,
		Config:        cfg,
		Env:           os.Getenv,
	})
}

func runUpload(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ferry upload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "peer URL")
	as := fs.String("as", "", "remote filename")
	ns := fs.String("namespace", "", "server namespace")
	tok := fs.String("token", "", "bearer token")
	cfgPath := fs.String("config", "", "config file path")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	idem := fs.String("idempotency-key", "", "idempotency key")
	chunk := fs.Int64("chunk-size", 0, "PATCH body size in bytes")
	checksum := fs.String("checksum", "crc32c", "per-chunk Upload-Checksum algo")
	noChecksum := fs.Bool("no-checksum", false, "disable per-chunk Upload-Checksum")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, uploadUsage)
			return 0
		}
		fmt.Fprintf(stderr, "ferry upload: %v\n", err)
		return 2
	}
	if *help {
		fmt.Fprint(stdout, uploadUsage)
		return 0
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "ferry upload: expected exactly one file argument")
		fmt.Fprint(stderr, uploadUsage)
		return 2
	}
	filePath := rest[0]

	resolved, err := resolveClientConfig(*to, *ns, *tok, *cfgPath, *chunk)
	if err != nil {
		fmt.Fprintf(stderr, "ferry upload: %v\n", err)
		return 2
	}

	c := client.NewClient(resolved.URL, resolved.Token)

	// Pick progress mode based on flags + TTY.
	mode := client.AutoProgressMode()
	if *jsonOut {
		mode = client.ProgressJSON
	}

	// Determine total size for the progress reporter. We restat here
	// (Chunker also stats) so the bar is sized before the first byte goes
	// over the wire.
	size, err := fileSize(filePath)
	if err != nil {
		fmt.Fprintf(stderr, "ferry upload: %v\n", err)
		return 1
	}
	prog := client.NewProgress(size, mode, stderr, stdout)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cksumAlgo := *checksum
	if *noChecksum {
		cksumAlgo = "none"
	}
	res, err := c.Upload(ctx, filePath, client.UploadOptions{
		Namespace:      resolved.Namespace,
		RemoteName:     *as,
		ChunkSize:      resolved.ChunkSize,
		IdempotencyKey: *idem,
		Progress:       prog,
		Checksum:       cksumAlgo,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ferry upload: %v\n", err)
		return 1
	}

	// Non-JSON modes already printed a friendly "done" line; emit a
	// machine-parseable summary when JSON is off too, so users can pipe.
	if mode != client.ProgressJSON {
		fmt.Fprintf(stdout, "%s  %d bytes -> %s\n", res.UploadID, res.Size, res.LocationURL)
	}
	return 0
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ferry status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "peer URL")
	uploadID := fs.String("upload", "", "upload id")
	ns := fs.String("namespace", "", "server namespace")
	tok := fs.String("token", "", "bearer token")
	cfgPath := fs.String("config", "", "config file path")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, statusUsage)
			return 0
		}
		fmt.Fprintf(stderr, "ferry status: %v\n", err)
		return 2
	}
	if *help {
		fmt.Fprint(stdout, statusUsage)
		return 0
	}
	if *uploadID == "" {
		fmt.Fprintln(stderr, "ferry status: --upload is required")
		fmt.Fprint(stderr, statusUsage)
		return 2
	}

	resolved, err := resolveClientConfig(*to, *ns, *tok, *cfgPath, 0)
	if err != nil {
		fmt.Fprintf(stderr, "ferry status: %v\n", err)
		return 2
	}

	c := client.NewClient(resolved.URL, resolved.Token)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := c.Status(ctx, resolved.Namespace, *uploadID)
	if err != nil {
		fmt.Fprintf(stderr, "ferry status: %v\n", err)
		return 1
	}

	pct := 0.0
	if st.Size > 0 {
		pct = float64(st.Offset) / float64(st.Size) * 100.0
	}
	state := "in-progress"
	if st.Complete {
		state = "complete"
	}

	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{
			"upload_id": *uploadID,
			"url":       st.UploadURL,
			"offset":    st.Offset,
			"size":      st.Size,
			"percent":   pct,
			"state":     state,
		})
		return 0
	}
	fmt.Fprintf(stdout, "%s  %d / %d bytes (%.1f%%)  %s\n", *uploadID, st.Offset, st.Size, pct, state)
	return 0
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if st.IsDir() {
		return 0, fmt.Errorf("%s is a directory", path)
	}
	return st.Size(), nil
}
