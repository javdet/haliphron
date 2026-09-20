#!/bin/sh
# Copy the generated artifacts into the haliphron-runtime chart.
#
# Two files, both derived and neither edited by hand:
#
#   config/crd/bases/haliphron.io_agentruns.yaml -> templates/crd.yaml
#   config/rbac/role.yaml                        -> templates/rbac-controller.yaml
#
# The CRD goes into templates/ rather than crds/ on purpose. Helm installs what
# is in crds/ once and ignores it afterwards, so the first additive schema
# change would fail to reach the cluster and surface as pruned fields in spec —
# runs with a silently lost setting. In templates/ with
# helm.sh/resource-policy: keep it upgrades with the release and survives an
# uninstall.
#
# Run after `make generate`; `make verify` fails if either copy is stale.
set -eu

chart=deploy/charts/haliphron-runtime/templates

# --- the CRD -----------------------------------------------------------------
{
  cat <<'HEAD'
{{/*
GENERATED from config/crd/bases/haliphron.io_agentruns.yaml by hack/chartgen.sh.
Do not edit: change the Go types in api/agentrun/v1alpha1, run `make generate`,
then `make chart-gen`.
*/}}
{{- if .Values.crd.install }}
HEAD

  sed \
    -e 's|^    controller-gen.kubebuilder.io/version: \(.*\)$|    controller-gen.kubebuilder.io/version: \1\n    helm.sh/resource-policy: keep\n    {{- if .Values.crd.conversionWebhook.enabled }}\n    cert-manager.io/inject-ca-from: {{ .Release.Namespace }}/{{ include "haliphron-runtime.fullname" . }}-serving-cert\n    {{- end }}|' \
    config/crd/bases/haliphron.io_agentruns.yaml

  cat <<'TAIL'
  {{- if .Values.crd.conversionWebhook.enabled }}
  conversion:
    strategy: Webhook
    webhook:
      conversionReviewVersions: ["v1"]
      clientConfig:
        service:
          namespace: {{ .Release.Namespace }}
          name: {{ include "haliphron-runtime.fullname" . }}-webhook
          path: /convert
          port: 443
  {{- end }}
{{- end }}
TAIL
} > "$chart/crd.yaml"

# --- the controller's rules --------------------------------------------------
#
# The rules are copied; the binding is not. Every resource in them is
# namespaced, so the chart binds this ClusterRole with two RoleBindings — one
# per namespace the controller's cache watches — rather than granting it the
# cluster. See templates/rbac.yaml.
{
  cat <<'HEAD'
{{/*
GENERATED from config/rbac/role.yaml by hack/chartgen.sh. Do not edit: change
the +kubebuilder:rbac markers, run `make generate`, then `make chart-gen`.
*/}}
{{- if .Values.rbac.create }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "haliphron-runtime.fullname" . }}
  labels: {{- include "haliphron-runtime.labels" . | nindent 4 }}
rules:
HEAD

  sed -n '/^rules:/,$p' config/rbac/role.yaml | tail -n +2

  cat <<'TAIL'
{{- end }}
TAIL
} > "$chart/rbac-controller.yaml"

echo "wrote $chart/crd.yaml $chart/rbac-controller.yaml"
