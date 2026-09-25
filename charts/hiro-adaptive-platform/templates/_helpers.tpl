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
