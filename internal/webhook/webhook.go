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

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	annotationRunOnce = "locked-init.io/run-once"
	volumeName        = "locked-init-bin"
	mountPath         = "/locked-init-bin"
	binaryPath        = mountPath + "/locked-init"
	initImage         = "locked-init:latest"
	imageEnvVar       = "LOCKED_INIT_IMAGE"
)

// PodMutator handles mutating admission requests for Pods.
type PodMutator struct {
	Logger  *zap.Logger
	Image   string
	decoder admission.Decoder
}

// Handle processes the admission request and returns a response with patches.
func (m *PodMutator) Handle(_ context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := m.decoder.Decode(req, pod); err != nil {
		m.Logger.Error("Failed to decode pod", zap.Error(err))
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Only mutate pods with the run-once annotation.
	if pod.Annotations == nil || pod.Annotations[annotationRunOnce] != "true" {
		return admission.Allowed("no mutation needed")
	}

	m.Logger.Info("Mutating pod",
		zap.String("name", pod.Name),
		zap.String("generateName", pod.GenerateName),
		zap.String("namespace", req.Namespace),
	)

	lockName := determineLockName(pod, req)

	patches, err := buildPatches(pod, lockName, m.Image)
	if err != nil {
		m.Logger.Error("Failed to build patches", zap.Error(err))
		return admission.Errored(http.StatusInternalServerError, err)
	}

	if len(patches) == 0 {
		return admission.Allowed("no init containers to wrap")
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		m.Logger.Error("Failed to marshal patches", zap.Error(err))
		return admission.Errored(http.StatusInternalServerError, err)
	}

	m.Logger.Info("Applying patches", zap.Int("count", len(patches)))
	return admission.Response{
		AdmissionResponse: admissionv1.AdmissionResponse{
			Allowed: true,
			PatchType: func() *admissionv1.PatchType {
				pt := admissionv1.PatchTypeJSONPatch
				return &pt
			}(),
			Patch: patchBytes,
		},
	}
}

// InjectDecoder injects the admission decoder.
func (m *PodMutator) InjectDecoder(d admission.Decoder) error {
	m.decoder = d
	return nil
}

// jsonPatch represents a single JSON Patch operation.
type jsonPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func buildPatches(pod *corev1.Pod, lockName, image string) ([]jsonPatch, error) {
	if len(pod.Spec.InitContainers) == 0 {
		return nil, nil
	}

	var patches []jsonPatch

	// 1. Add the emptyDir volume.
	volume := corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	if len(pod.Spec.Volumes) == 0 {
		patches = append(patches, jsonPatch{
			Op:    "add",
			Path:  "/spec/volumes",
			Value: []corev1.Volume{volume},
		})
	} else {
		patches = append(patches, jsonPatch{
			Op:    "add",
			Path:  "/spec/volumes/-",
			Value: volume,
		})
	}

	// 2. Prepend a copy-binary initContainer.
	copyContainer := corev1.Container{
		Name:    "locked-init-copy",
		Image:   image,
		Command: []string{"/bin/locked-init", "copy", mountPath + "/locked-init"},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      volumeName,
			MountPath: mountPath,
		}},
	}

	// Build the new initContainers list: copy container first, then mutated originals.
	newInitContainers := []corev1.Container{copyContainer}
	for _, ic := range pod.Spec.InitContainers {
		mutated := ic.DeepCopy()

		// Add volume mount.
		mutated.VolumeMounts = append(mutated.VolumeMounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: mountPath,
		})

		// Generate a per-container lock name.
		containerLockName := fmt.Sprintf("%s-%s", lockName, ic.Name)

		// Prepend the wrapper to the command.
		originalCmd := buildOriginalCommand(ic)
		if len(originalCmd) == 0 {
			// Skip containers with no command (they use the image entrypoint).
			newInitContainers = append(newInitContainers, ic)
			continue
		}

		wrappedCmd := append([]string{binaryPath, "--name=" + containerLockName, "--"}, originalCmd...)
		mutated.Command = wrappedCmd
		mutated.Args = nil // Args are now part of the wrapped command.

		newInitContainers = append(newInitContainers, *mutated)
	}

	// Replace the entire initContainers array.
	patches = append(patches, jsonPatch{
		Op:    "replace",
		Path:  "/spec/initContainers",
		Value: newInitContainers,
	})

	return patches, nil
}

// buildOriginalCommand reconstructs the full command from Command and Args.
func buildOriginalCommand(c corev1.Container) []string {
	var cmd []string
	cmd = append(cmd, c.Command...)
	cmd = append(cmd, c.Args...)
	return cmd
}

// determineLockName generates a deterministic lock name from the pod's owner reference.
func determineLockName(pod *corev1.Pod, req admission.Request) string {
	// Use OwnerReference to get a stable name across replicas.
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return sanitizeName(fmt.Sprintf("%s-%s", strings.ToLower(ref.Kind), ref.Name))
		}
	}

	// Fall back to generateName (strip trailing dash).
	if pod.GenerateName != "" {
		return sanitizeName(strings.TrimRight(pod.GenerateName, "-"))
	}

	// Last resort: use namespace-name.
	return sanitizeName(fmt.Sprintf("%s-%s", req.Namespace, pod.Name))
}

// sanitizeName ensures the name is valid for a Kubernetes Lease object.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	// Lease names must be DNS subdomains: max 253 chars, alphanumeric and hyphens.
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}
