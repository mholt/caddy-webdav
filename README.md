WebDAV for Caddy
================

This package implements a simple WebDAV handler module for Caddy.

Requires Caddy 2+.

## Syntax

```
webdav [<matcher>] {
	root <path>
	prefix <request-base-path>
}
```

Because this directive does not come standard with Caddy, you need to [put the directive in order](https://caddyserver.com/docs/caddyfile/options). The correct place is up to you, but usually putting it near the end works if no other terminal directives match the same requests. It's common to pair a webdav handler with a `file_server`, so ordering it just before is often a good choice:

```
{
	order webdav before file_server
}
```

Alternatively, you may use `route` to order it the way you want. For example:

```
localhost

root * /srv

route {
	rewrite /dav /dav/
	webdav /dav/* {
		prefix /dav
	}
	file_server
}
```

The `prefix` directive is optional but has to be used if a webdav share is used in
combination with matchers or path manipulations. This is because webdav uses
absolute paths in its response. There exist a similar issue when using reverse
proxies, see
[The "subfolder problem", OR, "why can't I reverse proxy my app into a subfolder?"](https://caddy.community/t/the-subfolder-problem-or-why-cant-i-reverse-proxy-my-app-into-a-subfolder/8575).

```
webdav /some/path/match/* {
	root /path
	prefix /some/path/match
}
```

## Credit

Special thanks to @hacdias for making caddy-webdav for Caddy 1, from which this work is derived: https://github.com/hacdias/caddy-webdav
