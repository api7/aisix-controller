// Command aisix-controller reconciles AISIX custom resources into a
// standalone gateway's resources file.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/api7/aisix-controller/api/v1alpha1"
	"github.com/api7/aisix-controller/internal/controller"
	"github.com/api7/aisix-controller/internal/validate"
)

func main() {
	var (
		namespace   = flag.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace of the gateway and its custom resources (default $POD_NAMESPACE)")
		secret      = flag.String("secret", "aisix-resources", "Secret the resources file is written to")
		secretKey   = flag.String("secret-key", "resources.yaml", "key of the resources file in the Secret")
		deployment  = flag.String("deployment", "aisix", "gateway Deployment")
		statusPort  = flag.Int("gateway-status-port", 9090, "gateway pods' metrics listener port, which serves /status/config")
		aisixBinary = flag.String("aisix-binary", "/usr/local/bin/aisix", "aisix binary used to validate resources files")
		tempDir     = flag.String("temp-dir", os.TempDir(), "directory for files being validated")
		resync      = flag.Duration("resync", time.Minute, "full re-check interval")
		poll        = flag.Duration("poll", 3*time.Second, "gateway status poll interval while a change is pending")
		metricsAddr = flag.String("metrics-bind-address", ":8080", "metrics endpoint address; 0 disables it")
		probeAddr   = flag.String("health-probe-bind-address", ":8081", "health probe address")
		leaderElect = flag.Bool("leader-elect", true, "elect a leader so only one replica writes")
	)
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	if err := run(*namespace, *secret, *secretKey, *deployment, *statusPort, *aisixBinary, *tempDir,
		*resync, *poll, *metricsAddr, *probeAddr, *leaderElect); err != nil {
		setupLog.Error(err, "controller stopped")
		os.Exit(1)
	}
}

func run(namespace, secret, secretKey, deployment string, statusPort int, aisixBinary, tempDir string,
	resync, poll time.Duration, metricsAddr, probeAddr string, leaderElect bool) error {
	if namespace == "" {
		return fmt.Errorf("--namespace or $POD_NAMESPACE is required")
	}
	validator := &validate.Binary{Path: aisixBinary, TempDir: tempDir}
	if err := validator.Check(); err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	ns := map[string]cache.Config{namespace: {}}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "aisix-controller.aisix.api7.ai",
		LeaderElectionNamespace: namespace,
		// Only the gateway's namespace is watched.
		Cache: cache.Options{DefaultNamespaces: ns},
	})
	if err != nil {
		return err
	}

	r := &controller.Reconciler{
		Client:     mgr.GetClient(),
		Namespace:  namespace,
		Secret:     secret,
		SecretKey:  secretKey,
		Deployment: deployment,
		Validator:  validator,
		Status:     &controller.HTTPStatusReader{Port: statusPort},
		Resync:     resync,
		Poll:       poll,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting", "namespace", namespace, "secret", secret, "deployment", deployment)
	return mgr.Start(ctrl.SetupSignalHandler())
}
