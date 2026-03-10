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
	"time"

	"github.com/andrewstucki/locked-init/internal/leader"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LeaseReaper periodically deletes completed Lease objects older than a TTL.
// It implements manager.Runnable so it can be added to a controller-runtime manager.
type LeaseReaper struct {
	Client   kubernetes.Interface
	Logger   *zap.Logger
	TTL      time.Duration
	Interval time.Duration
}

// Start runs the reaper loop until the context is cancelled.
func (r *LeaseReaper) Start(ctx context.Context) error {
	r.Logger.Info("Starting lease reaper",
		zap.Duration("ttl", r.TTL),
		zap.Duration("interval", r.Interval),
	)

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

func (r *LeaseReaper) sweep(ctx context.Context) {
	// List leases across all namespaces that have the completed annotation.
	namespaces, err := r.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		r.Logger.Error("Failed to list namespaces", zap.Error(err))
		return
	}

	now := time.Now().UTC()
	var deleted int

	for _, ns := range namespaces.Items {
		leases, err := r.Client.CoordinationV1().Leases(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			r.Logger.Debug("Failed to list leases", zap.String("namespace", ns.Name), zap.Error(err))
			continue
		}

		for _, lease := range leases.Items {
			if lease.Annotations[leader.StatusAnnotation] != leader.StatusCompleted {
				continue
			}

			completedAtStr, ok := lease.Annotations[leader.CompletedAtAnnotation]
			if !ok {
				continue
			}

			completedAt, err := time.Parse(time.RFC3339, completedAtStr)
			if err != nil {
				r.Logger.Debug("Invalid completed-at timestamp",
					zap.String("lease", lease.Name),
					zap.String("namespace", ns.Name),
					zap.String("value", completedAtStr),
				)
				continue
			}

			if now.Sub(completedAt) < r.TTL {
				continue
			}

			if err := r.Client.CoordinationV1().Leases(ns.Name).Delete(ctx, lease.Name, metav1.DeleteOptions{}); err != nil {
				r.Logger.Error("Failed to delete expired lease",
					zap.String("lease", lease.Name),
					zap.String("namespace", ns.Name),
					zap.Error(err),
				)
				continue
			}

			deleted++
			r.Logger.Info("Deleted expired lease",
				zap.String("lease", lease.Name),
				zap.String("namespace", ns.Name),
				zap.Time("completedAt", completedAt),
			)
		}
	}

	if deleted > 0 {
		r.Logger.Info("Sweep complete", zap.Int("deleted", deleted))
	}
}
