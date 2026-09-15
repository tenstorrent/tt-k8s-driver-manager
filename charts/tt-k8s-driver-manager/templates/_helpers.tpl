{{/* SPDX-License-Identifier: Apache-2.0 */}}
{{/* SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc. */}}

{{- define "tt-k8s-driver-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "tt-k8s-driver-manager.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "tt-k8s-driver-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "tt-k8s-driver-manager.labels" -}}
helm.sh/chart: {{ include "tt-k8s-driver-manager.chart" . }}
{{ include "tt-k8s-driver-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "tt-k8s-driver-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tt-k8s-driver-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
