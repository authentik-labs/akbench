package api

// GET /api/v1/deployments/{name}/rollout: the deployment's pods and its
// namespace's events, read live from the cluster it runs on - what is
// actually happening while the Ready condition only says the HelmRelease is
// still installing (image pulls, scheduling, migrations, crash loops).
// Reached through the shell's merged kubeconfig, same as sql.go, so it
// grants nothing the shell doesn't already.

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

const (
	rolloutTimeout   = 15 * time.Second
	rolloutMaxEvents = 30
)

// RolloutPod is one pod in a deployment's namespace and why it isn't ready
// yet, if it isn't.
type RolloutPod struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    string `json:"ready"`
	Restarts int32  `json:"restarts"`
	Node     string `json:"node,omitempty"`
	Created  string `json:"created,omitempty"`
	// Reason/Message explain a pod that isn't ready: a waiting or failed
	// container, an unschedulable pod, a failing readiness probe.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// RolloutEvent is an Event in the deployment's namespace, naming the object
// it is about.
type RolloutEvent struct {
	ObjectEvent
	Object string `json:"object"`
}

type rolloutResponse struct {
	Cluster   string         `json:"cluster"`
	Namespace string         `json:"namespace"`
	Pods      []RolloutPod   `json:"pods"`
	Events    []RolloutEvent `json:"events"`
}

func (s *Server) getRollout(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Shell == nil {
		writeError(w, r, http.StatusNotFound, "rollout details need the shell kubeconfig, which is not configured on this backend")
		return
	}
	var topo benchv1alpha1.AuthentikDeployment
	key := types.NamespacedName{Name: chi.URLParam(r, "name"), Namespace: s.cfg.Namespace}
	if err := s.client.Get(r.Context(), key, &topo); err != nil {
		writeAPIError(w, r, err)
		return
	}
	raw, kubeContext, ok := s.deploymentKubeContext(w, r, &topo)
	if !ok {
		return
	}
	cs, err := s.clusterClient(raw, kubeContext)
	if err != nil {
		writeError(w, r, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), rolloutTimeout)
	defer cancel()
	ns := topo.Spec.Namespace
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, fmt.Sprintf("list pods: %v", err))
		return
	}
	events, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		writeError(w, r, http.StatusBadGateway, fmt.Sprintf("list events: %v", err))
		return
	}
	cluster := topo.Spec.ClusterRef.Name
	if cluster == "" {
		cluster = ManagementClusterName
	}
	writeJSON(w, r, http.StatusOK, rolloutResponse{
		Cluster:   cluster,
		Namespace: ns,
		Pods:      rolloutPods(pods.Items),
		Events:    rolloutEvents(events.Items),
	})
}

func rolloutPods(items []corev1.Pod) []RolloutPod {
	pods := make([]RolloutPod, 0, len(items))
	for i := range items {
		p := &items[i]
		ready := 0
		var restarts int32
		for _, c := range p.Status.ContainerStatuses {
			if c.Ready {
				ready++
			}
			restarts += c.RestartCount
		}
		reason, message := podNotReadyReason(p)
		pods = append(pods, RolloutPod{
			Name:     p.Name,
			Phase:    string(p.Status.Phase),
			Ready:    fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)),
			Restarts: restarts,
			Node:     p.Spec.NodeName,
			Created:  formatEventTime(p.CreationTimestamp.Time),
			Reason:   reason,
			Message:  message,
		})
	}
	slices.SortFunc(pods, func(a, b RolloutPod) int { return strings.Compare(a.Name, b.Name) })
	return pods
}

// podNotReadyReason is the most specific explanation for a pod not being
// ready, in roughly the order kubectl's STATUS column picks one; empty for a
// ready or completed pod.
func podNotReadyReason(p *corev1.Pod) (string, string) {
	if p.DeletionTimestamp != nil {
		return "Terminating", ""
	}
	if p.Status.Phase == corev1.PodSucceeded {
		return "", ""
	}
	for _, c := range p.Status.InitContainerStatuses {
		if c.State.Waiting != nil && c.State.Waiting.Reason != "PodInitializing" {
			return "Init:" + c.State.Waiting.Reason, c.State.Waiting.Message
		}
		if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
			return "Init:" + t.Reason, t.Message
		}
		if c.State.Running != nil {
			return "Init:Running", c.Name
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil {
			return w.Reason, w.Message
		}
		if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
			return t.Reason, t.Message
		}
	}
	for _, cond := range p.Status.Conditions {
		if cond.Status != corev1.ConditionFalse {
			continue
		}
		switch cond.Type {
		case corev1.PodScheduled:
			return cmp.Or(cond.Reason, "Unschedulable"), cond.Message
		case corev1.ContainersReady:
			return "NotReady", cmp.Or(cond.Message, "readiness probe not passing yet")
		}
	}
	if p.Status.Phase == corev1.PodPending {
		return "Pending", p.Status.Message
	}
	return "", ""
}

func rolloutEvents(items []corev1.Event) []RolloutEvent {
	type sortable struct {
		event RolloutEvent
		last  time.Time
	}
	sorted := make([]sortable, 0, len(items))
	for i := range items {
		ev := &items[i]
		first, last := eventTimes(ev)
		sorted = append(sorted, sortable{
			event: RolloutEvent{
				ObjectEvent: ObjectEvent{
					Type:      ev.Type,
					Reason:    ev.Reason,
					Message:   ev.Message,
					Count:     max(ev.Count, 1),
					FirstSeen: formatEventTime(first),
					LastSeen:  formatEventTime(last),
					Source:    eventSource(ev),
				},
				Object: strings.ToLower(ev.InvolvedObject.Kind) + "/" + ev.InvolvedObject.Name,
			},
			last: last,
		})
	}
	slices.SortStableFunc(sorted, func(a, b sortable) int { return b.last.Compare(a.last) })
	events := make([]RolloutEvent, 0, min(len(sorted), rolloutMaxEvents))
	for _, s := range sorted[:min(len(sorted), rolloutMaxEvents)] {
		events = append(events, s.event)
	}
	return events
}
