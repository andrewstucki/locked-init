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

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/andrewstucki/locked-init/internal/cleanup"
	"github.com/andrewstucki/locked-init/internal/webhook"
	"github.com/redpanda-data/common-go/kube"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	webhookPort   int
	logLevel      string
	image         string
	leaseTTL      time.Duration
	sweepInterval time.Duration
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "webhook",
		Short: "Locked-init mutating admission webhook server",
		RunE:  run,
	}

	rootCmd.Flags().IntVar(&webhookPort, "port", 9443, "Webhook server port")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.Flags().StringVar(&image, "image", "", "locked-init image for the copy initContainer (required)")
	rootCmd.Flags().DurationVar(&leaseTTL, "lease-ttl", 2*time.Hour, "Delete completed Leases older than this duration (0 to disable)")
	rootCmd.Flags().DurationVar(&sweepInterval, "sweep-interval", 1*time.Hour, "How often to check for expired Leases")
	_ = rootCmd.MarkFlagRequired("image")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	logger, err := newLogger(logLevel)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	defer logger.Sync() //nolint:errcheck

	// Set controller-runtime logger.
	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseFlagOptions(&ctrlzap.Options{})))

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			return fmt.Errorf("POD_NAMESPACE not set and could not read namespace from service account: %w", err)
		}
		namespace = string(data)
	}

	webhookName := envOrDefault("WEBHOOK_NAME", "locked-init-webhook")
	secretName := envOrDefault("WEBHOOK_SECRET_NAME", "locked-init-webhook-tls")
	serviceName := envOrDefault("WEBHOOK_SERVICE_NAME", "locked-init-webhook")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port: webhookPort,
		}),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	// Set up certificate rotation using the redpanda CertRotator.
	rotatorConfig := kube.CertRotatorConfig{
		SecretKey: types.NamespacedName{
			Namespace: namespace,
			Name:      secretName,
		},
		DNSName: fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		Webhooks: []kube.WebhookInfo{{
			Type: kube.Mutating,
			Name: webhookName,
		}},
		ControllerName: "locked-init-cert-rotator",
	}

	rotator := kube.NewCertRotator(rotatorConfig)
	if err := kube.AddRotator(mgr, rotator); err != nil {
		return fmt.Errorf("adding cert rotator: %w", err)
	}

	// Register the mutating webhook handler.
	mutator := &webhook.PodMutator{
		Logger: logger,
		Image:  image,
	}
	decoder := admission.NewDecoder(mgr.GetScheme())
	_ = mutator.InjectDecoder(decoder)

	mgr.GetWebhookServer().Register("/mutate-pods", &ctrlwebhook.Admission{Handler: mutator})

	// Register lease reaper if TTL is configured.
	if leaseTTL > 0 {
		clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			return fmt.Errorf("creating kubernetes clientset: %w", err)
		}
		if err := mgr.Add(&cleanup.LeaseReaper{
			Client:   clientset,
			Logger:   logger.Named("lease-reaper"),
			TTL:      leaseTTL,
			Interval: sweepInterval,
		}); err != nil {
			return fmt.Errorf("adding lease reaper: %w", err)
		}
	}

	logger.Info("Starting webhook server",
		zap.Int("port", webhookPort),
		zap.String("namespace", namespace),
		zap.String("image", image),
	)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func newLogger(level string) (*zap.Logger, error) {
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = zapcore.InfoLevel
	}

	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg.Build()
}
