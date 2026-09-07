/*
Copyright 2026 HIRO Adaptive Orchestrator.

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
	"crypto/tls"
	"flag"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/controller"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/rebalance"
	webhookv1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/webhook/v1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(orchestrationv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// parseDurationEnv returns 0 (letting the caller apply its own default) when
// name is unset, or the parsed duration otherwise. Exits the process on an
// invalid value — a misconfigured duration should fail fast at startup, not
// silently fall back to a default the deployer didn't ask for.
func parseDurationEnv(name string) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		setupLog.Error(err, "invalid duration environment variable", "name", name, "value", v)
		os.Exit(1)
	}
	return d
}

// parseFloatEnv is parseDurationEnv's float64 counterpart.
func parseFloatEnv(name string) float64 {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		setupLog.Error(err, "invalid float environment variable", "name", name, "value", v)
		os.Exit(1)
	}
	return f
}

// parseIntEnv is parseDurationEnv's int counterpart.
func parseIntEnv(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		setupLog.Error(err, "invalid integer environment variable", "name", name, "value", v)
		os.Exit(1)
	}
	return n
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	restConfig := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "97efdfc7.orchestration.hiro.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// -------------------------------------------------------------------------
	// Register ProfileByAppRefIndex BEFORE SetupWithManager and mgr.Start().
	//
	// This field index enables O(1) profile lookups in:
	//   - internal/controller/op_watchers.go  (pod/workload → profile mapping)
	//   - internal/decision/builder.go        (pod → governing profile lookup)
	//
	// IMPORTANT: must be registered before the cache starts (before mgr.Start).
	// Registering after cache start will panic.
	// -------------------------------------------------------------------------
	if err := controller.RegisterProfileIndexes(mgr); err != nil {
		setupLog.Error(err, "unable to register profile indexes")
		os.Exit(1)
	}

	if err := (&controller.OrchestrationProfileReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("orchestrationprofile-controller"), //nolint:staticcheck
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "OrchestrationProfile")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// -------------------------------------------------------------------------
	// Decision Layer — PlacementServer
	//
	// HTTP server that receives PlacementContext from the kube-scheduler
	// custom scoring plugin and returns NodeScores.
	//
	// Environment variables:
	//   DECISION_AGENT_URL      — base URL of the External AI Agent (required)
	//                             e.g. "http://ai-agent.hiro-system.svc:8080"
	//   DECISION_AGENT_PATH     — HTTP path on the AI agent (optional)
	//                             default: "/api/v1/agent/placement/decision"
	//   PLACEMENT_SERVER_PORT   — listening address (optional, default ":8090")
	//   PLACEMENT_SCORE_PATH         — HTTP path for AI scoring (optional)
	//                                  default: "/api/v1/placement/score"
	//   PLACEMENT_FILTER_PATH        — HTTP path for energy gate filter (optional)
	//                                  default: "/api/v1/placement/filter"
	//   PLACEMENT_SERVER_HEALTH_PATH — HTTP path for health probes (optional)
	//                                  default: "/healthz"
	//   EAO_GROUP               — API group of EnergyAwareOrchestration CRD
	//                             (optional, default "eas.hiro.io")
	//   EAO_VERSION             — API version of EnergyAwareOrchestration CRD
	//                             (optional, default "v1")
	//   EAO_KIND                — Kind of EnergyAwareOrchestration CRD
	//                             (optional, default "EnergyAwareOrchestration")
	// -------------------------------------------------------------------------
	decisionAgentURL := os.Getenv("DECISION_AGENT_URL")
	if decisionAgentURL == "" {
		setupLog.Error(nil, "DECISION_AGENT_URL environment variable is required")
		os.Exit(1)
	}

	decisionAgentPath := os.Getenv("DECISION_AGENT_PATH")
	if decisionAgentPath == "" {
		decisionAgentPath = "/api/v1/agent/placement/decision"
	}

	placementServerPort := os.Getenv("PLACEMENT_SERVER_PORT")
	if placementServerPort == "" {
		placementServerPort = ":8090"
	}

	placementScorePath := os.Getenv("PLACEMENT_SCORE_PATH")
	if placementScorePath == "" {
		placementScorePath = "/api/v1/placement/score"
	}

	placementFilterPath := os.Getenv("PLACEMENT_FILTER_PATH")
	if placementFilterPath == "" {
		placementFilterPath = "/api/v1/placement/filter"
	}

	placementServerHealthPath := os.Getenv("PLACEMENT_SERVER_HEALTH_PATH")
	if placementServerHealthPath == "" {
		placementServerHealthPath = "/healthz"
	}

	extenderFilterPath := os.Getenv("EXTENDER_FILTER_PATH")
	if extenderFilterPath == "" {
		extenderFilterPath = "/extender/filter"
	}

	extenderPrioritizePath := os.Getenv("EXTENDER_PRIORITIZE_PATH")
	if extenderPrioritizePath == "" {
		extenderPrioritizePath = "/extender/prioritize"
	}

	eaoGroup := os.Getenv("EAO_GROUP")
	if eaoGroup == "" {
		eaoGroup = "eas.hiro.io"
	}
	eaoVersion := os.Getenv("EAO_VERSION")
	if eaoVersion == "" {
		eaoVersion = "v1"
	}
	eaoKind := os.Getenv("EAO_KIND")
	if eaoKind == "" {
		eaoKind = "EnergyAwareOrchestration"
	}
	eaoGVK := schema.GroupVersionKind{
		Group:   eaoGroup,
		Version: eaoVersion,
		Kind:    eaoKind + "List",
	}
	setupLog.Info("decision layer configured",
		"agentURL", decisionAgentURL,
		"agentPath", decisionAgentPath,
		"placementScorePath", placementScorePath,
		"placementFilterPath", placementFilterPath,
		"placementHealthPath", placementServerHealthPath,
		"extenderFilterPath", extenderFilterPath,
		"extenderPrioritizePath", extenderPrioritizePath,
		"eaoGroup", eaoGVK.Group,
		"eaoVersion", eaoGVK.Version,
		"eaoKind", eaoGVK.Kind,
	)

	enableWebhooks := os.Getenv("ENABLE_WEBHOOKS") != "false"
	hiroSchedulerName := os.Getenv("HIRO_SCHEDULER_NAME")
	if hiroSchedulerName == "" {
		hiroSchedulerName = webhookv1.DefaultSchedulerName
	}
	setupLog.Info("webhook configured",
		"enableWebhooks", enableWebhooks,
		"schedulerName", hiroSchedulerName,
	)

	contextBuilder := placementserver.NewDecisionContextBuilder(
		mgr.GetClient(),
		controller.ProfileByAppRefIndex,
		eaoGVK,
	)

	// Register the pod scheduler MutatingAdmissionWebhook.
	// Sets spec.schedulerName automatically on pods governed by an OrchestrationProfile,
	// removing the need for users to set it manually in their pod specs.
	// Controlled by ENABLE_WEBHOOKS (default "false" in manager.yaml;
	// set to "true" by hack/deploy_webhook.sh when deploying with the scheduler plugin).
	if enableWebhooks {
		if err := webhookv1.SetupPodWebhookWithManager(mgr, contextBuilder, hiroSchedulerName); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "Pod")
			os.Exit(1)
		}
	}

	// Create the DecisionClient with the External AI Agent URL and path.
	// The client will be used by the PlacementServer to send placement decision requests to the AI agent.
	decisionClient := placementserver.NewDecisionClient(
		decisionAgentURL,
		decisionAgentPath,
		8*time.Second, // must be < PlacementServer requestTimeout (10s)
	)

	// -------------------------------------------------------------------------
	// Rebalance Engine
	//
	// Runtime loop that acts on the AI's guidance after initial placement:
	// Watching -> Triggered -> Evaluating -> Decided -> Enacting, always
	// returning to Watching with the cycle's outcome (Enacted/NoOp/Rejected/
	// Deferred/Failed) recorded on recentDecisions. Registered as a
	// manager.Runnable so its lifecycle matches every other component here.
	//
	// StateWriter is the only component permitted to mutate
	// status.rebalancingStatus — see internal/rebalance/writer.go.
	//
	// Environment variables (all optional; unset/0 uses the package default):
	//   REBALANCE_MAX_RECENT_DECISIONS     — decision-history length kept per profile
	//   REBALANCE_DETECTION_INTERVAL       — periodic detection tick, e.g. "30s"
	//   REBALANCE_DECISION_TIMEOUT         — AI consultation timeout, e.g. "5s"
	//   REBALANCE_NODE_PRESSURE_THRESHOLD  — CPU/Memory pressure fraction, e.g. "0.90"
	//   REBALANCE_IMPROVEMENT_THRESHOLD    — minimum Move improvement score to enact, e.g. "20"
	//   REBALANCE_DECISION_STORE_TTL       — how long a Move decision biases scoring, e.g. "60s"
	//   REBALANCE_MOVE_ACTION_TIMEOUT      — max wait for a Move's replacement pod, e.g. "60s"
	//   REBALANCE_MOVE_RATE_LIMIT          — cluster-wide Moves/minute across every profile, e.g. "5"
	//   REBALANCE_SCALE_ACTION_TIMEOUT     — max wait for an AdjustReplicas rollout, e.g. "60s"
	//   REBALANCE_MIN_REPLICAS             — fallback min replicas when no HPA exists, e.g. "1"
	//   REBALANCE_MAX_REPLICAS             — fallback max replicas when no HPA exists, e.g. "10"
	// -------------------------------------------------------------------------
	rebalanceMaxRecentDecisions := parseIntEnv("REBALANCE_MAX_RECENT_DECISIONS")
	if rebalanceMaxRecentDecisions <= 0 {
		rebalanceMaxRecentDecisions = rebalance.DefaultMaxRecentDecisions
	}
	rebalanceDetectionInterval := parseDurationEnv("REBALANCE_DETECTION_INTERVAL")
	if rebalanceDetectionInterval <= 0 {
		rebalanceDetectionInterval = rebalance.DefaultDetectionInterval
	}
	rebalanceDecisionTimeout := parseDurationEnv("REBALANCE_DECISION_TIMEOUT")
	if rebalanceDecisionTimeout <= 0 {
		rebalanceDecisionTimeout = rebalance.DefaultDecisionTimeout
	}
	rebalanceNodePressureThreshold := parseFloatEnv("REBALANCE_NODE_PRESSURE_THRESHOLD")
	if rebalanceNodePressureThreshold <= 0 {
		rebalanceNodePressureThreshold = rebalance.DefaultNodePressureThreshold
	}
	rebalanceImprovementThreshold := parseFloatEnv("REBALANCE_IMPROVEMENT_THRESHOLD")
	if rebalanceImprovementThreshold <= 0 {
		rebalanceImprovementThreshold = rebalance.DefaultImprovementThreshold
	}
	rebalanceDecisionStoreTTL := parseDurationEnv("REBALANCE_DECISION_STORE_TTL")
	if rebalanceDecisionStoreTTL <= 0 {
		rebalanceDecisionStoreTTL = placementserver.DefaultDecisionStoreTTL
	}
	rebalanceMoveActionTimeout := parseDurationEnv("REBALANCE_MOVE_ACTION_TIMEOUT")
	if rebalanceMoveActionTimeout <= 0 {
		rebalanceMoveActionTimeout = rebalance.DefaultMoveActionTimeout
	}
	rebalanceMoveRateLimit := parseIntEnv("REBALANCE_MOVE_RATE_LIMIT")
	if rebalanceMoveRateLimit <= 0 {
		rebalanceMoveRateLimit = rebalance.DefaultMoveRateLimit
	}
	rebalanceScaleActionTimeout := parseDurationEnv("REBALANCE_SCALE_ACTION_TIMEOUT")
	if rebalanceScaleActionTimeout <= 0 {
		rebalanceScaleActionTimeout = rebalance.DefaultScaleActionTimeout
	}
	rebalanceMinReplicas := int32(parseIntEnv("REBALANCE_MIN_REPLICAS"))
	if rebalanceMinReplicas <= 0 {
		rebalanceMinReplicas = rebalance.DefaultMinReplicas
	}
	rebalanceMaxReplicas := int32(parseIntEnv("REBALANCE_MAX_REPLICAS"))
	if rebalanceMaxReplicas <= 0 {
		rebalanceMaxReplicas = rebalance.DefaultMaxReplicas
	}
	// Resolved above (not left at the parseXEnv zero-sentinel) so this log
	// line — and everything downstream — reflects what's actually in
	// effect, not "0" for anything the deployer left unset.
	setupLog.Info("rebalance engine configured",
		"maxRecentDecisions", rebalanceMaxRecentDecisions,
		"detectionInterval", rebalanceDetectionInterval,
		"decisionTimeout", rebalanceDecisionTimeout,
		"nodePressureThreshold", rebalanceNodePressureThreshold,
		"improvementThreshold", rebalanceImprovementThreshold,
		"decisionStoreTTL", rebalanceDecisionStoreTTL,
		"moveActionTimeout", rebalanceMoveActionTimeout,
		"moveRateLimit", rebalanceMoveRateLimit,
		"scaleActionTimeout", rebalanceScaleActionTimeout,
		"minReplicas", rebalanceMinReplicas,
		"maxReplicas", rebalanceMaxReplicas,
	)

	// decisionStore is shared between the PlacementServer (reads it in
	// score, in-process) and the rebalance engine's Move enactor (writes to
	// it before eviction) — both live in this same operator binary.
	decisionStore := placementserver.NewDecisionStore(rebalanceDecisionStoreTTL)

	rebalanceWriter := rebalance.NewStateWriter(
		mgr.GetClient(),
		mgr.GetAPIReader(), // uncached — see NewStateWriter's doc comment on why
		mgr.GetEventRecorderFor("rebalance-engine"), //nolint:staticcheck
		rebalanceMaxRecentDecisions,
	)
	rebalanceEngine := rebalance.NewEngine(mgr.GetClient(), rebalanceWriter)
	if err := mgr.Add(rebalanceEngine); err != nil {
		setupLog.Error(err, "unable to register rebalance engine")
		os.Exit(1)
	}

	// metricsClient talks to metrics-server (metrics.k8s.io) for the
	// CPUThreshold/MemoryThreshold trigger conditions. metrics-server is an
	// optional cluster component — NodePressureEvaluator soft-fails per node
	// when it's unavailable, so this client is safe to construct unconditionally.
	metricsClient, err := metricsclientset.NewForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to create metrics-server client")
		os.Exit(1)
	}
	pressureEvaluator := rebalance.NewNodePressureEvaluator(mgr.GetClient(), metricsClient, rebalanceNodePressureThreshold)
	triggerEvaluator := rebalance.NewTriggerEvaluator(mgr.GetClient(), eaoGVK, pressureEvaluator)

	rebalanceDetector := rebalance.NewReconciler(
		mgr.GetClient(),
		rebalanceWriter,
		triggerEvaluator,
		controller.ProfileByAppRefIndex,
		rebalanceDetectionInterval,
		contextBuilder,
		decisionClient,
		rebalanceDecisionTimeout,
		rebalanceImprovementThreshold,
		decisionStore,
		rebalanceMoveActionTimeout,
		rebalanceMoveRateLimit,
		rebalanceScaleActionTimeout,
		rebalanceMinReplicas,
		rebalanceMaxReplicas,
	)
	// eaoGVK above is the List kind (used for List() calls); Watches()/
	// RESTMapper need the singular item kind, derived here rather than
	// carrying a second GVK variable through main.go.
	eaoItemGVK := eaoGVK
	eaoItemGVK.Kind = strings.TrimSuffix(eaoGVK.Kind, "List")
	if err := rebalanceDetector.SetupWithManager(mgr, eaoItemGVK); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RebalanceDetection")
		os.Exit(1)
	}

	// Create the PlacementServer with the context builder and decision client.
	// The server will use these to handle incoming placement decision requests from the kube-scheduler plugin.
	placementServer := placementserver.NewPlacementServer(
		contextBuilder,
		decisionClient,
		decisionStore,
		placementServerPort,
		placementScorePath,
		placementFilterPath,
		placementServerHealthPath,
		extenderFilterPath,
		extenderPrioritizePath,
		10*time.Second, // requestTimeout: must be > DecisionClient timeout
	)

	// Start PlacementServer alongside the manager.
	// Shuts down gracefully when the manager context is cancelled.
	ctx := ctrl.SetupSignalHandler()
	go func() {
		setupLog.Info("starting placement decision server", "addr", placementServer.Addr)
		if err := placementServer.Start(ctx); err != nil {
			setupLog.Error(err, "placement decision server stopped unexpectedly")
			os.Exit(1)
		}
	}()

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	// -------------------------------------------------------------------------
	// Start the manager — blocks until context cancelled.
	// Starts: informer cache, reconciler, leader election, health probes.
	// -------------------------------------------------------------------------
	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
