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

// cmd is the entry point for the HIRO kube-scheduler binary.
//
// Build from the scheduler-plugin directory:
//
//	cd scheduler-plugin && go build -o ../bin/hiro-scheduler ./cmd/
//
// Or use the Makefile target:
//
//	make build-scheduler
//
// The binary is a standard kube-scheduler with one additional plugin registered:
// HIROScore (FilterPlugin + PreScorePlugin + ScorePlugin). All default in-tree
// plugins remain available. Only pods with schedulerName=hiro-scheduler are routed
// here; the default scheduler handles everything else.
package main

import (
	"context"
	"os"
	"time"

	"k8s.io/component-base/cli"
	"k8s.io/klog/v2"
	app "k8s.io/kubernetes/cmd/kube-scheduler/app"

	schedulerplugin "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/scheduler-plugin"
)

// k8sVersion is injected at build time:
//
//	-ldflags="-X main.k8sVersion=v1.35.0"
var k8sVersion = "unknown"

func main() {
	// kube-scheduler has no tracing config of its own, so the plugin owns the
	// TracerProvider lifecycle. No-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
	shutdownTracing, err := schedulerplugin.InitTracing(context.Background(), k8sVersion)
	if err != nil {
		klog.ErrorS(err, "initializing tracing; continuing without it")
	}

	command := app.NewSchedulerCommand(
		app.WithPlugin(schedulerplugin.PluginName, schedulerplugin.New),
	)
	code := cli.Run(command)

	if shutdownTracing != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			klog.ErrorS(err, "flushing traces on shutdown")
		}
	}
	os.Exit(code)
}
