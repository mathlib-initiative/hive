package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		cli    string
		resume bool
		want   string
	}{
		{"claude", false, "claude -p --model m"},
		{"claude", true, "claude -p --continue --model m"},
		{"codex", false, "codex exec --model m -"},
		{"codex", true, "codex exec resume --last --model m -"},
		{"pi", false, "pi -p --model m"},
		{"pi", true, "pi -p --continue --model m"},
	} {
		a := &agent{cli: tc.cli, resume: tc.resume, args: []string{"--model", "m"}}
		if got := strings.Join(a.command(), " "); got != tc.want {
			t.Errorf("%s resume=%v: %q, want %q", tc.cli, tc.resume, got, tc.want)
		}
	}
}

// fakeCLI puts claude and codex scripts on PATH. Each invocation appends
// its arguments and stdin to the returned log, then runs the shell commands
// in $FAKE_THEN, if set.
func fakeCLI(t *testing.T) (log string) {
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := "#!/bin/sh\n{ echo \"args: $*\"; echo \"stdin: $(cat)\"; echo end; } >> " + log + "\neval \"$FAKE_THEN\"\n"
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return log
}

func newTestAgent(prompt string) *agent {
	return &agent{
		cli:    "claude",
		prompt: func() (string, error) { return prompt, nil },
		env:    os.Environ(),
		wake:   make(chan struct{}, 1),
	}
}

// start runs the agent's loop until the test ends.
func start(t *testing.T, a *agent) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.loop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// runs waits until the log holds n runs and returns them.
func runs(t *testing.T, log string, n int) []string {
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(log)
		r := strings.Split(string(b), "end\n")
		r = r[:len(r)-1]
		if len(r) >= n || time.Now().After(deadline) {
			if len(r) < n {
				t.Fatalf("got %d runs, want %d: %q", len(r), n, r)
			}
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTriggersMerge(t *testing.T) {
	log := fakeCLI(t)
	a := newTestAgent("base prompt\n")
	a.trigger("one")
	a.trigger("")
	a.trigger("two")
	start(t, a)

	r := runs(t, log, 1)
	if want := "args: -p\nstdin: base prompt\n\none\n\ntwo\n"; r[0] != want {
		t.Errorf("run %q, want %q", r[0], want)
	}
	time.Sleep(200 * time.Millisecond)
	if r := runs(t, log, 1); len(r) != 1 {
		t.Errorf("%d runs, want 1", len(r))
	}
}

func TestTriggerDuringRun(t *testing.T) {
	log := fakeCLI(t)
	t.Setenv("FAKE_THEN", "sleep 0.3")
	a := newTestAgent("p")
	a.trigger("first")
	start(t, a)
	runs(t, log, 1)
	a.trigger("second")
	a.trigger("third")

	r := runs(t, log, 2)
	if want := "args: -p\nstdin: p\n\nsecond\n\nthird\n"; r[1] != want {
		t.Errorf("run %q, want %q", r[1], want)
	}
}

func TestFullQueueStillWakes(t *testing.T) {
	a := newTestAgent("p")
	a.trigger(strings.Repeat("x", maxQueued))
	<-a.wake // as if a run started but could not read its prompt
	if a.trigger("y") {
		t.Error("trigger past a full queue accepted")
	}
	select {
	case <-a.wake:
	default:
		t.Error("rejected trigger did not ask for a run")
	}
}

// TestNoRunAfterCancel checks that the loop starts no run once ctx is
// cancelled, even with a trigger pending.
func TestNoRunAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		a := newTestAgent("p")
		a.prompt = func() (string, error) {
			t.Fatal("run started after cancel")
			return "", nil
		}
		a.trigger("")
		a.loop(ctx)
	}
}

func TestTimer(t *testing.T) {
	log := fakeCLI(t)
	a := newTestAgent("p")
	a.every = 50 * time.Millisecond
	start(t, a)
	runs(t, log, 3)
}

func TestEmptyPromptSkipped(t *testing.T) {
	log := fakeCLI(t)
	a := newTestAgent("")
	a.trigger("")
	a.run(context.Background(), 1, "test")
	a.trigger("only")
	a.run(context.Background(), 2, "test")
	if r := runs(t, log, 1); len(r) != 1 || r[0] != "args: -p\nstdin: only\n" {
		t.Errorf("runs %q", r)
	}
}

func TestPromptErrorKeepsMessages(t *testing.T) {
	log := fakeCLI(t)
	a := newTestAgent("p")
	fail := true
	a.prompt = func() (string, error) {
		if fail {
			fail = false
			return "", errors.New("unreadable")
		}
		return "p", nil
	}
	a.trigger("m")
	a.run(context.Background(), 1, "test")
	a.run(context.Background(), 2, "test")
	if r := runs(t, log, 1); len(r) != 1 || r[0] != "args: -p\nstdin: p\n\nm\n" {
		t.Errorf("runs %q", r)
	}
}

// logOutput collects what the test logs.
func logOutput(t *testing.T) *bytes.Buffer {
	var b bytes.Buffer
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &b
}

func TestTimeout(t *testing.T) {
	fakeCLI(t)
	t.Setenv("FAKE_THEN", "sleep 30")
	out := logOutput(t)
	a := newTestAgent("p")
	a.timeout = 200 * time.Millisecond

	begin := time.Now()
	a.run(context.Background(), 1, "test")
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("run took %s", d)
	}
	if !strings.Contains(out.String(), "run 1 timed out") {
		t.Errorf("log %q", out)
	}
}

// TestLeftoversStopped checks that a background process the CLI leaves
// behind is stopped when the run ends.
func TestLeftoversStopped(t *testing.T) {
	for _, wait := range []string{
		"",         // the CLI exits at once
		"sleep 30", // the run times out
	} {
		log := fakeCLI(t)
		marker := filepath.Join(t.TempDir(), "marker")
		t.Setenv("FAKE_THEN", "(sleep 0.5; touch "+marker+") & "+wait)
		a := newTestAgent("p")
		a.timeout = 200 * time.Millisecond
		a.run(context.Background(), 1, "test")
		runs(t, log, 1)
		time.Sleep(time.Second)
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%q: a child of the CLI survived the run", wait)
		}
	}
}

func TestShutdownStopsRun(t *testing.T) {
	log := fakeCLI(t)
	t.Setenv("FAKE_THEN", "sleep 30")
	a := newTestAgent("p")
	a.trigger("")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.loop(ctx); close(done) }()
	runs(t, log, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop the run")
	}
}

func TestHandler(t *testing.T) {
	a := newTestAgent("p")
	srv := httptest.NewServer(a.handler("secret"))
	defer srv.Close()

	post := func(auth, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/trigger", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("", "x"); got != 401 {
		t.Errorf("no token: %d", got)
	}
	if got := post("Bearer wrong", "x"); got != 401 {
		t.Errorf("wrong token: %d", got)
	}
	if got := post("bearer secret", " hello \n"); got != 202 {
		t.Errorf("token: %d", got)
	}
	if got := post("Bearer secret", strings.Repeat("x", 1<<20+1)); got != 400 {
		t.Errorf("large body: %d", got)
	}
	if m := a.take(); !slices.Equal(m, []string{"hello"}) {
		t.Errorf("messages %q", m)
	}
	if got := post("Bearer secret", strings.Repeat("x", 1<<20)); got != 202 {
		t.Errorf("full queue: %d", got)
	}
	if got := post("Bearer secret", "y"); got != 429 {
		t.Errorf("past full queue: %d", got)
	}
	if got := post("Bearer secret", ""); got != 202 {
		t.Errorf("empty body with full queue: %d", got)
	}
	if m := a.take(); len(m) != 1 {
		t.Errorf("%d messages, want 1", len(m))
	}

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
}

func TestChildEnv(t *testing.T) {
	got := childEnv([]string{"A=1", tokenVar + "=secret", tokenVar + "_OTHER=2"})
	if want := []string{"A=1", tokenVar + "_OTHER=2"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
