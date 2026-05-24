// rein is the credential broker CLI. In Phase 0 it has one subcommand
// (credential-helper) and reads its target repository from REIN_* env vars.
// Future checkpoints add sessions, scope ceilings, prompts, and a top-level
// `rein run` wrapper.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/broker"
	"github.com/TomHennen/rein/internal/githubapp"
)

const (
	// mintTimeout caps each installation-token mint. Git users feel this
	// latency directly when the helper is invoked, so keep it tight.
	mintTimeout = 5 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "credential-helper":
		if len(os.Args) < 3 {
			usage()
			os.Exit(2)
		}
		if err := runCredentialHelper(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "rein credential-helper: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "rein: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rein credential-helper {get|store|erase}")
}

// runCredentialHelper wires env-derived config to the broker. All errors
// returned here are programming/config errors — credential-mint failures
// are handled inside the broker per TM-G8.
func runCredentialHelper(action string) error {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}

	logger, closeLog, err := openLog()
	if err != nil {
		return err
	}
	defer closeLog()

	client, err := githubapp.NewClient(cfg.app)
	if err != nil {
		return err
	}

	mint := broker.MintFunc(func(ctx context.Context) (string, time.Time, error) {
		return client.MintReadOnlyToken(ctx)
	})

	return broker.RunCredentialHelper(action, os.Stdin, os.Stdout, broker.Config{
		Mint:        mint,
		MintTimeout: mintTimeout,
		Logger:      logger,
	})
}

type config struct {
	app githubapp.Config
}

func loadConfigFromEnv() (config, error) {
	required := []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
		"REIN_TEST_REPO_A",
	}
	for _, k := range required {
		if os.Getenv(k) == "" {
			return config{}, fmt.Errorf("missing env var %s (did you source ./dev-env?)", k)
		}
	}
	installationID, err := strconv.ParseInt(os.Getenv("REIN_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return config{}, fmt.Errorf("REIN_APP_INSTALLATION_ID not an int64: %w", err)
	}

	slug := os.Getenv("REIN_TEST_REPO_A")
	// slug is "owner/name"; the installation already pins the owner, so we
	// only forward the bare name to the App-token API.
	_, repoName, ok := strings.Cut(slug, "/")
	if !ok || repoName == "" {
		return config{}, fmt.Errorf("REIN_TEST_REPO_A %q is not owner/name", slug)
	}

	return config{
		app: githubapp.Config{
			ClientID:       os.Getenv("REIN_APP_CLIENT_ID"),
			PrivateKeyPath: os.Getenv("REIN_APP_PRIVATE_KEY_PATH"),
			InstallationID: installationID,
			RepoName:       repoName,
		},
	}, nil
}

// openLog returns a logger writing to $XDG_STATE_HOME/rein/helper.log
// (~/.local/state/rein/helper.log by default). git invokes the helper as a
// subprocess; stderr would be captured by git but a dedicated log file is
// easier to inspect after the fact and survives multiple invocations within
// one git operation.
func openLog() (*log.Logger, func(), error) {
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("locate home dir: %w", err)
		}
		stateDir = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(stateDir, "rein")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(dir, "helper.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return log.New(f, fmt.Sprintf("[pid %d] ", os.Getpid()), log.LstdFlags|log.LUTC),
		func() { _ = f.Close() },
		nil
}
