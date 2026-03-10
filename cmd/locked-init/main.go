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
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/andrewstucki/locked-init/internal/leader"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	lockName string
	logLevel string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "locked-init --name=<lock-name> -- <command> [args...]",
		Short: "Run an init command exactly once across scaled replicas using Kubernetes Lease leader election",
		Args:  cobra.MinimumNArgs(1),
		RunE:  run,
	}

	rootCmd.Flags().StringVar(&lockName, "name", "", "Name of the Kubernetes Lease to use for leader election (required)")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	_ = rootCmd.MarkFlagRequired("name")

	copyCmd := &cobra.Command{
		Use:   "copy <destination>",
		Short: "Copy the locked-init binary to a destination path",
		Args:  cobra.ExactArgs(1),
		RunE:  runCopy,
	}
	rootCmd.AddCommand(copyCmd)

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

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		// Fall back to reading the namespace from the service account.
		data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			return fmt.Errorf("POD_NAMESPACE not set and could not read namespace from service account: %w", err)
		}
		namespace = string(data)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	logger.Info("Starting locked-init",
		zap.String("lockName", lockName),
		zap.String("namespace", namespace),
		zap.Strings("command", args),
	)

	exitCode, err := leader.Run(ctx, leader.Config{
		LockName:  lockName,
		Namespace: namespace,
		Command:   args,
		Logger:    logger,
	})
	if err != nil {
		logger.Error("Leader election failed", zap.Error(err))
		os.Exit(1)
	}
	os.Exit(exitCode)
	return nil
}

func runCopy(_ *cobra.Command, args []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving own executable path: %w", err)
	}

	src, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("opening self: %w", err)
	}
	defer src.Close() //nolint:errcheck

	dst, err := os.OpenFile(args[0], os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating destination: %w", err)
	}
	defer dst.Close() //nolint:errcheck

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copying binary: %w", err)
	}
	return nil
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
