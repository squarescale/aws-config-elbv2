package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/braintree/manners"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

var version string

func getenv(varname, defaultval string) string {
	res := os.Getenv(varname)
	if res == "" {
		return defaultval
	} else {
		return res
	}
}

func main() {
	var endpoints string
	var listen string

	flag.StringVar(&listen, "listen", getenv("NOMAD_ADDR_http", ""), "Listen address (env: NOMAD_ADDR_http)")
	flag.StringVar(&endpoints, "endpoints", getenv("ETCD_ENDPOINTS", "http://127.0.0.1:2379"), "List of etcd endpoints separated by commas")
	flag.Parse()

	log.Printf("Starting aws-config-elbv2 version %s", version)
	log.Printf("\thttp listen address: %s", listen)
	log.Printf("\tetcd endpoints: %s", endpoints)

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	targetGroups, err := NewTargetGroups(ctx, wg, strings.Split(endpoints, ","))
	if err != nil {
		log.Fatal(err)
	}

	errChan := make(chan error, 10)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	var server *manners.GracefulServer
	if listen == "" {
		log.Printf("Running without a web server")
	} else {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", httpHealth)
		mux.Handle("/target-groups", targetGroups)

		server = manners.NewServer()
		server.Handler = mux
		server.Addr = listen

		go func() {
			log.Printf("HTTP service starting on %s", listen)
			errChan <- server.ListenAndServe()
		}()
	}

	for {
		select {
		case err := <-errChan:
			if err != nil {
				log.Fatal(err)
			}
		case s := <-signalChan:
			log.Println(fmt.Sprintf("Captured %v. Exiting...", s))
			cancel()
			wg.Wait()
			if server != nil {
				server.BlockingClose()
			}
			os.Exit(0)
		}
	}
}

func httpHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
