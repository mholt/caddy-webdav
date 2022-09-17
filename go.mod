module github.com/mholt/caddy-webdav

go 1.16

// Workaround bug in x/net/webdav while we wait for patch to be merged.
// See issue 16: https://github.com/mholt/caddy-webdav/issues/16
// See Go CL: https://go-review.googlesource.com/c/net/+/285752/
// TODO: Remove this replace once the patch is merged
replace golang.org/x/net => github.com/heimoshuiyu/net v0.0.0-20220813162333-9dad79f931f2

require (
	github.com/caddyserver/caddy/v2 v2.5.2
	go.uber.org/zap v1.21.0
	golang.org/x/net v0.0.0-20220728153142-1f511ac62c11
)
