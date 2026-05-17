// dingdong-cli is the agent client for dingdong. It exposes three subcommands:
//
//	knock  — publish a knock to the server
//	wait   — block until the next matching knock arrives, print it, exit
//	tail   — stream matching knocks forever, one JSON per line
//
// All subcommands read DINGDONG_URL (default http://localhost:8080) and
// DINGDONG_TOKEN from the environment.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type knock struct {
	ID        string    `json:"id,omitempty"`
	Ts        time.Time `json:"ts,omitempty"`
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"`
	Topic     string    `json:"topic"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject,omitempty"`
	Body      string    `json:"body,omitempty"`
	InReplyTo string    `json:"in_reply_to,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	cfg, err := loadEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	switch cmd {
	case "knock":
		err = runKnock(cfg, args)
	case "wait":
		err = runWait(cfg, args)
	case "tail":
		err = runTail(cfg, args)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `dingdong — agent-to-agent knock service

usage:
  dingdong knock --from <id> --topic <t> [--to <id>] [--kind <k>] [--subject <s>] [--body <b>] [--in-reply-to <id>] [--body-stdin]
  dingdong wait  [--topic <t>] [--to <id>] [--since <id>] [--timeout <dur>]
  dingdong tail  [--topic <t>] [--to <id>] [--since <id>]

env:
  DINGDONG_URL    base URL of the server (default http://localhost:8080)
  DINGDONG_TOKEN  bearer token (required)
`)
}

type config struct {
	URL   string
	Token string
}

func loadEnv() (config, error) {
	c := config{
		URL:   strings.TrimRight(os.Getenv("DINGDONG_URL"), "/"),
		Token: os.Getenv("DINGDONG_TOKEN"),
	}
	if c.URL == "" {
		c.URL = "http://localhost:8080"
	}
	if c.Token == "" {
		return c, errors.New("DINGDONG_TOKEN must be set")
	}
	return c, nil
}

func runKnock(cfg config, args []string) error {
	fs := flag.NewFlagSet("knock", flag.ContinueOnError)
	from := fs.String("from", os.Getenv("DINGDONG_FROM"), "agent identifier (default $DINGDONG_FROM)")
	topic := fs.String("topic", "default", "topic")
	to := fs.String("to", "", "recipient (optional)")
	kind := fs.String("kind", "info", "knock kind: knock|ready|need|info|reply")
	subject := fs.String("subject", "", "short headline")
	body := fs.String("body", "", "longer body")
	inReplyTo := fs.String("in-reply-to", "", "id of the knock being replied to")
	bodyStdin := fs.Bool("body-stdin", false, "read body from stdin (overrides --body)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("--from is required (or set DINGDONG_FROM)")
	}
	if *bodyStdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		*body = string(b)
	}

	k := knock{
		From: *from, To: *to, Topic: *topic, Kind: *kind,
		Subject: *subject, Body: *body, InReplyTo: *inReplyTo,
	}
	buf, _ := json.Marshal(k)

	req, err := http.NewRequest(http.MethodPost, cfg.URL+"/v1/knocks", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func runWait(cfg config, args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	topic := fs.String("topic", "", "topic filter")
	to := fs.String("to", "", "recipient filter (matches directed messages and broadcasts)")
	since := fs.String("since", "", "only return knocks with id > this")
	timeout := fs.Duration("timeout", 0, "max wait duration; 0 = wait forever")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()
	if *timeout > 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, *timeout)
		defer c2()
	}

	first := make(chan knock, 1)
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- streamKnocks(ctx, cfg, *topic, *to, *since, func(k knock) bool {
			select {
			case first <- k:
			default:
			}
			return false // stop after first
		})
	}()

	select {
	case k := <-first:
		_ = json.NewEncoder(os.Stdout).Encode(k)
		cancel()
		<-streamErr
		return nil
	case err := <-streamErr:
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("timeout: no matching knock arrived")
		}
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

// permanentErr wraps errors that should not be retried (e.g. 401/403/404).
type permanentErr struct{ err error }

func (e *permanentErr) Error() string { return e.err.Error() }
func (e *permanentErr) Unwrap() error { return e.err }

var tailRetryDelays = []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}

// knockStreamer abstracts the SSE consumer so tailLoop can be exercised in
// tests without an HTTP server. The production implementation is
// httpStreamer, which delegates to streamKnocks.
type knockStreamer interface {
	Stream(ctx context.Context, cfg config, topic, to, since string, onKnock func(knock) bool) error
}

type httpStreamer struct{}

func (httpStreamer) Stream(ctx context.Context, cfg config, topic, to, since string, onKnock func(knock) bool) error {
	return streamKnocks(ctx, cfg, topic, to, since, onKnock)
}

// tailOpts configures tailLoop's retry behavior and I/O sinks. Zero values
// fall back to production defaults (tailRetryDelays, os.Stderr).
type tailOpts struct {
	Delays  []time.Duration
	NoRetry bool
	ErrW    io.Writer
}

// nextTailDelay returns the backoff delay for the current drop and the
// updated attempt counter. When the just-dropped connection emitted at least
// one event, the counter is reset to 0 first — a successful reconnect is
// evidence that the server is reachable, so the next drop should start over
// at the shortest delay instead of inheriting a capped backoff.
func nextTailDelay(delays []time.Duration, attempt int, gotEvent bool) (delay time.Duration, next int) {
	if gotEvent {
		attempt = 0
	}
	delay = delays[min(attempt, len(delays)-1)]
	return delay, attempt + 1
}

func runTail(cfg config, args []string) error {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	topic := fs.String("topic", "", "topic filter")
	to := fs.String("to", "", "recipient filter")
	since := fs.String("since", "", "only return knocks with id > this")
	noRetry := fs.Bool("no-retry", false, "exit on first SSE drop instead of reconnecting")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	return tailLoop(ctx, httpStreamer{}, cfg, *topic, *to, *since, os.Stdout, tailOpts{NoRetry: *noRetry})
}

// tailLoop runs the production retry loop: stream knocks, write them to w,
// reconnect on transient drops with the last-seen id as since, and stop on
// context cancellation, permanent errors, or NoRetry. It is the single
// implementation shared by runTail and tests.
func tailLoop(ctx context.Context, s knockStreamer, cfg config, topic, to, since string, w io.Writer, opts tailOpts) error {
	delays := opts.Delays
	if len(delays) == 0 {
		delays = tailRetryDelays
	}
	errW := opts.ErrW
	if errW == nil {
		errW = os.Stderr
	}

	enc := json.NewEncoder(w)
	lastSeen := since
	attempt := 0

	for {
		gotEvent := false
		err := s.Stream(ctx, cfg, topic, to, lastSeen, func(k knock) bool {
			gotEvent = true
			if k.ID != "" {
				lastSeen = k.ID
			}
			_ = enc.Encode(k)
			return true
		})

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}

		var perm *permanentErr
		if errors.As(err, &perm) {
			return err
		}

		if opts.NoRetry {
			return err
		}

		var delay time.Duration
		delay, attempt = nextTailDelay(delays, attempt, gotEvent)
		reason := "EOF"
		if err != nil {
			reason = err.Error()
		}
		_, _ = fmt.Fprintf(errW, "dingdong tail: connection dropped (%s); reconnecting in %v\n", reason, delay)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil
		}
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// streamKnocks opens an SSE connection and calls onKnock for each event until
// it returns false or ctx is cancelled.
func streamKnocks(ctx context.Context, cfg config, topic, to, since string, onKnock func(knock) bool) error {
	q := url.Values{}
	q.Set("token", cfg.Token)
	if topic != "" {
		q.Set("topic", topic)
	}
	if to != "" {
		q.Set("to", to)
	}
	if since != "" {
		q.Set("since", since)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL+"/v1/stream?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		msg := fmt.Sprintf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return &permanentErr{errors.New(msg)}
		}
		return errors.New(msg)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// dispatch event
			if len(dataLines) > 0 {
				payload := strings.Join(dataLines, "\n")
				dataLines = dataLines[:0]
				var k knock
				if err := json.Unmarshal([]byte(payload), &k); err == nil {
					if !onKnock(k) {
						return nil
					}
				}
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keepalive
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// id: and event: lines are intentionally ignored — we tag everything as a knock
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return scanner.Err()
}
