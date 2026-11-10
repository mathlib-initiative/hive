// Command hive-agent runs a coding agent CLI, claude or codex, again and
// again: on a timer, when triggered over HTTP, or both. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// tokenVar holds the trigger API token. It is removed from the CLI's
// environment, so that it does not turn up in the agent's tool output.
const tokenVar = "HIVE_AGENT_TOKEN"

func main() {
	cli := flag.String("cli", "", `agent CLI to run: "claude" or "codex" (required)`)
	session := flag.String("session", "", `"resume" continues the previous session on every run, "fresh" starts a new one (required)`)
	prompt := flag.String("prompt", "", "prompt for every run")
	promptFile := flag.String("prompt-file", "", "file holding the prompt, read again before every run")
	every := flag.Duration("every", 0, "run at start, then this long after each run ends")
	listen := flag.String("listen", "", "address for the trigger API, such as :8080")
	timeout := flag.Duration("timeout", 0, "stop a run that takes longer than this")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: hive-agent [flags] [-- extra CLI arguments]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	a := &agent{
		cli:     *cli,
		resume:  *session == "resume",
		prompt:  func() (string, error) { return *prompt, nil },
		args:    flag.Args(),
		every:   *every,
		timeout: *timeout,
		env:     childEnv(os.Environ()),
		wake:    make(chan struct{}, 1),
	}
	if *promptFile != "" {
		a.prompt = func() (string, error) {
			b, err := os.ReadFile(*promptFile)
			return string(b), err
		}
	}
	switch {
	case *cli != "claude" && *cli != "codex":
		log.Fatal(`-cli must be "claude" or "codex"`)
	case *session != "resume" && *session != "fresh":
		log.Fatal(`-session must be "resume" or "fresh"`)
	case *prompt != "" && *promptFile != "":
		log.Fatal("set -prompt or -prompt-file, not both")
	case *every <= 0 && *listen == "":
		log.Fatal("nothing would ever run: set -every, -listen or both")
	case *every > 0 && *prompt == "" && *promptFile == "":
		log.Fatal("-every needs -prompt or -prompt-file")
	}
	if _, err := exec.LookPath(*cli); err != nil {
		log.Fatal(err)
	}
	if _, err := a.prompt(); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *listen != "" {
		// Listen before the first run, so that a bad address stops
		// hive-agent before it starts anything.
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		token := strings.TrimSpace(os.Getenv(tokenVar))
		if token == "" {
			log.Printf("%s is not set, so anyone who can reach %s can trigger runs", tokenVar, *listen)
		}
		srv := &http.Server{
			Handler:           a.handler(token),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       time.Minute,
		}
		go func() {
			log.Printf("listening on %s", ln.Addr())
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
		defer srv.Close()
	}
	// loop returns once ctx is cancelled and the current run, if any, has
	// stopped.
	a.loop(ctx)
}

func childEnv(environ []string) []string {
	var env []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, tokenVar+"=") {
			env = append(env, kv)
		}
	}
	return env
}
