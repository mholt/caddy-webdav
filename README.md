WebDAV for Caddy
================

This package implements a simple WebDAV handler module for Caddy.

Requires Caddy 2+.

## Syntax

```
webdav [<matcher>] {
	root <path>
}
```

Because this directive does not come standard with Caddy, you need to [put the directive in order](https://caddyserver.com/docs/caddyfile/options). The correct place is up to you, but usually putting it at the end works if no other terminal directives match the same requests:

```
{
	order webdav last
}
```


## Credit

Special thanks to @hacdias for making caddy-webdav for Caddy 1, from which this work is derived: https://github.com/hacdias/caddy-webdav
