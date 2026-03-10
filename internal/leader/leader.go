// Copyright 2026 Andrew Stucki
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package leader

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	// StatusAnnotation is the Lease annotation key for completion status.
	StatusAnnotation = "locked-init.io/status"
	// StatusCompleted is the value that marks a Lease as finished.
	StatusCompleted = "completed"
	// CompletedAtAnnotation records when the Lease was marked completed (RFC 3339).
	CompletedAtAnnotation = "locked-init.io/completed-at"
)

// Config holds the configuration for the leader election wrapper.
type Config struct {
	LockName   string
	Namespace  string
	Command    []string
	Logger     *zap.Logger
	RestConfig *rest.Config // Optional; uses InClusterConfig if nil.
}

// Run executes the leader election wrapper logic.
//
// Guarantees: at most one instance runs the command concurrently (mutual
// exclusion via Lease). If the leader crashes after the command succeeds
// but before the annotation is written, a new leader will re-run the
// command — so the wrapped command should be idempotent.
func Run(ctx context.Context, cfg Config) (int, error) {
	logger := cfg.Logger

	restCfg := cfg.RestConfig
	if restCfg == nil {
		var err error
		restCfg, err = rest.InClusterConfig()
		if err != nil {
			return 1, fmt.Errorf("getting in-cluster config: %w", err)
		}
	}

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return 1, fmt.Errorf("creating kubernetes client: %w", err)
	}

	// Fast path: if the lease already has status=completed, exit immediately.
	completed, err := isLeaseCompleted(ctx, client, cfg.Namespace, cfg.LockName)
	if err != nil {
		logger.Warn("Failed to check lease status, proceeding with election", zap.Error(err))
	} else if completed {
		logger.Info("Lease already completed, exiting successfully")
		return 0, nil
	}

	identity, err := os.Hostname()
	if err != nil {
		return 1, fmt.Errorf("getting hostname: %w", err)
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      cfg.LockName,
			Namespace: cfg.Namespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	// Use a done channel + sync.Once to safely deliver exactly one result,
	// avoiding a race between the leader callback and the follower poll loop.
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	var sendOnce sync.Once
	sendResult := func(code int, err error) {
		sendOnce.Do(func() {
			done <- result{code: code, err: err}
		})
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				code := runCommand(ctx, cfg.Command, logger)
				if code != 0 {
					logger.Error("Command failed", zap.Int("exitCode", code))
					sendResult(code, nil)
					return
				}
				if err := markLeaseCompleted(ctx, client, cfg.Namespace, cfg.LockName); err != nil {
					logger.Error("Failed to mark lease as completed", zap.Error(err))
					sendResult(1, nil)
					return
				}
				logger.Info("Command succeeded, lease marked completed")
				sendResult(0, nil)
			},
			OnStoppedLeading: func() {
				logger.Info("Lost leadership")
			},
			OnNewLeader: func(current string) {
				if current == identity {
					return
				}
				logger.Info("Another pod is the leader, waiting for completion", zap.String("leader", current))
			},
		},
	}

	electionCtx, electionCancel := context.WithCancel(ctx)
	defer electionCancel()

	go leaderelection.RunOrDie(electionCtx, lec)

	// Follower poll loop: watches for the completed annotation regardless
	// of whether this instance is the leader or a follower.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-electionCtx.Done():
				return
			case <-ticker.C:
				completed, err := isLeaseCompleted(electionCtx, client, cfg.Namespace, cfg.LockName)
				if err != nil {
					logger.Debug("Error polling lease", zap.Error(err))
					continue
				}
				if completed {
					logger.Info("Lease marked completed by leader, exiting successfully")
					sendResult(0, nil)
					return
				}
			}
		}
	}()

	select {
	case r := <-done:
		electionCancel()
		return r.code, r.err
	case <-ctx.Done():
		electionCancel()
		return 1, ctx.Err()
	}
}

func isLeaseCompleted(ctx context.Context, client kubernetes.Interface, namespace, name string) (bool, error) {
	lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return lease.Annotations[StatusAnnotation] == StatusCompleted, nil
}

func markLeaseCompleted(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting lease: %w", err)
		}
		// Should not happen — leader election creates the Lease — but handle defensively.
		return fmt.Errorf("lease %s/%s not found after winning election: %w", namespace, name, err)
	}

	if lease.Annotations == nil {
		lease.Annotations = make(map[string]string)
	}
	lease.Annotations[StatusAnnotation] = StatusCompleted
	lease.Annotations[CompletedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	_, err = client.CoordinationV1().Leases(namespace).Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating lease annotation: %w", err)
	}
	return nil
}

func runCommand(ctx context.Context, args []string, logger *zap.Logger) int {
	if len(args) == 0 {
		logger.Error("No command specified")
		return 1
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	// Forward signals to child process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()
	defer signal.Stop(sigCh)

	logger.Info("Executing command", zap.Strings("args", args))
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		logger.Error("Failed to run command", zap.Error(err))
		return 1
	}
	return 0
}
