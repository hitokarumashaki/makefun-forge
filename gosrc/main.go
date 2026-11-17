// Entry point for the make.fun github-core-clone-01 forge (Go binary core).
// The lifecycle is the harness's, not the contest's:
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial
//      container). Without it there is nothing to talk to, so the binary
//      serves locally instead (PORT, default 8080) so it can be run and
//      tested by hand; the trial always sets MAKEFUN_ENV_URL.
//   2. Listen on an ephemeral port. Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address
//      the request came from plus N as your base URL and dials it from then
//      on. The first register wins; a 409 means another entry already
//      registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is a
//      poll, not a long-held request. A failed or slow poll is retried with a
//      short exponential backoff; the process never exits before the env says
//      done.
//   5. Close the server and exit 0.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// How long one register or done request may take before it is abandoned and retried.
const requestTimeout = 30 * time.Second

// The first wait between retries, and the longest.
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

// The wait after `attempt` failed requests: exponential from retryInitial, capped.
func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	d := retryInitial * time.Duration(1<<uint(attempt))
	if d > retryMax {
		d = retryMax
	}
	return d
}

var httpClient = &http.Client{Timeout: requestTimeout}

// register POSTs the bound port until the env accepts it. 200 is the normal
// answer; 409 means a first register already won, which is not this process's
// to fix. 400 and 405 are contract errors and end the program; a network
// error or another status is retried, since the env may still be starting.
func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		res, err := httpClient.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}; never returns
// before that.
func waitForDone(envURL string) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodGet, envURL+"/done", nil)
		if err == nil {
			res, err := httpClient.Do(req)
			if err == nil {
				var parsed struct {
					Done bool `json:"done"`
				}
				if err := json.NewDecoder(res.Body).Decode(&parsed); err == nil && parsed.Done {
					_ = res.Body.Close()
					return
				}
				_ = res.Body.Close()
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	log.SetFlags(0)
	store := NewStore()
	token := appToken()
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	server := &http.Server{Handler: makeHandler(store, token)}

	if envURL == "" {
		// No env to register with: serve locally for hand testing.
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		listener, err := net.Listen("tcp", ":"+port)
		if err != nil {
			log.Fatalf("[entry] listen: %v", err)
		}
		log.Printf("[entry] MAKEFUN_ENV_URL is not set: serving locally on port %s", port)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[entry] serve: %v", err)
		}
		return
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatalf("[entry] listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		_ = server.Serve(listener)
	}()
	log.Printf("[entry] listening on port %d", port)

	if err := register(envURL, port); err != nil {
		log.Fatalf("[entry] %v", err)
	}
	log.Printf("[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	_ = server.Close()
	log.Printf("[entry] done")
}
