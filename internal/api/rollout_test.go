package api

import (
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestRolloutReportsPodsAndEvents(t *testing.T) {
	s, _, _ := sqlServer(t, podExecResult{}, nil, sqlDeployment("d1", benchv1alpha1.PostgresEmbedded, "wc1"))
	pulling := &corev1.Pod{
		Name: "d1-server-abc", Namespace: "d1-ns",
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "server", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}
	probing := &corev1.Pod{
		Name: "d1-worker-def", Namespace: "d1-ns",
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "worker", RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			Conditions:        []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse}},
		},
	}
	event := &corev1.Event{
		Name: "e1", Namespace: "d1-ns",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "d1-server-abc"},
		Type:           "Normal", Reason: "Pulling", Message: "Pulling image",
	}
	var gotContext string
	s.clusterClient = func(_ []byte, kubeContext string) (kubernetes.Interface, error) {
		gotContext = kubeContext
		return fake.NewClientset(pulling, probing, event), nil
	}

	rec := do(t, s, http.MethodGet, "/api/v1/deployments/d1/rollout", nil)
	requireStatus(t, rec, http.StatusOK)
	resp := decodeBody[rolloutResponse](t, rec)

	if gotContext != "wc1-admin@wc1" || resp.Cluster != "wc1" || resp.Namespace != "d1-ns" {
		t.Fatalf("context = %q, response = %+v", gotContext, resp)
	}
	if len(resp.Pods) != 2 {
		t.Fatalf("pods = %+v", resp.Pods)
	}
	if p := resp.Pods[0]; p.Reason != "ContainerCreating" || p.Ready != "0/1" {
		t.Errorf("server pod = %+v", p)
	}
	if p := resp.Pods[1]; p.Reason != "NotReady" || p.Restarts != 2 {
		t.Errorf("worker pod = %+v", p)
	}
	if len(resp.Events) != 1 || resp.Events[0].Object != "pod/d1-server-abc" || resp.Events[0].Reason != "Pulling" {
		t.Errorf("events = %+v", resp.Events)
	}
}

func TestRolloutWithoutShellIsNotFound(t *testing.T) {
	s, _ := newServer(t, Config{})
	s.clusterClient = func([]byte, string) (kubernetes.Interface, error) { panic("unreachable") }
	requireStatus(t, do(t, s, http.MethodGet, "/api/v1/deployments/d1/rollout", nil), http.StatusNotFound)
}
