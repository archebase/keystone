// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Command keystone-import-mcap imports one local MCAP through Keystone's Data Gateway.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"archebase.com/keystone-edge/internal/mcapimport"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) (exitCode int) {
	cfg, err := mcapimport.ParseConfig(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if usageErr := printUsage(os.Stdout); usageErr != nil {
				return 1
			}
			return 0
		}
		if _, writeErr := fmt.Fprintf(os.Stderr, "configuration error: %v\n\n", err); writeErr != nil {
			return 1
		}
		if usageErr := printUsage(os.Stderr); usageErr != nil {
			return 1
		}
		return 2
	}

	client, err := mcapimport.NewGatewayClient(cfg)
	if err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "initialize Data Gateway client: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			if _, writeErr := fmt.Fprintf(os.Stderr, "warning: close Data Gateway client: %v\n", closeErr); writeErr != nil {
				exitCode = 1
			}
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var progressMu sync.Mutex
	var progressErr error
	progress := func(format string, values ...any) {
		progressMu.Lock()
		defer progressMu.Unlock()
		if progressErr == nil {
			_, progressErr = fmt.Fprintf(os.Stderr, format+"\n", values...)
		}
	}
	runner := mcapimport.Runner{
		Control:  client,
		Uploader: mcapimport.TOSUploader{Progress: progress},
		Progress: progress,
	}
	result, err := runner.Run(ctx, cfg)
	if err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "import failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	progressMu.Lock()
	finalProgressErr := progressErr
	progressMu.Unlock()
	if finalProgressErr != nil {
		return 1
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "encode result: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

func printUsage(output io.Writer) error {
	_, err := fmt.Fprint(output, `Usage:
  keystone-import-mcap --endpoint HOST:PORT --file FILE.mcap --workspace-id ID --dc-plan-id ID --task-id ID [device authentication]

Device authentication (choose one):
  KEYSTONE_IMPORT_DEVICE_API_KEY=KEY ... --device-id ID
  --device-credentials-file FILE
  KEYSTONE_IMPORT_DEVICE_AUTH_TOKEN=TOKEN ... --device-name NAME --device-credentials-file NEW_FILE

Optional:
  --capture-id UUID       generated automatically when omitted
  --duration-sec SECONDS  positive recording duration
  --camera-serial SERIAL  optional camera serial for calibration association
  --parallel N            concurrent TOS parts (default 4, maximum 32)
  --rpc-timeout DURATION  per-gRPC-call timeout (default 30s)
  --tls=false             disable TLS for a local endpoint
  --tls-ca-file FILE      use a custom CA bundle
  --tls-server-name NAME  override TLS certificate server name

Secrets are accepted only through environment variables or a 0600 credentials file.
`)
	return err
}
