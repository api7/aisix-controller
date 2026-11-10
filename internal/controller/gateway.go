package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/api7/aisix-controller/api/v1alpha1"
)

// PodStatus is what one gateway pod reports on /status/config.
type PodStatus struct {
	// ConfigHash is applied.config_hash: in file mode, SHA-256 of the raw
	// resources file the pod serves.
	ConfigHash string
	// LastFailure is last_failure.last_error, when a load failed.
	LastFailure string
}

// StatusReader reads a gateway pod's configuration status.
type StatusReader interface {
	Read(ctx context.Context, pod *corev1.Pod) (PodStatus, error)
}

// HTTPStatusReader reads GET /status/config on the pod's metrics listener,
// which serves it without auth (aisix crates/aisix-admin metrics_router).
type HTTPStatusReader struct {
	Port   int
	Client *http.Client
}

type statusConfig struct {
	Applied *struct {
		ConfigHash string `json:"config_hash"`
	} `json:"applied"`
	LastFailure *struct {
		LastError string `json:"last_error"`
	} `json:"last_failure"`
}

func (r *HTTPStatusReader) Read(ctx context.Context, pod *corev1.Pod) (PodStatus, error) {
	if pod.Status.PodIP == "" {
		return PodStatus{}, fmt.Errorf("pod %s has no IP", pod.Name)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	url := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(r.Port)) + "/status/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PodStatus{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return PodStatus{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return PodStatus{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return PodStatus{}, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	var s statusConfig
	if err := json.Unmarshal(body, &s); err != nil {
		return PodStatus{}, fmt.Errorf("%s: %w", url, err)
	}
	var out PodStatus
	if s.Applied != nil {
		out.ConfigHash = strings.ToLower(s.Applied.ConfigHash)
	}
	if s.LastFailure != nil {
		out.LastFailure = s.LastFailure.LastError
	}
	return out, nil
}

// Gateway is whether the gateway serves the wanted document.
type Gateway struct {
	Ready   bool
	Reason  string
	Message string
}

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// readyPods are the pods that take traffic.
func readyPods(pods []corev1.Pod) []*corev1.Pod {
	var out []*corev1.Pod
	for i := range pods {
		if podReady(&pods[i]) {
			out = append(out, &pods[i])
		}
	}
	return out
}

// evaluate decides whether every pod that takes traffic serves the wanted
// hash, and enough of them do to make up the Deployment.
func evaluate(dep *appsv1.Deployment, ready []*corev1.Pod, statuses map[string]PodStatus, errs map[string]error, want string) Gateway {
	if dep == nil {
		return Gateway{Reason: "GatewayNotFound", Message: "the gateway Deployment does not exist"}
	}
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	if replicas == 0 {
		return Gateway{Reason: "Pending", Message: "the gateway Deployment is scaled to zero"}
	}
	serving := 0
	var notes []string
	for _, p := range ready {
		if err, ok := errs[p.Name]; ok {
			notes = append(notes, fmt.Sprintf("pod %s: %v", p.Name, err))
			continue
		}
		s := statuses[p.Name]
		if s.ConfigHash == want {
			serving++
			continue
		}
		if s.LastFailure != "" {
			notes = append(notes, fmt.Sprintf("pod %s: %s", p.Name, s.LastFailure))
		}
	}
	if serving == len(ready) && int32(serving) >= replicas {
		return Gateway{Ready: true, Reason: "Programmed", Message: fmt.Sprintf("served by %d gateway pod(s)", serving)}
	}
	msg := fmt.Sprintf("%d/%d gateway pods serve this configuration", serving, replicas)
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	return Gateway{Reason: "Pending", Message: msg}
}

func stamped(dep *appsv1.Deployment, hash string) bool {
	return dep.Spec.Template.Annotations[v1alpha1.HashAnnotation] == hash
}
