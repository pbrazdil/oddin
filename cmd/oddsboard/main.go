// Command oddsboard serves the embeddable live odds board.
//
//	oddsboard [flags]                 run the server
//	oddsboard replay [flags] FILE     push a recorded feed (JSON array) into a running server
package main

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"oddsboard/internal/board"
	"oddsboard/internal/feed"
	"oddsboard/internal/journal"
	"oddsboard/internal/market"
	"oddsboard/internal/server"
	"oddsboard/internal/webui"
)

var markets = []market.Definition{{
	Key:   market.Key{MatchID: "match-1", MarketID: "winner"},
	Event: "Falcons vs Wolves",
	Name:  "Match winner",
	Outcomes: []market.OutcomeDef{
		{ID: "falcons", Name: "Falcons"},
		{ID: "wolves", Name: "Wolves"},
	},
}}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "replay" {
		if err := replay(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "replay:", err)
			os.Exit(1)
		}
		return
	}
	if err := serve(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "oddsboard:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("oddsboard", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "listen address")
	publicURL := fs.String("public-url", "", "base URL shown in links (default http://<addr>)")
	dataDir := fs.String("data", "data", `directory for the journal and tokens; "" keeps everything in memory`)
	simulate := fs.Bool("simulate", true, "run the built-in feed provider simulator")
	seed := fs.Uint64("seed", uint64(time.Now().UnixNano()), "simulator random seed")
	snapshotURL := fs.String("snapshot-url", "", "provider snapshot endpoint for recovery when not simulating, with {matchId} and {marketId} placeholders")
	staleAfter := fs.Duration("stale-after", 3*time.Second, "feed silence after which prices are stale")
	recoverAfter := fs.Duration("recover-after", 2*time.Second, "how long a market may be inconsistent before a snapshot is requested")
	frameAncestors := fs.String("frame-ancestors", "*", "CSP frame-ancestors for the embed page, e.g. \"https://partner.example\"")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var j *journal.Journal
	if *dataDir == "" {
		j = journal.NewMemory()
	} else {
		if err := os.MkdirAll(*dataDir, 0o700); err != nil {
			return err
		}
		var err error
		if j, err = journal.Open(filepath.Join(*dataDir, "journal.jsonl"), log); err != nil {
			return err
		}
	}
	defer j.Close()

	adminToken, err := token(*dataDir, "admin.token", "ODDSBOARD_ADMIN_TOKEN")
	if err != nil {
		return err
	}
	feedToken, err := token(*dataDir, "feed.token", "ODDSBOARD_FEED_TOKEN")
	if err != nil {
		return err
	}

	// The board is created before the simulator, which delivers into it.
	var sim *feed.Simulator
	var snapshots board.SnapshotSource
	var b *board.Board
	if *simulate {
		sim = feed.NewSimulator(markets[0], *seed, func(raw []byte) {
			if _, err := b.SubmitEvent(raw, board.SourceSimulator); err != nil {
				log.Error("simulator delivery failed", "err", err)
			}
		})
		snapshots = sim
	} else if *snapshotURL != "" {
		snapshots = feed.HTTPSnapshots{URL: *snapshotURL}
	}
	b, err = board.New(markets, board.Config{StaleAfter: *staleAfter, RecoverAfter: *recoverAfter, Log: log}, j, snapshots)
	if err != nil {
		return fmt.Errorf("restoring from journal: %w", err)
	}
	defer b.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go b.Run(ctx, 250*time.Millisecond)
	if sim != nil {
		// Continue the provider's sequence after whatever the journal holds,
		// like a real provider would after our restart.
		if p, ok := b.Current(markets[0].Key); ok {
			sim.ContinueAfter(p.State.Seq)
		}
		sim.Configure(feed.Settings{Running: true, IntervalMs: 1000})
		go sim.Run(ctx)
	}

	done := make(chan struct{})
	h := server.New(server.Options{
		Board:          b,
		Simulator:      sim,
		Replayer:       feed.NewReplayer(func(raw []byte) (board.Ack, error) { return b.SubmitEvent(raw, board.SourceReplay) }),
		Assets:         webui.FS(),
		AdminToken:     adminToken,
		FeedToken:      feedToken,
		FrameAncestors: *frameAncestors,
		Log:            log,
		Done:           done,
	})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No WriteTimeout: event streams are long-lived. The stream handler
		// sets per-write deadlines instead.
	}

	base := *publicURL
	if base == "" {
		host, _, _ := net.SplitHostPort(*addr)
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
		base = "http://" + net.JoinHostPort(host, port)
	}
	base = strings.TrimRight(base, "/")
	embed := base + "/embed/" + markets[0].Key.MatchID + "/" + markets[0].Key.MarketID
	fmt.Printf(`
Live odds board — %s, %s

  Board (iframe src)    %s
  Partner demo page     %s/demo/
  Superadmin + replay   %s#admin=%s
  Feed ingestion        POST %s/api/v1/feed/events   Authorization: Bearer %s
  Journal               %s

`, markets[0].Event, markets[0].Name, embed, base, embed, adminToken, base, feedToken, journalPath(*dataDir))

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	close(done)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func journalPath(dir string) string {
	if dir == "" {
		return "(in memory)"
	}
	return filepath.Join(dir, "journal.jsonl")
}

// token returns a secret from the environment, or from dir/name, creating
// a random one there on first start so links stay valid across restarts.
func token(dir, name, env string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	if dir == "" {
		return rand.Text(), nil
	}
	path := filepath.Join(dir, name)
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 16 {
			return t, nil
		}
	}
	t := rand.Text()
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	baseURL := fs.String("url", "http://localhost:8080", "server base URL")
	tok := fs.String("token", "", "feed or admin token (default: $ODDSBOARD_FEED_TOKEN or data/feed.token)")
	interval := fs.Duration("interval", time.Second, "delay between messages")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: oddsboard replay [flags] FILE|-\n\nFILE holds a JSON array of feed events (or one event).")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected one file")
	}
	var data []byte
	var err error
	if fs.Arg(0) == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(fs.Arg(0))
	}
	if err != nil {
		return err
	}
	items, err := market.SplitBatch(data)
	if err != nil {
		return err
	}
	t := *tok
	if t == "" {
		t = os.Getenv("ODDSBOARD_FEED_TOKEN")
	}
	if t == "" {
		b, err := os.ReadFile(filepath.Join("data", "feed.token"))
		if err != nil {
			return errors.New("no token: pass -token or set ODDSBOARD_FEED_TOKEN")
		}
		t = strings.TrimSpace(string(b))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return feed.Push(ctx, strings.TrimRight(*baseURL, "/"), t, []jsontext.Value(items), *interval, os.Stdout)
}
