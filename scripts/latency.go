//go:build ignore

// Run from the repository root: go run ./scripts/latency.go -bin /path/to/crosshall
// Use an installed release with note support (v0.1.0-rc.2 or later).
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	binary := flag.String("bin", "crosshall", "installed release executable")
	samples := flag.Int("samples", 100, "measured commands per client and workload")
	warmup := flag.Int("warmup", 5, "unmeasured commands per client and workload")
	flag.Parse()
	if *samples < 1 || *warmup < 0 || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *binary, *samples, *warmup); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Every child gets only the benchmark's state path and, for members, its own
// route. A shell's vendor markers and state overrides never reach a child.
type caller struct {
	bin string
	env []string
}

func (c caller) member() caller {
	var route [16]byte
	rand.Read(route[:])
	return caller{bin: c.bin, env: append(slices.Clone(c.env), fmt.Sprintf("CROSSHALL_ROUTE=%x", route[:]))}
}

type response struct {
	OK       bool  `json:"ok"`
	ID       int64 `json:"id"`
	Exchange int64 `json:"exchange"`
	Debit    int   `json:"debit"`
	Messages []struct {
		ID int64 `json:"id"`
	} `json:"messages"`
}

func (c caller) call(ctx context.Context, args ...string) (response, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, append([]string{"--json"}, args...)...)
	cmd.Env = c.env
	start := time.Now()
	output, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		return response{}, elapsed, fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	var r response
	if err := json.Unmarshal(output, &r); err != nil || !r.OK {
		return response{}, elapsed, fmt.Errorf("%s: invalid success response: %s", args[0], strings.TrimSpace(string(output)))
	}
	return r, elapsed, nil
}

func (c caller) setup(ctx context.Context, args ...string) error {
	_, _, err := c.call(ctx, args...)
	return err
}

func run(ctx context.Context, binary string, samples, warmup int) error {
	bin, err := exec.LookPath(binary)
	if err != nil {
		return err
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "crosshall-latency-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	version := exec.CommandContext(ctx, bin, "version")
	version.Env = []string{"HOME=" + root, "CROSSHALL_STATE_DIR=" + filepath.Join(root, "version")}
	output, err := version.Output()
	if err != nil {
		return fmt.Errorf("read crosshall version: %w", err)
	}
	tag := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`).MatchString(tag) {
		return fmt.Errorf("a tagged crosshall release is required, got %q", tag)
	}
	fmt.Printf("# crosshall %s, %s/%s, %d logical CPUs, %d warmups per client\n",
		tag, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), warmup)
	fmt.Println("command,clients,samples,p50_ms,p95_ms")
	for _, workload := range []struct {
		label  string
		fanout int
	}{{"read", 0}, {"ask-one", 1}, {"ask-three", 3}} {
		for _, clients := range []int{1, 4, 8} {
			elapsed, err := measure(ctx, bin, root, workload.fanout, clients, samples, warmup)
			if err != nil {
				return fmt.Errorf("%s with %d clients: %w", workload.label, clients, err)
			}
			slices.Sort(elapsed)
			fmt.Printf("%s,%d,%d,%.2f,%.2f\n", workload.label, clients, len(elapsed),
				percentile(elapsed, 50), percentile(elapsed, 95))
		}
	}
	return nil
}

func percentile(sorted []time.Duration, p int) float64 {
	// Nearest rank: the one-based index is ceil(p*n/100).
	at := (p*len(sorted) + 99) / 100
	return float64(sorted[at-1]) / float64(time.Millisecond)
}

func measure(ctx context.Context, bin, root string, fanout, clients, samples, warmup int) (elapsed []time.Duration, err error) {
	state := filepath.Join(root, fmt.Sprintf("state-%d-%d", fanout, clients))
	if err := os.Mkdir(state, 0700); err != nil {
		return nil, err
	}
	owner := caller{bin: bin, env: []string{
		"HOME=" + root, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "CROSSHALL_STATE_DIR=" + state,
	}}
	stop, err := startDispatcher(ctx, owner, state)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, stop()) }()
	if err := owner.setup(ctx, "owner", "channel", "create", "latency"); err != nil {
		return nil, err
	}
	workers := make([]caller, clients)
	aliases := make([]string, clients)
	for i := range workers {
		workers[i] = owner.member()
		aliases[i] = fmt.Sprintf("client-%d", i+1)
		if err := workers[i].setup(ctx, "join", "latency", "--as", aliases[i]); err != nil {
			return nil, err
		}
	}
	peers := make([]string, max(1, fanout))
	var publisher caller
	for i := range peers {
		publisher = owner.member()
		alias := fmt.Sprintf("peer-%d", i+1)
		peers[i] = "@" + alias
		if err := publisher.setup(ctx, "join", "latency", "--as", alias); err != nil {
			return nil, err
		}
	}
	total := samples + warmup
	var action func(context.Context, int, int) (time.Duration, error)
	if fanout == 0 {
		action, err = prepareReads(ctx, publisher, workers, total)
	} else {
		action, err = prepareAsks(ctx, owner, workers, aliases, peers, total)
	}
	if err != nil {
		return nil, err
	}
	if _, err := runClients(ctx, clients, 0, warmup, action); err != nil {
		return nil, fmt.Errorf("warmup: %w", err)
	}
	return runClients(ctx, clients, warmup, total, action)
}

func prepareReads(ctx context.Context, publisher caller, workers []caller, count int) (func(context.Context, int, int) (time.Duration, error), error) {
	ids := make([]int64, count)
	for i := range ids {
		r, _, err := publisher.call(ctx, "note", "--all", "latency sample")
		if err != nil {
			return nil, err
		}
		if r.ID < 1 {
			return nil, fmt.Errorf("note response is missing the message ID")
		}
		ids[i] = r.ID
	}
	return func(ctx context.Context, client, i int) (time.Duration, error) {
		r, elapsed, err := workers[client].call(ctx, "read", strconv.FormatInt(ids[i], 10))
		if err == nil && (len(r.Messages) != 1 || r.Messages[0].ID != ids[i]) {
			err = fmt.Errorf("read response did not contain message %d", ids[i])
		}
		return elapsed, err
	}, nil
}

func prepareAsks(ctx context.Context, owner caller, workers []caller, aliases, peers []string, count int) (func(context.Context, int, int) (time.Duration, error), error) {
	commands := make([][]string, len(workers))
	for i, worker := range workers {
		if err := owner.setup(ctx, "owner", "budget", "grant", "latency/@"+aliases[i], "--roots", "1"); err != nil {
			return nil, err
		}
		// Each measured ask uses an existing, independently funded exchange.
		seed, _, err := worker.call(ctx, "ask", peers[0], "latency setup", "--new")
		if err != nil {
			return nil, err
		}
		if seed.ID < 1 || seed.Exchange < 1 {
			return nil, fmt.Errorf("seed ask response is missing the message or exchange ID")
		}
		target, units := fmt.Sprintf("latency/x%d", seed.Exchange), strconv.Itoa(count*len(peers))
		if err := owner.setup(ctx, "owner", "budget", "grant", target, units); err != nil {
			return nil, err
		}
		args := append([]string{"ask"}, peers...)
		commands[i] = append(args, "latency sample", "--re", strconv.FormatInt(seed.ID, 10))
	}
	return func(ctx context.Context, client, _ int) (time.Duration, error) {
		r, elapsed, err := workers[client].call(ctx, commands[client]...)
		if err == nil && r.Debit != len(peers) {
			err = fmt.Errorf("ask response debited %d units, wanted %d", r.Debit, len(peers))
		}
		return elapsed, err
	}, nil
}

func runClients(ctx context.Context, clients, first, end int, action func(context.Context, int, int) (time.Duration, error)) ([]time.Duration, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	elapsed := make([]time.Duration, clients*(end-first))
	var once sync.Once
	var failure error
	var workers sync.WaitGroup
	start := make(chan struct{})
	for client := range clients {
		workers.Go(func() {
			<-start
			for i := first; i < end; i++ {
				took, err := action(ctx, client, i)
				if err != nil {
					once.Do(func() {
						failure = fmt.Errorf("client %d command %d: %w", client+1, i+1, err)
						cancel()
					})
					return
				}
				elapsed[client*(end-first)+i-first] = took
			}
		})
	}
	close(start)
	workers.Wait()
	return elapsed, failure
}

// Start the packaged daemon as our child so cleanup can signal the process
// handle we own. The public owner commands still exercise their usual spool.
func startDispatcher(ctx context.Context, owner caller, state string) (func() error, error) {
	digest := sha256.Sum256([]byte(state))
	rendezvous := filepath.Join("/tmp", fmt.Sprintf("crosshall-%d-%x", os.Geteuid(), digest[:4]))
	if err := os.Mkdir(rendezvous, 0700); err != nil {
		return nil, fmt.Errorf("reserve benchmark rendezvous: %w", err)
	}
	cmd := exec.Command(owner.bin, "__dispatch")
	cmd.Env, cmd.Dir, cmd.Stderr = owner.env, state, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, os.RemoveAll(rendezvous))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() error {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			return errors.Join(err, os.RemoveAll(rendezvous))
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			return errors.Join(<-done, os.RemoveAll(rendezvous))
		}
	}

	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return nil, errors.Join(fmt.Errorf("dispatcher exited before readiness"), err, os.RemoveAll(rendezvous))
		case <-wait.Done():
			return nil, errors.Join(fmt.Errorf("dispatcher readiness: %w", wait.Err()), stop())
		case <-tick.C:
			output, err := os.ReadFile(filepath.Join(rendezvous, "ready"))
			var marker struct{ PID int }
			if err == nil && json.Unmarshal(output, &marker) == nil && marker.PID == cmd.Process.Pid {
				return stop, nil
			}
		}
	}
}
