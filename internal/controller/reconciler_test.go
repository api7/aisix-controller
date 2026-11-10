package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/api7/aisix-controller/api/v1alpha1"
	"github.com/api7/aisix-controller/internal/render"
	"github.com/api7/aisix-controller/internal/validate"
)

var (
	testEnv    *envtest.Environment
	testClient client.Client
	testScheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS unset; skipping envtest suites")
		os.Exit(m.Run())
	}
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = v1alpha1.AddToScheme(testScheme)
	testEnv = &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd"}, ErrorIfCRDPathMissing: true}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	testClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

func needEnv(t *testing.T) {
	if testEnv == nil {
		t.Skip("needs envtest (KUBEBUILDER_ASSETS)")
	}
}

// rejectingValidator rejects any entry whose model_name is "reject-me",
// scoped the way the gateway scopes errors.
var rejectingValidator = validate.Func(func(_ context.Context, doc []byte) ([]validate.LoadError, error) {
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		return []validate.LoadError{{Scope: "(file)", Message: err.Error()}}, nil
	}
	var errs []validate.LoadError
	items, _ := root["models"].([]any)
	for i, it := range items {
		if it.(map[string]any)["model_name"] == "reject-me" {
			errs = append(errs, validate.LoadError{Scope: fmt.Sprintf("models[%d]", i), Message: "rejected by the gateway"})
		}
	}
	return errs, nil
})

// fakeStatus serves configured hashes per pod.
type fakeStatus struct {
	mu     sync.Mutex
	hashes map[string]string
}

func (f *fakeStatus) set(pod, hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashes[pod] = hash
}

func (f *fakeStatus) Read(_ context.Context, p *corev1.Pod) (PodStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return PodStatus{ConfigHash: f.hashes[p.Name]}, nil
}

func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	if err := testClient.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

func spec(t *testing.T, v map[string]any) apiextensionsv1.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return apiextensionsv1.JSON{Raw: b}
}

func create(t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := testClient.Create(context.Background(), o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
}

func gatewayDeployment(ns string) *appsv1.Deployment {
	labels := map[string]string{"app": "aisix"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "aisix", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "aisix", Image: "aisix"}}},
			},
		},
	}
}

func readyPod(t *testing.T, ns, name string) {
	t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": "aisix"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "aisix", Image: "aisix"}}},
	}
	create(t, p)
	p.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		PodIP:      "10.0.0.7",
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	if err := testClient.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func newReconciler(ns string, status StatusReader) *Reconciler {
	return &Reconciler{
		Client: testClient, Namespace: ns, Secret: "aisix-resources", SecretKey: "resources.yaml",
		Deployment: "aisix", Validator: rejectingValidator, Status: status,
		Resync: time.Minute, Poll: time.Second,
	}
}

func reconcileOnce(t *testing.T, r *Reconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func served(t *testing.T, ns string) []byte {
	t.Helper()
	var s corev1.Secret
	if err := testClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "aisix-resources"}, &s); err != nil {
		t.Fatal(err)
	}
	return s.Data["resources.yaml"]
}

func conditionOf(t *testing.T, o client.Object, typ string) *metav1.Condition {
	t.Helper()
	if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(o.(v1alpha1.Object).ResourceStatus().Conditions, typ)
	if c == nil {
		t.Fatalf("%s has no %s condition", o.GetName(), typ)
	}
	return c
}

func TestReconcileSeedsAnEmptyResourcesFile(t *testing.T) {
	needEnv(t)
	ns := newNamespace(t)
	reconcileOnce(t, newReconciler(ns, &fakeStatus{hashes: map[string]string{}}))
	if got := served(t, ns); !bytes.Equal(got, render.Document(nil)) {
		t.Errorf("seed = %q", got)
	}
}

func TestReconcileServesResourcesAndReportsStatus(t *testing.T) {
	needEnv(t)
	ns := newNamespace(t)
	status := &fakeStatus{hashes: map[string]string{}}
	r := newReconciler(ns, status)

	create(t,
		gatewayDeployment(ns),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns},
			StringData: map[string]string{"upstream": "sk-$x", "caller": "caller-key"},
		},
		&v1alpha1.ProviderKey{
			ObjectMeta: metav1.ObjectMeta{Name: "mock", Namespace: ns},
			Spec: spec(t, map[string]any{"provider": "openai", "api_base": "http://mock/v1",
				v1alpha1.APIKeySecretRef: map[string]any{"name": "creds", "key": "upstream"}}),
		},
		&v1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: ns},
			Spec: spec(t, map[string]any{"display_name": "e2e/chat", "provider": "openai", "model_name": "a",
				v1alpha1.ProviderKeyRef: map[string]any{"name": "mock"}}),
		},
		&v1alpha1.APIKey{
			ObjectMeta: metav1.ObjectMeta{Name: "caller", Namespace: ns},
			Spec: spec(t, map[string]any{"allowed_models": []any{"*"},
				v1alpha1.KeySecretRef: map[string]any{"name": "creds", "key": "caller"}}),
		},
	)

	// First pass: written and stamped, not yet served.
	res := reconcileOnce(t, r)
	doc := served(t, ns)
	for _, want := range []string{`"api_key": "sk-$$x"`, `"provider_key": "mock"`, `"key_hash": "` + render.HashKey("caller-key")} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("document lacks %s:\n%s", want, doc)
		}
	}
	if strings.Contains(string(doc), "caller-key") {
		t.Error("caller key plaintext is in the document")
	}
	var dep appsv1.Deployment
	if err := testClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "aisix"}, &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Annotations[v1alpha1.HashAnnotation] != render.Hash(doc) {
		t.Errorf("deployment not stamped: %v", dep.Spec.Template.Annotations)
	}
	model := &v1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: ns}}
	if c := conditionOf(t, model, CondAccepted); c.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v", c)
	}
	if c := conditionOf(t, model, CondProgrammed); c.Status != metav1.ConditionFalse || c.Reason != "Pending" {
		t.Errorf("Programmed = %+v", c)
	}
	if res.RequeueAfter != r.Poll {
		t.Errorf("pending change should poll, got %v", res.RequeueAfter)
	}

	// A ready pod that serves an older file keeps it pending.
	readyPod(t, ns, "aisix-1")
	status.set("aisix-1", "old")
	reconcileOnce(t, r)
	if c := conditionOf(t, model, CondProgrammed); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "0/1") {
		t.Errorf("Programmed = %+v", c)
	}

	// Once it serves the written file, everything is programmed.
	status.set("aisix-1", render.Hash(doc))
	res = reconcileOnce(t, r)
	for _, o := range []client.Object{
		&v1alpha1.ProviderKey{ObjectMeta: metav1.ObjectMeta{Name: "mock", Namespace: ns}},
		model,
		&v1alpha1.APIKey{ObjectMeta: metav1.ObjectMeta{Name: "caller", Namespace: ns}},
	} {
		if c := conditionOf(t, o, CondProgrammed); c.Status != metav1.ConditionTrue {
			t.Errorf("%s Programmed = %+v", o.GetName(), c)
		}
	}
	if res.RequeueAfter != r.Resync {
		t.Errorf("settled state should resync, got %v", res.RequeueAfter)
	}

	// A change the gateway rejects keeps the served version.
	if err := testClient.Get(context.Background(), client.ObjectKeyFromObject(model), model); err != nil {
		t.Fatal(err)
	}
	model.Spec = spec(t, map[string]any{"display_name": "e2e/chat", "provider": "openai", "model_name": "reject-me",
		v1alpha1.ProviderKeyRef: map[string]any{"name": "mock"}})
	if err := testClient.Update(context.Background(), model); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r)
	if !bytes.Equal(served(t, ns), doc) {
		t.Error("a rejected change altered the served document")
	}
	c := conditionOf(t, model, CondAccepted)
	if c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "rejected by the gateway") || c.ObservedGeneration != model.Generation {
		t.Errorf("Accepted = %+v (generation %d)", c, model.Generation)
	}
	if c := conditionOf(t, model, CondProgrammed); c.Message != "the previously accepted version keeps serving" {
		t.Errorf("Programmed = %+v", c)
	}

	// Deleting a resource removes it from the file.
	if err := testClient.Delete(context.Background(), &v1alpha1.APIKey{ObjectMeta: metav1.ObjectMeta{Name: "caller", Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r)
	if strings.Contains(string(served(t, ns)), "api_keys") {
		t.Error("deleted APIKey is still served")
	}
}

func TestSelfReferenceDoesNotFeedBack(t *testing.T) {
	needEnv(t)
	ns := newNamespace(t)
	r := newReconciler(ns, &fakeStatus{hashes: map[string]string{}})
	reconcileOnce(t, r)

	loop := &v1alpha1.APIKey{
		ObjectMeta: metav1.ObjectMeta{Name: "loop", Namespace: ns},
		Spec: spec(t, map[string]any{"allowed_models": []any{"*"},
			v1alpha1.KeySecretRef: map[string]any{"name": "aisix-resources", "key": "resources.yaml"}}),
	}
	create(t, loop)
	reconcileOnce(t, r)
	var before corev1.Secret
	if err := testClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "aisix-resources"}, &before); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r)
	reconcileOnce(t, r)
	var after corev1.Secret
	if err := testClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "aisix-resources"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("the resources Secret keeps changing")
	}
	if !bytes.Equal(after.Data["resources.yaml"], render.Document(nil)) {
		t.Errorf("served = %s", after.Data["resources.yaml"])
	}
	if c := conditionOf(t, loop, CondAccepted); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "holds the generated resources file") {
		t.Errorf("Accepted = %+v", c)
	}
}

func TestManagerReconcilesOnStartAndOnChange(t *testing.T) {
	needEnv(t)
	ns := newNamespace(t)
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newReconciler(ns, &fakeStatus{hashes: map[string]string{}})
	r.Client = mgr.GetClient()
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = mgr.Start(ctx) }()

	waitFor := func(what string, ok func(doc []byte) bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var s corev1.Secret
			if err := testClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "aisix-resources"}, &s); err == nil && ok(s.Data["resources.yaml"]) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the seeded file", func(doc []byte) bool { return len(doc) > 0 })

	create(t, &v1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "routed", Namespace: ns},
		Spec:       spec(t, map[string]any{"routing": map[string]any{"targets": []any{map[string]any{"model": "x"}}}}),
	})
	waitFor("the new Model", func(doc []byte) bool { return strings.Contains(string(doc), `"routed"`) })
}
