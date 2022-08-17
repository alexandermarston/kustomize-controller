/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"time"

	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"sigs.k8s.io/cli-utils/pkg/kstatus/polling"
	"sigs.k8s.io/cli-utils/pkg/kstatus/polling/engine"
	ctrl "sigs.k8s.io/controller-runtime"
	crtlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fluxcd/pkg/runtime/acl"
	"github.com/fluxcd/pkg/runtime/client"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/leaderelection"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/fluxcd/pkg/runtime/metrics"
	"github.com/fluxcd/pkg/runtime/pprof"
	"github.com/fluxcd/pkg/runtime/probes"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	"github.com/fluxcd/kustomize-controller/controllers"
	"github.com/fluxcd/kustomize-controller/internal/statusreaders"
	// +kubebuilder:scaffold:imports
)

const controllerName = "kustomize-controller"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = sourcev1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr           string
		eventsAddr            string
		healthAddr            string
		concurrent            int
		requeueDependency     time.Duration
		clientOptions         client.Options
		kubeConfigOpts        client.KubeConfigOptions
		logOptions            logger.Options
		leaderElectionOptions leaderelection.Options
		rateLimiterOptions    helper.RateLimiterOptions
		aclOptions            acl.Options
		watchAllNamespaces    bool
		noRemoteBases         bool
		httpRetry             int
		defaultServiceAccount string
		reorder               string
	)

	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&eventsAddr, "events-addr", "", "The address of the events receiver.")
	flag.StringVar(&healthAddr, "health-addr", ":9440", "The address the health endpoint binds to.")
	flag.IntVar(&concurrent, "concurrent", 4, "The number of concurrent kustomize reconciles.")
	flag.DurationVar(&requeueDependency, "requeue-dependency", 30*time.Second, "The interval at which failing dependencies are reevaluated.")
	flag.BoolVar(&watchAllNamespaces, "watch-all-namespaces", true,
		"Watch for custom resources in all namespaces, if set to false it will only watch the runtime namespace.")
	flag.BoolVar(&noRemoteBases, "no-remote-bases", false,
		"Disallow remote bases usage in Kustomize overlays. When this flag is enabled, all resources must refer to local files included in the source artifact.")
	flag.IntVar(&httpRetry, "http-retry", 9, "The maximum number of retries when failing to fetch artifacts over HTTP.")
	flag.StringVar(&defaultServiceAccount, "default-service-account", "", "Default service account used for impersonation.")
	flag.StringVar(&reorder, "reorder", "legacy",
		"Reorder the resources just before output. "+
			"Use 'legacy' to apply a legacy reordering "+
			"(Namespaces first, Webhooks last, etc). "+
			"Use 'none' to suppress a final reordering.")
	clientOptions.BindFlags(flag.CommandLine)
	logOptions.BindFlags(flag.CommandLine)
	leaderElectionOptions.BindFlags(flag.CommandLine)
	aclOptions.BindFlags(flag.CommandLine)
	kubeConfigOpts.BindFlags(flag.CommandLine)
	rateLimiterOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(logger.NewLogger(logOptions))

	metricsRecorder := metrics.NewRecorder()
	crtlmetrics.Registry.MustRegister(metricsRecorder.Collectors()...)

	watchNamespace := ""
	if !watchAllNamespaces {
		watchNamespace = os.Getenv("RUNTIME_NAMESPACE")
	}

	restConfig := client.GetConfigOrDie(clientOptions)
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		MetricsBindAddress:            metricsAddr,
		HealthProbeBindAddress:        healthAddr,
		Port:                          9443,
		LeaderElection:                leaderElectionOptions.Enable,
		LeaderElectionReleaseOnCancel: leaderElectionOptions.ReleaseOnCancel,
		LeaseDuration:                 &leaderElectionOptions.LeaseDuration,
		RenewDeadline:                 &leaderElectionOptions.RenewDeadline,
		RetryPeriod:                   &leaderElectionOptions.RetryPeriod,
		LeaderElectionID:              fmt.Sprintf("%s-leader-election", controllerName),
		Namespace:                     watchNamespace,
		Logger:                        ctrl.Log,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	probes.SetupChecks(mgr, setupLog)
	pprof.SetupHandlers(mgr, setupLog)

	var eventRecorder *events.Recorder
	if eventRecorder, err = events.NewRecorder(mgr, ctrl.Log, eventsAddr, controllerName); err != nil {
		setupLog.Error(err, "unable to create event recorder")
		os.Exit(1)
	}

	jobStatusReader := statusreaders.NewCustomJobStatusReader(mgr.GetRESTMapper())
	pollingOpts := polling.Options{
		CustomStatusReaders: []engine.StatusReader{jobStatusReader},
	}
	if err = (&controllers.KustomizationReconciler{
		ControllerName:        controllerName,
		DefaultServiceAccount: defaultServiceAccount,
		Reorder:               reorder,
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		EventRecorder:         eventRecorder,
		MetricsRecorder:       metricsRecorder,
		NoCrossNamespaceRefs:  aclOptions.NoCrossNamespaceRefs,
		NoRemoteBases:         noRemoteBases,
		KubeConfigOpts:        kubeConfigOpts,
		PollingOpts:           pollingOpts,
		StatusPoller:          polling.NewStatusPoller(mgr.GetClient(), mgr.GetRESTMapper(), pollingOpts),
	}).SetupWithManager(mgr, controllers.KustomizationReconcilerOptions{
		MaxConcurrentReconciles:   concurrent,
		DependencyRequeueInterval: requeueDependency,
		HTTPRetry:                 httpRetry,
		RateLimiter:               helper.GetRateLimiter(rateLimiterOptions),
	}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", controllerName)
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
