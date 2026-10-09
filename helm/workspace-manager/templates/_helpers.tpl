{{/*
Expand the name of the chart.
*/}}
{{- define "workspace-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "workspace-manager.fullname" -}}
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

{{/*
Chart label value. A label value is at most 63 characters and begins and ends
alphanumeric: the cut of a long version (a branch build's
<version>-dev.<branch>.<date>.<time>.<sha>, or the <version>+<digest>
helm-controller installs) can land on any run of ".", "_" (from "+") and "-",
so the whole run is trimmed.
*/}}
{{- define "workspace-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimAll "-._" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "workspace-manager.labels" -}}
helm.sh/chart: {{ include "workspace-manager.chart" . | quote }}
{{ include "workspace-manager.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "workspace-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "workspace-manager.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "workspace-manager.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "workspace-manager.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The platform identity contract (global.identity), an empty dict when absent.
*/}}
{{- define "workspace-manager.globalIdentity" -}}
{{- dig "identity" (dict) (.Values.global | default dict) | toJson }}
{{- end }}

{{/*
Existing Secret with the Dex client secret: oauth.existingSecret, else the
platform's global.identity.existingSecret, else the chart-rendered one.
*/}}
{{- define "workspace-manager.oauthSecretName" -}}
{{- $g := include "workspace-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.existingSecret | default (dig "existingSecret" "" $g) | default (printf "%s-oauth" (include "workspace-manager.fullname" .)) }}
{{- end }}

{{/*
Whether the chart renders its own OAuth Secret (no existing one named).
*/}}
{{- define "workspace-manager.oauthRendersSecret" -}}
{{- $g := include "workspace-manager.globalIdentity" . | fromJson -}}
{{- if and .Values.oauth.enabled (not .Values.oauth.existingSecret) (not (dig "existingSecret" "" $g)) }}true{{ end }}
{{- end }}

{{/*
OAuth base URL: oauth.baseURL, else https://<fullname>.<global.domain>.
*/}}
{{- define "workspace-manager.oauthBaseURL" -}}
{{- $domain := dig "domain" "" (.Values.global | default dict) -}}
{{- $derived := "" -}}
{{- if $domain }}{{ $derived = printf "https://%s.%s" (include "workspace-manager.fullname" .) $domain }}{{ end -}}
{{- required "oauth.baseURL is required when oauth.enabled (or set global.domain)" (.Values.oauth.baseURL | default $derived) }}
{{- end }}

{{/*
Dex issuer / client id with the global.identity fallbacks.
*/}}
{{- define "workspace-manager.oauthDexIssuerURL" -}}
{{- $g := include "workspace-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.issuerURL (or global.identity.issuerUrl) is required when oauth.enabled" (.Values.oauth.dex.issuerURL | default (dig "issuerUrl" "" $g)) }}
{{- end }}

{{- define "workspace-manager.oauthDexClientID" -}}
{{- $g := include "workspace-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.clientID (or global.identity.clientId) is required when oauth.enabled" (.Values.oauth.dex.clientID | default (dig "clientId" "" $g)) }}
{{- end }}

{{/*
CA Secret of a private-certificate Dex: oauth.dex.caSecret, else
global.identity.ca. Name empty means system trust.
*/}}
{{- define "workspace-manager.oauthDexCASecretName" -}}
{{- $g := include "workspace-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.dex.caSecret.name | default (dig "ca" "secretName" "" $g) }}
{{- end }}

{{- define "workspace-manager.oauthDexCASecretKey" -}}
{{- $g := include "workspace-manager.globalIdentity" . | fromJson -}}
{{- if .Values.oauth.dex.caSecret.name }}{{ .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ else }}{{ dig "ca" "key" "" $g | default .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ end }}
{{- end }}

{{/*
Trusted audiences, comma-separated: the OAuth client ids whose Dex id_tokens
this server accepts as bearer tokens. The union, in this order and without
duplicates, of
  - oauth.trustedAudiences, else the platform client (global.identity.clientId)
    -- the client MCP clients and the muster CLI log in with;
  - muster.mcpServer.auth.requiredAudiences -- every token muster forwards to
    this server carries them by construction (muster requests them at login)
    and they are the audiences the kube-apiserver trusts, so a portal
    session's token, which carries them but not the platform client, is
    accepted too.
*/}}
{{- define "workspace-manager.oauthTrustedAudiences" -}}
{{- $g := include "workspace-manager.globalIdentity" . | fromJson -}}
{{- $base := .Values.oauth.trustedAudiences | default (list (dig "clientId" "" $g)) -}}
{{- $auds := list -}}
{{- range concat $base (.Values.muster.mcpServer.auth.requiredAudiences | default list) -}}
{{- if and . (not (has . $auds)) }}{{ $auds = append $auds . }}{{ end -}}
{{- end -}}
{{- join "," $auds -}}
{{- end }}

{{/*
The OTLP collector the pod exports to, as YAML {namespace, port}: the
namespace of an in-cluster Service host (<svc>.<ns>.svc[.cluster.local]),
empty for any other host; the endpoint's port, else the protocol's default
(4317 gRPC, 4318 HTTP). Empty when no endpoint is set.
*/}}
{{- define "workspace-manager.otlpEgress" -}}
{{- with .Values.observability.otel.endpoint -}}
{{- $url := urlParse (ternary . (printf "grpc://%s" .) (contains "://" .)) -}}
{{- $hostPort := splitList ":" $url.host -}}
{{- $host := first $hostPort -}}
{{- $port := ternary (last $hostPort) "" (gt (len $hostPort) 1) -}}
{{- if not $port -}}
{{- $port = ternary "4318" "4317" (hasPrefix "http/" ($.Values.observability.otel.protocol | default "grpc")) -}}
{{- end -}}
{{- $labels := splitList "." $host -}}
{{- $namespace := "" -}}
{{- if and (ge (len $labels) 3) (eq (index $labels 2) "svc") -}}
{{- $namespace = index $labels 1 -}}
{{- end -}}
namespace: {{ $namespace | quote }}
port: {{ $port }}
{{- end -}}
{{- end -}}
