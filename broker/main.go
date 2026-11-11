// Command hive-broker hands agents short-lived GitHub App installation
// tokens, so that no agent holds a long-lived GitHub credential. See
// README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "/etc/hive-broker/config.json", "path to the JSON config file")
	flag.Parse()

	c, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	b, err := newBroker(c)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              c.Listen,
		Handler:           b,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second, // request bodies are small
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("listening on %s", c.Listen)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	stop() // a second signal ends the process at once
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
}
