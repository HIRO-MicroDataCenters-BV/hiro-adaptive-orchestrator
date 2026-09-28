{{/*
Selector labels of the operator Deployment/pods created by the hiro-operator
subchart (dist/chart, aliased "hiro-operator" — see Chart.yaml). Templates in
THIS chart that need to target those pods (e.g. the webhook Service) use this
instead of hardcoding the subchart's naming convention in multiple places.
*/}}
{{- define "hiro-adaptive-platform.operatorSelectorLabels" -}}
app.kubernetes.io/name: hiro-operator
control-plane: controller-manager
{{- end }}

{{/*
Error message for webhook.enable=true with scheduler.mode != "plugin".
Checked at the top of templates/webhook/manifests.yaml (the one guarded
by webhook.enable), same pattern as the "required" messages in
templates/scheduler-plugin/*.yaml — message text lives here, the actual
if/fail check stays next to the resource it protects.
*/}}
{{- define "hiro-adaptive-platform.errors.webhookRequiresPlugin" -}}
webhook.enable=true requires scheduler.mode: plugin. The webhook sets spec.schedulerName on new pods so they route to the scheduler-plugin Deployment; with mode=none nothing claims that name (pods stick Pending), and with mode=extender the default scheduler already handles every pod via the extender patch, so schedulerName would misroute them instead. This mirrors deploy_full_stack.sh, where the webhook has no independent toggle — it's driven 1:1 by DEPLOY_SCHEDULER_PLUGIN.
{{- end }}
