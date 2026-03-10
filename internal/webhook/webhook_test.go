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
	"net/http"
	"testing"

	"go.uber.org/zap/zaptest"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const testImage = "ghcr.io/test/locked-init/wrapper:latest"

func TestBuildPatches_NoInitContainers(t *testing.T) {
	pod := &corev1.Pod{}
	patches, err := buildPatches(pod, "test-lock", testImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patches != nil {
		t.Errorf("expected nil patches, got %d", len(patches))
	}
}

func TestBuildPatches_SingleInitContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:    "migrate",
				Image:   "myapp:latest",
				Command: []string{"npm", "run", "migrate"},
			}},
		},
	}

	patches, err := buildPatches(pod, "test-lock", testImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect 2 patches: add volumes array + replace initContainers.
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(patches))
	}

	// First patch: add volumes.
	if patches[0].Op != "add" || patches[0].Path != "/spec/volumes" {
		t.Errorf("expected add /spec/volumes, got %s %s", patches[0].Op, patches[0].Path)
	}

	// Second patch: replace initContainers.
	if patches[1].Op != "replace" || patches[1].Path != "/spec/initContainers" {
		t.Errorf("expected replace /spec/initContainers, got %s %s", patches[1].Op, patches[1].Path)
	}

	containers, ok := patches[1].Value.([]corev1.Container)
	if !ok {
		t.Fatal("expected []corev1.Container value")
	}

	// Should have 2 containers: copy + wrapped migrate.
	if len(containers) != 2 {
		t.Fatalf("expected 2 init containers, got %d", len(containers))
	}

	// Verify copy container.
	if containers[0].Name != "locked-init-copy" {
		t.Errorf("expected copy container name, got %q", containers[0].Name)
	}
	if containers[0].Image != testImage {
		t.Errorf("expected image %q, got %q", testImage, containers[0].Image)
	}

	// Verify wrapped command.
	expected := []string{binaryPath, "--name=test-lock-migrate", "--", "npm", "run", "migrate"}
	if len(containers[1].Command) != len(expected) {
		t.Fatalf("expected command %v, got %v", expected, containers[1].Command)
	}
	for i, arg := range expected {
		if containers[1].Command[i] != arg {
			t.Errorf("command[%d]: expected %q, got %q", i, arg, containers[1].Command[i])
		}
	}
	if containers[1].Args != nil {
		t.Errorf("expected nil args, got %v", containers[1].Args)
	}
}

func TestBuildPatches_ExistingVolumes(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "existing",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}},
			InitContainers: []corev1.Container{{
				Name:    "migrate",
				Image:   "myapp:latest",
				Command: []string{"migrate"},
			}},
		},
	}

	patches, err := buildPatches(pod, "lock", testImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Volume patch should use append (/-) when volumes already exist.
	if patches[0].Op != "add" || patches[0].Path != "/spec/volumes/-" {
		t.Errorf("expected add /spec/volumes/-, got %s %s", patches[0].Op, patches[0].Path)
	}
}

func TestBuildPatches_ContainerWithArgs(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:    "migrate",
				Image:   "myapp:latest",
				Command: []string{"python"},
				Args:    []string{"manage.py", "migrate"},
			}},
		},
	}

	patches, err := buildPatches(pod, "lock", testImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers := patches[1].Value.([]corev1.Container)
	wrapped := containers[1]

	// Command + Args should be merged into Command.
	expected := []string{binaryPath, "--name=lock-migrate", "--", "python", "manage.py", "migrate"}
	if len(wrapped.Command) != len(expected) {
		t.Fatalf("expected command %v, got %v", expected, wrapped.Command)
	}
	for i, arg := range expected {
		if wrapped.Command[i] != arg {
			t.Errorf("command[%d]: expected %q, got %q", i, arg, wrapped.Command[i])
		}
	}
}

func TestBuildPatches_NoCommand(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:  "entrypoint-only",
				Image: "myapp:latest",
				// No Command or Args — uses image ENTRYPOINT.
			}},
		},
	}

	patches, err := buildPatches(pod, "lock", testImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers := patches[1].Value.([]corev1.Container)
	// Copy container + unmutated original.
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}
	// The original should be passed through unchanged.
	if containers[1].Name != "entrypoint-only" {
		t.Errorf("expected original container, got %q", containers[1].Name)
	}
	if containers[1].Command != nil {
		t.Errorf("expected nil command for entrypoint-only container, got %v", containers[1].Command)
	}
}

func TestBuildPatches_MultipleInitContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "migrate", Image: "app:latest", Command: []string{"migrate"}},
				{Name: "seed", Image: "app:latest", Command: []string{"seed"}},
			},
		},
	}

	patches, err := buildPatches(pod, "deploy", testImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers := patches[1].Value.([]corev1.Container)
	// copy + 2 wrapped = 3.
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	// Each gets its own lock name.
	if containers[1].Command[1] != "--name=deploy-migrate" {
		t.Errorf("expected --name=deploy-migrate, got %q", containers[1].Command[1])
	}
	if containers[2].Command[1] != "--name=deploy-seed" {
		t.Errorf("expected --name=deploy-seed, got %q", containers[2].Command[1])
	}
}

func TestBuildPatches_VolumeMount(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:    "migrate",
				Image:   "app:latest",
				Command: []string{"migrate"},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: "/data",
				}},
			}},
		},
	}

	patches, err := buildPatches(pod, "lock", testImage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers := patches[1].Value.([]corev1.Container)
	wrapped := containers[1]

	// Should have original mount + locked-init-bin mount.
	if len(wrapped.VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(wrapped.VolumeMounts))
	}
	if wrapped.VolumeMounts[0].Name != "data" {
		t.Errorf("expected original mount first, got %q", wrapped.VolumeMounts[0].Name)
	}
	if wrapped.VolumeMounts[1].Name != volumeName {
		t.Errorf("expected locked-init-bin mount second, got %q", wrapped.VolumeMounts[1].Name)
	}
}

func TestDetermineLockName_OwnerReference(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "ReplicaSet",
				Name:       "myapp-7f8b9c",
				Controller: ptr.To(true),
			}},
		},
	}
	name := determineLockName(pod, admission.Request{})
	if name != "replicaset-myapp-7f8b9c" {
		t.Errorf("expected replicaset-myapp-7f8b9c, got %q", name)
	}
}

func TestDetermineLockName_GenerateName(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "myapp-",
		},
	}
	name := determineLockName(pod, admission.Request{})
	if name != "myapp" {
		t.Errorf("expected myapp, got %q", name)
	}
}

func TestDetermineLockName_FallbackToName(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "standalone-pod",
		},
	}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Namespace: "production",
		},
	}
	name := determineLockName(pod, req)
	if name != "production-standalone-pod" {
		t.Errorf("expected production-standalone-pod, got %q", name)
	}
}

func TestDetermineLockName_NonControllerOwner(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "myapp-",
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "ReplicaSet",
				Name:       "myapp-abc",
				Controller: ptr.To(false),
			}},
		},
	}
	// Non-controller owner should be skipped, fall back to generateName.
	name := determineLockName(pod, admission.Request{})
	if name != "myapp" {
		t.Errorf("expected myapp, got %q", name)
	}
}

func TestSanitizeName_Truncation(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "a"
	}
	result := sanitizeName(long)
	if len(result) != 253 {
		t.Errorf("expected length 253, got %d", len(result))
	}
}

func TestSanitizeName_Lowercase(t *testing.T) {
	result := sanitizeName("MyApp-Deploy")
	if result != "myapp-deploy" {
		t.Errorf("expected myapp-deploy, got %q", result)
	}
}

// TestHandle_NoAnnotation verifies pods without the annotation pass through.
func TestHandle_NoAnnotation(t *testing.T) {
	mutator := &PodMutator{
		Logger: zaptest.NewLogger(t),
		Image:  testImage,
	}
	// Need to register corev1 types.
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = mutator.InjectDecoder(admission.NewDecoder(scheme))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-annotation",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "app:latest",
			}},
		},
	}

	raw, _ := json.Marshal(pod)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Object:    runtime.RawExtension{Raw: raw},
			Namespace: "default",
		},
	}

	resp := mutator.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatal("expected request to be allowed")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch, got %s", string(resp.Patch))
	}
}

// TestHandle_WithAnnotation verifies the full mutation flow.
func TestHandle_WithAnnotation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	mutator := &PodMutator{
		Logger: zaptest.NewLogger(t),
		Image:  testImage,
	}
	_ = mutator.InjectDecoder(admission.NewDecoder(scheme))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Annotations: map[string]string{
				annotationRunOnce: "true",
			},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "ReplicaSet",
				Name:       "myapp-abc123",
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:    "migrate",
				Image:   "myapp:latest",
				Command: []string{"npm", "run", "migrate"},
			}},
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "myapp:latest",
			}},
		},
	}

	raw, _ := json.Marshal(pod)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Object:    runtime.RawExtension{Raw: raw},
			Namespace: "default",
		},
	}

	resp := mutator.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatal("expected request to be allowed")
	}
	if resp.Patch == nil {
		t.Fatal("expected patches to be returned")
	}
	if *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Errorf("expected JSONPatch type, got %v", *resp.PatchType)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("unmarshaling patches: %v", err)
	}
	if len(patches) != 2 {
		t.Errorf("expected 2 patches, got %d", len(patches))
	}
}

// TestHandle_NoInitContainers verifies pods with annotation but no init containers.
func TestHandle_NoInitContainers(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	mutator := &PodMutator{
		Logger: zaptest.NewLogger(t),
		Image:  testImage,
	}
	_ = mutator.InjectDecoder(admission.NewDecoder(scheme))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-init",
			Namespace: "default",
			Annotations: map[string]string{
				annotationRunOnce: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "app:latest",
			}},
		},
	}

	raw, _ := json.Marshal(pod)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Object:    runtime.RawExtension{Raw: raw},
			Namespace: "default",
		},
	}

	resp := mutator.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatal("expected request to be allowed")
	}
	if resp.Patch != nil {
		t.Error("expected no patches for pod without init containers")
	}
}

// TestHandle_BadPayload verifies error handling for invalid requests.
func TestHandle_BadPayload(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	mutator := &PodMutator{
		Logger: zaptest.NewLogger(t),
		Image:  testImage,
	}
	_ = mutator.InjectDecoder(admission.NewDecoder(scheme))

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:    "test-uid",
			Object: runtime.RawExtension{Raw: []byte(`{invalid`)},
		},
	}

	resp := mutator.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected request to be rejected")
	}
	if resp.Result.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.Result.Code)
	}
}
