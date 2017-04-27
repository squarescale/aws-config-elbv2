package main

import (
	"context"
	"flag"
	"github.com/braintree/manners"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/aws/aws-sdk-go/aws/session"
)

var version string
var awsSession *session.Session

func getenv(varname, defaultval string) string {
	res := os.Getenv(varname)
	if res == "" {
		return defaultval
	} else {
		return res
	}
}

func main() {
	var err error
	var endpoints string
	var endpointList []string
	var listen string

	flag.StringVar(&listen, "listen", getenv("NOMAD_ADDR_http", ""), "Listen address (env: NOMAD_ADDR_http)")
	flag.StringVar(&endpoints, "endpoints", getenv("ETCD_ENDPOINTS", "http://127.0.0.1:2379"), "List of etcd endpoints separated by commas")
	flag.Parse()

	endpointList = strings.Split(endpoints, ",")

	log.Printf("Starting aws-config-elbv2 version %s", version)
	log.Printf("\tNOMAD_ADDR_http: %#v", listen)
	log.Printf("\tETCD_ENDPOINTS:  %v", endpointList)

	awsSession, err = session.NewSession()
	if err != nil {
		log.Printf("Fatal error: %v", err)
		os.Exit(1)
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	targetGroups, err := NewTargetGroups(ctx, &wg, endpointList)
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

	select {
	case err := <-errChan:
		log.Printf("Server error: %v", err)
		server = nil
	case s := <-signalChan:
		log.Printf("Captured %v. Cancel tasks.", s)
	}

	cancel()
	log.Println("Waiting...")
	if server != nil {
		wg.Add(1)
		go func() {
			server.BlockingClose()
			wg.Done()
		}()
	}
	wg.Wait()
	log.Println("Done.")
	os.Exit(0)
}

func httpHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
