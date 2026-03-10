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
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type testEnvResult struct {
	client kubernetes.Interface
	config *rest.Config
}

func setupEnvtest(t *testing.T) testEnvResult {
	t.Helper()

	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return testEnvResult{client: client, config: cfg}
}

func TestIsLeaseCompleted(t *testing.T) {
	env := setupEnvtest(t)
	ctx := context.Background()

	// Create an incomplete lease.
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lease",
			Namespace: "default",
		},
	}
	_, err := env.client.CoordinationV1().Leases("default").Create(ctx, lease, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating lease: %v", err)
	}

	// Should not be completed.
	completed, err := isLeaseCompleted(ctx, env.client, "default", "test-lease")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Fatal("expected lease to not be completed")
	}

	// Re-fetch to get resourceVersion, then mark completed.
	lease, err = env.client.CoordinationV1().Leases("default").Get(ctx, "test-lease", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	lease.Annotations = map[string]string{StatusAnnotation: StatusCompleted}
	_, err = env.client.CoordinationV1().Leases("default").Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("updating lease: %v", err)
	}

	// Should now be completed.
	completed, err = isLeaseCompleted(ctx, env.client, "default", "test-lease")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !completed {
		t.Fatal("expected lease to be completed")
	}
}

func TestIsLeaseCompleted_NotFound(t *testing.T) {
	env := setupEnvtest(t)
	ctx := context.Background()

	_, err := isLeaseCompleted(ctx, env.client, "default", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent lease")
	}
}

func TestMarkLeaseCompleted(t *testing.T) {
	env := setupEnvtest(t)
	ctx := context.Background()

	// Create a lease without annotations.
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mark-test",
			Namespace: "default",
		},
	}
	_, err := env.client.CoordinationV1().Leases("default").Create(ctx, lease, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating lease: %v", err)
	}

	// Mark it completed.
	err = markLeaseCompleted(ctx, env.client, "default", "mark-test")
	if err != nil {
		t.Fatalf("marking lease completed: %v", err)
	}

	// Verify annotations.
	updated, err := env.client.CoordinationV1().Leases("default").Get(ctx, "mark-test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if updated.Annotations[StatusAnnotation] != StatusCompleted {
		t.Errorf("expected status=%q, got %q", StatusCompleted, updated.Annotations[StatusAnnotation])
	}
	completedAt, ok := updated.Annotations[CompletedAtAnnotation]
	if !ok {
		t.Fatal("expected completed-at annotation to be set")
	}
	ts, err := time.Parse(time.RFC3339, completedAt)
	if err != nil {
		t.Fatalf("parsing completed-at: %v", err)
	}
	if time.Since(ts) > 10*time.Second {
		t.Errorf("completed-at timestamp too old: %v", ts)
	}
}

func TestMarkLeaseCompleted_NotFound(t *testing.T) {
	env := setupEnvtest(t)
	ctx := context.Background()

	err := markLeaseCompleted(ctx, env.client, "default", "does-not-exist")
	if err == nil {
		t.Fatal("expected error for nonexistent lease")
	}
}

func TestRunCommand_Success(t *testing.T) {
	logger := zaptest.NewLogger(t)
	code := runCommand(context.Background(), []string{"true"}, logger)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
}

func TestRunCommand_Failure(t *testing.T) {
	logger := zaptest.NewLogger(t)
	code := runCommand(context.Background(), []string{"false"}, logger)
	if code == 0 {
		t.Error("expected non-zero exit code")
	}
}

func TestRunCommand_NonExistent(t *testing.T) {
	logger := zaptest.NewLogger(t)
	code := runCommand(context.Background(), []string{"/nonexistent/binary"}, logger)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

func TestRunCommand_Empty(t *testing.T) {
	logger := zaptest.NewLogger(t)
	code := runCommand(context.Background(), nil, logger)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

func TestRunCommand_ContextCancelled(t *testing.T) {
	logger := zaptest.NewLogger(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() {
		done <- runCommand(ctx, []string{"sleep", "60"}, logger)
	}()

	cancel()
	code := <-done
	if code == 0 {
		t.Error("expected non-zero exit code after cancellation")
	}
}

func TestRunCommand_ExitCode(t *testing.T) {
	logger := zaptest.NewLogger(t)
	// sh -c "exit 42" produces exit code 42.
	code := runCommand(context.Background(), []string{"sh", "-c", "exit 42"}, logger)
	if code != 42 {
		t.Errorf("expected exit code 42, got %d", code)
	}
}

// TestRun_FastPath verifies that Run exits immediately when the lease is already completed.
func TestRun_FastPath(t *testing.T) {
	env := setupEnvtest(t)
	ctx := context.Background()

	// Pre-create a completed lease.
	_, err := env.client.CoordinationV1().Leases("default").Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fast-path",
			Namespace: "default",
			Annotations: map[string]string{
				StatusAnnotation:      StatusCompleted,
				CompletedAtAnnotation: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating lease: %v", err)
	}

	code, err := Run(ctx, Config{
		LockName:   "fast-path",
		Namespace:  "default",
		Command:    []string{"sh", "-c", "echo SHOULD NOT RUN && exit 1"},
		Logger:     zaptest.NewLogger(t),
		RestConfig: env.config,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
}

// TestRun_LeaderSuccess verifies the full leader flow: win election, run command, mark completed.
func TestRun_LeaderSuccess(t *testing.T) {
	env := setupEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	code, err := Run(ctx, Config{
		LockName:   "leader-success",
		Namespace:  "default",
		Command:    []string{"true"},
		Logger:     zaptest.NewLogger(t),
		RestConfig: env.config,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}

	// Verify the lease was marked completed.
	lease, err := env.client.CoordinationV1().Leases("default").Get(ctx, "leader-success", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if lease.Annotations[StatusAnnotation] != StatusCompleted {
		t.Errorf("expected lease to be completed, got annotations: %v", lease.Annotations)
	}
	if _, ok := lease.Annotations[CompletedAtAnnotation]; !ok {
		t.Error("expected completed-at annotation")
	}
}

// TestRun_LeaderFailure verifies that a failed command returns the child's exit code.
func TestRun_LeaderFailure(t *testing.T) {
	env := setupEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	code, err := Run(ctx, Config{
		LockName:   "leader-failure",
		Namespace:  "default",
		Command:    []string{"sh", "-c", "exit 3"},
		Logger:     zaptest.NewLogger(t),
		RestConfig: env.config,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 3 {
		t.Errorf("expected exit code 3, got %d", code)
	}

	// Verify the lease was NOT marked completed.
	lease, err := env.client.CoordinationV1().Leases("default").Get(ctx, "leader-failure", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting lease: %v", err)
	}
	if lease.Annotations[StatusAnnotation] == StatusCompleted {
		t.Error("expected lease to NOT be completed after command failure")
	}
}

// TestRun_ContextCancelled verifies Run returns an error when the parent context is cancelled.
func TestRun_ContextCancelled(t *testing.T) {
	env := setupEnvtest(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var code int
	var runErr error
	go func() {
		code, runErr = Run(ctx, Config{
			LockName:   "ctx-cancel",
			Namespace:  "default",
			Command:    []string{"sleep", "60"},
			Logger:     zaptest.NewLogger(t),
			RestConfig: env.config,
		})
		close(done)
	}()

	// Give it time to start the election, then cancel.
	time.Sleep(1 * time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if runErr == nil && code == 0 {
		t.Error("expected non-zero exit or error after cancellation")
	}
}

// TestRun_FollowerSeesCompletion verifies that a second runner exits 0
// when it finds the lease already completed by a prior leader.
func TestRun_FollowerSeesCompletion(t *testing.T) {
	env := setupEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First run: leader succeeds and marks lease completed.
	code, err := Run(ctx, Config{
		LockName:   "follower-test",
		Namespace:  "default",
		Command:    []string{"true"},
		Logger:     zaptest.NewLogger(t).Named("leader"),
		RestConfig: env.config,
	})
	if err != nil {
		t.Fatalf("leader run error: %v", err)
	}
	if code != 0 {
		t.Fatalf("leader expected exit 0, got %d", code)
	}

	// Second run: should see completed lease and exit immediately (fast path).
	code, err = Run(ctx, Config{
		LockName:   "follower-test",
		Namespace:  "default",
		Command:    []string{"sh", "-c", "echo SHOULD NOT RUN && exit 1"},
		Logger:     zaptest.NewLogger(t).Named("follower"),
		RestConfig: env.config,
	})
	if err != nil {
		t.Fatalf("follower run error: %v", err)
	}
	if code != 0 {
		t.Errorf("follower expected exit 0, got %d", code)
	}
}
