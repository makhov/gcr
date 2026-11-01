//go:build linux

package nodeshutdown

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	login1  = "org.freedesktop.login1"
	manager = "org.freedesktop.login1.Manager"
)

// GuardOptions say what the guard does with the checkpoints.
type GuardOptions struct {
	// Push pushes pod uid's checkpoint, at CheckpointPath(uid).
	Push func(ctx context.Context, uid string) error
	// Wait bounds how long the guard holds the shutdown; logind's
	// InhibitDelayMaxSec bounds it too, and must be at least as long.
	Wait time.Duration
	Log  func(format string, args ...any)
}

// Guard holds the node's shutdown, with a systemd-logind delay lock, until
// the pods that opted in are checkpointed and pushed: it marks the
// shutdown (Begin) for the shims, which checkpoint containers as the
// kubelet stops them, pushes each checkpoint as it is written, and lets
// the shutdown go on once the kubelet has stopped its pods (it releases
// its own lock) and every checkpoint is pushed, or failed. It runs until
// ctx ends.
func Guard(ctx context.Context, o GuardOptions) error {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("system bus: %w", err)
	}
	defer conn.Close()
	// A shutdown marked before is over: this is a new boot, or a guard
	// restarted.
	if err := Cancel(); err != nil {
		return err
	}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface(manager), dbus.WithMatchMember("PrepareForShutdown")); err != nil {
		return fmt.Errorf("logind's shutdown signal: %w", err)
	}
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	lock, err := inhibit(conn)
	if err != nil {
		return err
	}
	defer func() {
		if lock != nil {
			lock.Close()
		}
	}()
	o.Log("holding node shutdowns for checkpoints (at most %s)", o.Wait)
	for {
		select {
		case <-ctx.Done():
			return nil
		case sig, ok := <-signals:
			if !ok {
				return errors.New("system bus: disconnected")
			}
			if sig.Name != manager+".PrepareForShutdown" || len(sig.Body) != 1 {
				continue
			}
			if starting, _ := sig.Body[0].(bool); !starting {
				// Called off: hold the next one.
				o.Log("node shutdown called off")
				if err := Cancel(); err != nil {
					o.Log("%v", err)
				}
				if lock == nil {
					if lock, err = inhibit(conn); err != nil {
						return err
					}
				}
				continue
			}
			if lock == nil {
				continue
			}
			o.Log("node shutting down: checkpointing the pods that opted in, as the kubelet stops them")
			if err := Begin(); err != nil {
				o.Log("%v: not checkpointing", err)
			} else {
				o.hold(ctx, conn)
			}
			lock.Close()
			lock = nil
		}
	}
}

// hold pushes checkpoints as the shims write them, until the kubelet has
// stopped its pods and every checkpoint is pushed or failed, or o.Wait.
func (o GuardOptions) hold(ctx context.Context, conn *dbus.Conn) {
	deadline := time.Now().Add(o.Wait)
	if !kubeletHolds(conn) {
		o.Log("the kubelet holds no shutdown lock: graceful node shutdown is off (shutdownGracePeriod), and pods are stopped without checkpoints")
	}
	var (
		mu      sync.Mutex
		pushing = map[string]bool{}
		wg      sync.WaitGroup
	)
	for {
		states, err := States()
		if err != nil {
			o.Log("%v", err)
		}
		busy := false
		for uid, state := range states {
			switch state {
			case Pending:
				busy = true
			case Ready:
				busy = true
				mu.Lock()
				started := pushing[uid]
				pushing[uid] = true
				mu.Unlock()
				if started {
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					begin := time.Now()
					if err := o.Push(ctx, uid); err != nil {
						o.Log("pod %s: push: %v", uid, err)
						_ = Set(uid, Failed, "push: "+err.Error())
						return
					}
					_ = Set(uid, Pushed, "")
					o.Log("pod %s: checkpoint pushed in %s", uid, time.Since(begin).Round(time.Millisecond))
				}()
			}
		}
		if !busy && !kubeletHolds(conn) {
			break
		}
		if time.Now().After(deadline) {
			o.Log("letting the shutdown go on before every checkpoint is pushed: waited %s", o.Wait)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	wg.Wait()
	o.Log("checkpoints done: letting the shutdown go on")
}

// inhibit takes a delay lock on shutdowns; closing it releases them.
func inhibit(conn *dbus.Conn) (*os.File, error) {
	var fd dbus.UnixFD
	err := conn.Object(login1, "/org/freedesktop/login1").Call(manager+".Inhibit", 0,
		"shutdown", "gcr", "Checkpointing pods that move to other nodes", "delay").Store(&fd)
	if err != nil {
		return nil, fmt.Errorf("logind: taking a shutdown lock: %w", err)
	}
	return os.NewFile(uintptr(fd), "gcr-shutdown-lock"), nil
}

// kubeletHolds says whether the kubelet holds its shutdown lock: it does
// until it has stopped every pod, graceful node shutdown on.
func kubeletHolds(conn *dbus.Conn) bool {
	var locks []struct {
		What, Who, Why, Mode string
		UID, PID             uint32
	}
	if err := conn.Object(login1, "/org/freedesktop/login1").Call(manager+".ListInhibitors", 0).Store(&locks); err != nil {
		return false
	}
	for _, l := range locks {
		if l.Who == "kubelet" && strings.Contains(l.What, "shutdown") {
			return true
		}
	}
	return false
}
