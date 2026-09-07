# WebDAV for Caddy

This package implements a simple WebDAV handler module for Caddy.

> [!NOTE]
> This is not an official repository of the [Caddy Web Server](https://github.com/caddyserver) organization.

## Compiling

The recommended way is to use [xcaddy](https://github.com/caddyserver/xcaddy):

```sh
xcaddy build --with github.com/mholt/caddy-webdav
```

Alternatively ou can clone and build and run like this:

1. Clone `git clone https://github.com/mholt/caddy-webdav.git`
2. In the project folder, run `xcaddy` just like you would run `caddy`.
   For example: `xcaddy list-modules` and you should see the `webdav` modules.

## Syntax

```
webdav [<matcher>] {
	root <path>
	prefix <request-base-path>
	atomic_upload
	temp_file_dir <path>
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

The `prefix` directive is optional but has to be used if a webdav share is used in combination with matchers or path manipulations. This is because webdav uses absolute paths in its response. There exist a similar issue when using reverse proxies, see [The "subfolder problem", OR, "why can't I reverse proxy my app into a subfolder?"](https://caddy.community/t/the-subfolder-problem-or-why-cant-i-reverse-proxy-my-app-into-a-subfolder/8575).

```
webdav /some/path/match/* {
	root /path
	prefix /some/path/match
}
```

If you want to serve WebDAV and directory listing under same path (similar behaviour as in Apache and Nginx), you may use [Request Matchers](https://caddyserver.com/docs/caddyfile/matchers) to filter out GET requests and pass those to [file_server](https://caddyserver.com/docs/caddyfile/directives/file_server).

Example with authenticated WebDAV and directory listing under the same path:

```
@get method GET HEAD

route {
    basicauth {
        username hashed_password_base64
    }
    file_server @get browse
    webdav
}
```

Or, if you want to create a public listing, but keep WebDAV behind authentication:

```
@notget not method GET HEAD

route @notget {
    basicauth {
        username hashed_password_base64
    }
    webdav
}
file_server browse
```

## Atomic Upload

By default, WebDAV PUT requests write directly to the destination file. If the client disconnects or the network fails mid-upload, an incomplete file may be left at the target location.

Enabling `atomic_upload` stages PUT uploads as temp files before atomically moving them to their final path. This means:

- Readers never see incomplete files — they either see the full file or no file at all
- Cross-filesystem uploads automatically fall back to a copy-and-rename strategy to maintain atomicity
- Failed or interrupted uploads do not pollute the destination directory

### Configuration Rules

- `atomic_upload` is disabled by default. When off, behavior is identical to upstream.
- When `atomic_upload` is enabled:
  - If `root` is a static path (contains no `{` placeholder), `temp_file_dir` may be omitted. A `.caddy_webdav_temp` directory will be created automatically under `root`.
  - If `root` contains placeholders (e.g. `{http.request.host}`), you **must** explicitly set `temp_file_dir`, or provisioning will fail.

#### Scope

Atomic upload only applies to full overwrite writes (such as PUT). Append and partial-update operations bypass the staging mechanism and write directly to the destination.

#### Placeholder Resolution in `temp_file_dir`

`temp_file_dir` is resolved **exactly once** during the module's lifetime (on the first request), after which it remains fixed. This means even if you use placeholders (such as `{http.request.host}`), they are only evaluated on the first request and never change again.

If you absolutely need a per-request staging directory, this is not currently supported due to the complexity of managing cleanup lifecycles across dynamic paths; leave `atomic_upload` disabled instead.

### Temp File Management

Under normal circumstances, temp files are cleaned up promptly after a successful or failed upload. However, in certain situations (such as a process crash), cleanup may fail and the temp file becomes orphaned. These orphaned files are automatically removed after they exceed **7 days** in age.

- Cleanup runs every **24 hours**
- When multiple WebDAV instances share the same temp directory, cleanup is automatically deduplicated to prevent concurrent scans

### Examples

Static root with the default temp directory (defaults to `[root]/.caddy_webdav_temp`):

```caddyfile
webdav /dav/* {
	root /data
	prefix /dav
	atomic_upload
}
```

Dynamic root with an explicit temp directory:

```caddyfile
webdav {
	root /data/{http.request.host}
	atomic_upload
	temp_file_dir /var/tmp/webdav-staging
}
```

Equivalent JSON:

```json
{
	"handler": "webdav",
	"root": "/data",
	"atomic_upload": true,
	"temp_file_dir": "/var/tmp/webdav-staging"
}
```

## Permissions

To use the WebDAV PUT command, the caddy process needs to be able to write to the storage directory. On Linux, this ordinarily means that the directory needs to be owned by the `caddy` user, or it must be world-writable (the `w` permission needs to be set for other).

Additionally, if running caddy via a systemd, it may be necessary to add the storage directory to the `ReadWriteDirectories` option of the service. If you explicitly set `temp_file_dir` to a location outside the root path, you must also add that directory to `ReadWriteDirectories`. For details, see [this issue](https://github.com/mholt/caddy-webdav/issues/21#issue-811534672).

## Credit

Special thanks to @hacdias for making caddy-webdav for Caddy 1, from which this work is derived: https://github.com/hacdias/caddy-webdav
