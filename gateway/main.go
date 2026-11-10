// Command hive-gateway is a reverse proxy for inference APIs. It holds the
// provider credentials: clients send requests without them, and the gateway
// adds them before forwarding. See README.md.
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
	configPath := flag.String("config", "/etc/hive-gateway/config.json", "path to the JSON config file")
	flag.Parse()

	c, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              c.Listen,
		Handler:           newGateway(c),
		ReadHeaderTimeout: 10 * time.Second,
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
	// Give in-flight requests, often long streams, a moment to finish.
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
}
