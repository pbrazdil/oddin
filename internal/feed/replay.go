package feed

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"oddsboard/internal/board"
	"oddsboard/internal/market"
)

// ReplayStatus describes the current or last replay.
type ReplayStatus struct {
	Running    bool        `json:"running"`
	Total      int         `json:"total"`
	Sent       int         `json:"sent"`
	IntervalMs int64       `json:"intervalMs"`
	StartedAt  time.Time   `json:"startedAt,omitzero"`
	Acks       []board.Ack `json:"acks"`
	Error      string      `json:"error,omitzero"`
}

// Replayer feeds a recorded list of messages into the board at a fixed
// pace, as if they came from the provider. One replay runs at a time.
type Replayer struct {
	submit func([]byte) (board.Ack, error)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	status ReplayStatus
}

// NewReplayer returns a replayer that submits through fn.
func NewReplayer(fn func([]byte) (board.Ack, error)) *Replayer {
	return &Replayer{submit: fn, status: ReplayStatus{Acks: []board.Ack{}}}
}

// ErrReplayRunning is returned by Start while a replay is in progress.
var ErrReplayRunning = errors.New("a replay is already running")

// Start replays items in the background, one every interval. With a zero
// interval all items are submitted before Start returns.
func (r *Replayer) Start(items []jsontext.Value, interval time.Duration) error {
	r.mu.Lock()
	if r.status.Running {
		r.mu.Unlock()
		return ErrReplayRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	r.status = ReplayStatus{Running: true, Total: len(items), IntervalMs: interval.Milliseconds(), StartedAt: time.Now().UTC(), Acks: []board.Ack{}}
	done := r.done
	r.mu.Unlock()

	run := func() {
		defer close(done)
		defer cancel()
		for i, item := range items {
			if i > 0 && interval > 0 {
				select {
				case <-ctx.Done():
					r.finish("stopped")
					return
				case <-time.After(interval):
				}
			}
			ack, err := r.submit(item)
			r.mu.Lock()
			r.status.Sent++
			r.status.Acks = append(r.status.Acks, ack)
			r.mu.Unlock()
			if err != nil {
				r.finish(err.Error())
				return
			}
		}
		r.finish("")
	}
	if interval == 0 {
		run()
		return nil
	}
	go run()
	return nil
}

func (r *Replayer) finish(errText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Running, r.status.Error = false, errText
}

// Stop cancels a running replay and waits for it to end.
func (r *Replayer) Stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Status returns a copy of the replay status.
func (r *Replayer) Status() ReplayStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Acks = append([]board.Ack(nil), s.Acks...)
	return s
}

// Push posts each message to a running server's feed endpoint, one every
// interval, and writes the server's verdicts to out. It is the command
// line counterpart of the admin replay.
func Push(ctx context.Context, baseURL, token string, items []jsontext.Value, interval time.Duration, out io.Writer) error {
	client := &http.Client{Timeout: 10 * time.Second}
	for i, item := range items {
		if i > 0 && interval > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/feed/events", bytes.NewReader(item))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("message %d: %s: %s", i+1, resp.Status, bytes.TrimSpace(body))
		}
		var res struct {
			Acks []board.Ack `json:"acks"`
		}
		if err := json.Unmarshal(body, &res); err != nil || len(res.Acks) != 1 {
			return fmt.Errorf("message %d: unexpected response %s", i+1, body)
		}
		a := res.Acks[0]
		fmt.Fprintf(out, "seq %-6d %-9s version %-5d %s\n", a.Seq, a.Verdict, a.Version, a.Reason)
	}
	return nil
}

// HTTPSnapshots fetches snapshots from a provider's HTTP endpoint. The URL
// may contain {matchId} and {marketId} placeholders.
type HTTPSnapshots struct {
	URL    string
	Client *http.Client
}

// Snapshot implements board.SnapshotSource.
func (h HTTPSnapshots) Snapshot(ctx context.Context, key market.Key) ([]byte, error) {
	u := strings.NewReplacer("{matchId}", url.PathEscape(key.MatchID), "{marketId}", url.PathEscape(key.MarketID)).Replace(h.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("snapshot endpoint returned %s", resp.Status)
	}
	return body, nil
}
