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

package cleanup

import (
	"context"
	"testing"
	"time"

	"github.com/andrewstucki/locked-init/internal/leader"
	"go.uber.org/zap/zaptest"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func setupEnvtest(t *testing.T) kubernetes.Interface {
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
	return client
}

func createNamespace(t *testing.T, client kubernetes.Interface, name string) {
	t.Helper()
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
}

func createLease(t *testing.T, client kubernetes.Interface, namespace, name string, annotations map[string]string) {
	t.Helper()
	_, err := client.CoordinationV1().Leases(namespace).Create(context.Background(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating lease %s/%s: %v", namespace, name, err)
	}
}

func leaseExists(t *testing.T, client kubernetes.Interface, namespace, name string) bool {
	t.Helper()
	_, err := client.CoordinationV1().Leases(namespace).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

func TestSweep_DeletesExpiredLeases(t *testing.T) {
	client := setupEnvtest(t)
	createNamespace(t, client, "test-ns")

	// Create an expired completed lease (completed 3 hours ago).
	createLease(t, client, "test-ns", "expired-lease", map[string]string{
		leader.StatusAnnotation:      leader.StatusCompleted,
		leader.CompletedAtAnnotation: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
	})

	reaper := &LeaseReaper{
		Client: client,
		Logger: zaptest.NewLogger(t),
		TTL:    1 * time.Hour,
	}

	reaper.sweep(context.Background())

	if leaseExists(t, client, "test-ns", "expired-lease") {
		t.Error("expected expired lease to be deleted")
	}
}

func TestSweep_KeepsFreshLeases(t *testing.T) {
	client := setupEnvtest(t)
	createNamespace(t, client, "test-ns")

	// Create a recently completed lease (completed 5 minutes ago).
	createLease(t, client, "test-ns", "fresh-lease", map[string]string{
		leader.StatusAnnotation:      leader.StatusCompleted,
		leader.CompletedAtAnnotation: time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339),
	})

	reaper := &LeaseReaper{
		Client: client,
		Logger: zaptest.NewLogger(t),
		TTL:    1 * time.Hour,
	}

	reaper.sweep(context.Background())

	if !leaseExists(t, client, "test-ns", "fresh-lease") {
		t.Error("expected fresh lease to be kept")
	}
}

func TestSweep_IgnoresNonCompletedLeases(t *testing.T) {
	client := setupEnvtest(t)
	createNamespace(t, client, "test-ns")

	// Create a lease without the completed annotation (e.g., active leader election).
	createLease(t, client, "test-ns", "active-lease", nil)

	reaper := &LeaseReaper{
		Client: client,
		Logger: zaptest.NewLogger(t),
		TTL:    1 * time.Hour,
	}

	reaper.sweep(context.Background())

	if !leaseExists(t, client, "test-ns", "active-lease") {
		t.Error("expected non-completed lease to be kept")
	}
}

func TestSweep_IgnoresMissingTimestamp(t *testing.T) {
	client := setupEnvtest(t)
	createNamespace(t, client, "test-ns")

	// Completed but missing the completed-at annotation.
	createLease(t, client, "test-ns", "no-timestamp", map[string]string{
		leader.StatusAnnotation: leader.StatusCompleted,
	})

	reaper := &LeaseReaper{
		Client: client,
		Logger: zaptest.NewLogger(t),
		TTL:    1 * time.Hour,
	}

	reaper.sweep(context.Background())

	if !leaseExists(t, client, "test-ns", "no-timestamp") {
		t.Error("expected lease without timestamp to be kept")
	}
}

func TestSweep_IgnoresInvalidTimestamp(t *testing.T) {
	client := setupEnvtest(t)
	createNamespace(t, client, "test-ns")

	createLease(t, client, "test-ns", "bad-timestamp", map[string]string{
		leader.StatusAnnotation:      leader.StatusCompleted,
		leader.CompletedAtAnnotation: "not-a-date",
	})

	reaper := &LeaseReaper{
		Client: client,
		Logger: zaptest.NewLogger(t),
		TTL:    1 * time.Hour,
	}

	reaper.sweep(context.Background())

	if !leaseExists(t, client, "test-ns", "bad-timestamp") {
		t.Error("expected lease with invalid timestamp to be kept")
	}
}

func TestSweep_MultipleNamespaces(t *testing.T) {
	client := setupEnvtest(t)
	createNamespace(t, client, "ns-a")
	createNamespace(t, client, "ns-b")

	expired := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	completedAnnotations := map[string]string{
		leader.StatusAnnotation:      leader.StatusCompleted,
		leader.CompletedAtAnnotation: expired,
	}

	createLease(t, client, "ns-a", "lease-a", completedAnnotations)
	createLease(t, client, "ns-b", "lease-b", completedAnnotations)

	reaper := &LeaseReaper{
		Client: client,
		Logger: zaptest.NewLogger(t),
		TTL:    1 * time.Hour,
	}

	reaper.sweep(context.Background())

	if leaseExists(t, client, "ns-a", "lease-a") {
		t.Error("expected lease-a to be deleted")
	}
	if leaseExists(t, client, "ns-b", "lease-b") {
		t.Error("expected lease-b to be deleted")
	}
}

func TestSweep_MixedLeases(t *testing.T) {
	client := setupEnvtest(t)
	createNamespace(t, client, "test-ns")

	// Expired.
	createLease(t, client, "test-ns", "old", map[string]string{
		leader.StatusAnnotation:      leader.StatusCompleted,
		leader.CompletedAtAnnotation: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
	})
	// Fresh.
	createLease(t, client, "test-ns", "new", map[string]string{
		leader.StatusAnnotation:      leader.StatusCompleted,
		leader.CompletedAtAnnotation: time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339),
	})
	// Active (no annotations).
	createLease(t, client, "test-ns", "active", nil)

	reaper := &LeaseReaper{
		Client: client,
		Logger: zaptest.NewLogger(t),
		TTL:    1 * time.Hour,
	}

	reaper.sweep(context.Background())

	if leaseExists(t, client, "test-ns", "old") {
		t.Error("expected old lease to be deleted")
	}
	if !leaseExists(t, client, "test-ns", "new") {
		t.Error("expected new lease to be kept")
	}
	if !leaseExists(t, client, "test-ns", "active") {
		t.Error("expected active lease to be kept")
	}
}

func TestStart_StopsOnContextCancel(t *testing.T) {
	client := setupEnvtest(t)

	reaper := &LeaseReaper{
		Client:   client,
		Logger:   zaptest.NewLogger(t),
		TTL:      1 * time.Hour,
		Interval: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reaper.Start(ctx)
	}()

	// Let it run a couple ticks.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not stop after context cancellation")
	}
}
