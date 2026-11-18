# drover

One static binary that runs any command as a set of supervised instances, load balances one port across
them, and replaces the binary they run without dropping requests. It does the job of supervisord + a
reverse proxy + a rollout script, and it is made to be copied into an existing container image.

```mermaid
flowchart LR
    client([client]) -->|":8080"| lb
    you([you]) -->|"127.0.0.1:9100"| admin
    prom([Prometheus]) -.->|":9101 optional"| metrics

    subgraph drover
        lb[load balancer]
        admin["admin API<br/>status, releases, rollback, logs"]
        metrics["/metrics"]
        store[("releases<br/>builtin, v1, v2, ...")]
        admin --> store
        admin -. "rolling update" .-> lb
    end

    lb --> i0["instance 0<br/>127.0.0.1:9000"]
    lb --> i1["instance 1<br/>127.0.0.1:9001"]
    lb --> i2["instance 2<br/>127.0.0.1:9002"]
    store -. "binary" .-> i0 & i1 & i2
```

- **Supervise.** Each instance is restarted when it exits, with a delay that grows from 1 s to 30 s.
- **Load balance.** Requests go to the healthy instance with the fewest open requests. A request whose
  instance cannot be connected to is retried on another one. WebSockets and streaming responses pass through.
- **Update.** Give it a new binary by URL, from a container image, or by upload. It rolls out one instance
  at a time: the new instance starts beside the old one and takes over only once it is healthy, then the old
  one finishes its open requests and is stopped. If the new binary does not come up, the rollout is called
  off, the old version keeps serving, and the error says what the new binary printed.
- **Binaries or folders.** A version is one binary, or for apps that are not built into one (PHP, Node,
  Python) a whole folder, taken from an archive, a git repository or a folder inside a container image.
- **Roll back.** Every version is kept under the name you gave it. Switch to any of them, or back to the one
  from before, the same way.
- **Observe.** Recent output of every instance over HTTP, and Prometheus metrics.
- **No root.** It writes only to its data and cache directories and uses no privileged calls. Linux only.

**Contents:** [Quick start](#quick-start) · [Moving an existing command](#moving-an-existing-command-to-drover) ·
[Updating the binary](#updating-the-binary) ·
[Folder apps](#folder-apps-php-node-python) · [Adding apps through the API](#adding-apps-through-the-api) · [Commands](#commands) · [Configuration](#configuration) ·
[Environment variables](#environment-variables-of-the-app) ·
[Adding it to an image](#adding-it-to-an-image) · [Logs](#logs) · [Metrics](#metrics) ·
[WebSockets](#websockets-and-other-long-lived-connections) · [For AI agents](#for-ai-agents) ·
[Security](#security) · [Limits](#what-it-does-not-do) · [Develop](#develop) · [Not tested](#not-tested)

## Quick start

Get the binary one of three ways:

```sh
go build -o drover .                                                               # from this repository
curl -fLo drover https://github.com/mrofi/drover/releases/latest/download/drover-linux-amd64 && chmod +x drover
docker build -t ghcr.io/mrofi/drover:latest .                                      # the image, see below
```

Run an app as three instances behind port 8080:

```sh
./drover run --listen :8080 --instances 3 --health /health -- /usr/local/bin/app --port '{port}'
```

`{port}` is replaced with each instance's own port (it is also in `$PORT`). Quote it: zsh and bash treat a
bare `{port}` before a redirect as their own syntax.

In another terminal:

```sh
curl localhost:8080/                      # the app, through the load balancer
./drover status app                       # instances, ports, pids, state
./drover upload app ./app-new --version v2   # rolling update, no dropped requests
./drover rollback app                     # back to the version from before
./drover logs app -f                      # what the instances print
```

With more than one app, or to set timeouts, use a config file: `drover run -c drover.yaml`. Every key is in
[`drover.example.yaml`](drover.example.yaml).

## Moving an existing command to drover

How the app learns its port decides what the command looks like. In every case the public port moves to
`--listen`, and each instance gets a private port of its own (9000, 9001, ...).

**The app takes a port flag.** Put `{port}` where the number went:

```sh
# before
/usr/local/bin/myapp --port 3000
# with drover
drover run --listen :3000 --instances 2 -- /usr/local/bin/myapp --port '{port}'
```

**The app reads `$PORT`.** Drop the `PORT=` prefix: drover sets `PORT` for every instance.

```sh
# before
PORT=3000 /usr/local/bin/myapp --env /etc/myapp.env
# with drover
drover run --listen :3000 --instances 2 -- /usr/local/bin/myapp --env /etc/myapp.env
```

The same as a config file, with other environment variables under `env:`:

```yaml
apps:
  - name: myapp
    command: /usr/local/bin/myapp --env /etc/myapp.env
    listen: ":3000"
    instances: 2
    env:
      LOG_LEVEL: info
```

**The binary is not the first word** (an interpreter runs it). Say which file a release replaces:

```yaml
apps:
  - name: api
    command: node {bin} --port {port}
    binary: /srv/server.js
    listen: ":3000"
```

Things that go wrong:

- **`VAR=value` in front of the command.** No shell runs the command, so `PORT=3000 myapp` would look for a
  program named `PORT=3000`. Use `env:` in the config (or set the variable for drover itself; instances
  inherit its environment).
- **A fixed port somewhere else.** If an env file the app loads by itself (like `/etc/myapp.env` above)
  sets `PORT` and wins over the real environment, every instance tries the same port and all but one keep
  crashing. Remove it there. (`PORT` under `env:` or in drover's `env_file` is harmless: drover's own value
  always wins.)
- **Listening on another interface.** drover connects to instances on `127.0.0.1`, so the app must listen
  on `127.0.0.1` or `0.0.0.0`.
- **No health path.** Without `--health /path` an instance counts as healthy as soon as its port accepts
  connections, which may be before the app is ready.

## Updating the binary

The first word of the command is the binary that a release replaces (or put `{bin}` where it belongs, as in
the `node` example above). A new binary comes in one of three ways:

```sh
# drover downloads it
curl -X POST localhost:9100/apps/app/releases \
  -d '{"url": "https://example.com/builds/app-1.4.2", "version": "1.4.2", "sha256": "<optional>"}'

# drover takes it out of a container image, through an image-file-fetch server
# (https://github.com/mrofi/image-file-fetch; set image_fetch_url or DROVER_IMAGE_FETCH_URL)
curl -X POST localhost:9100/apps/app/releases \
  -d '{"image": "ghcr.io/me/app:1.4.2", "path": "/usr/local/bin/app", "version": "1.4.2"}'

# you upload it
curl -T ./app localhost:9100/apps/app/releases/1.4.2
wget -qO- --method=PUT --body-file=./app localhost:9100/apps/app/releases/1.4.2
```

Each of these stores the version and rolls it out. The call returns when the rollout is over, with the new
status (200) or the reason it was called off (500, and the app is still on the version from before). Add
`"activate": false` (or `?activate=false` on an upload) to store a version without switching to it.

### Sources that need a login

| Source | How to authorize |
|---|---|
| URL with a token | `--header "Name: value"`, repeatable (`"headers": {...}` in the JSON) |
| URL with basic auth | `--username` / `--password`, or `https://user:pass@host/...` |
| URL that is pre-signed (S3, GCS) | nothing: the authorization is in the URL |
| Container image | `--username` / `--password`, passed on to image-file-fetch |
| Git repository | `--username` / `--password` (a token as the password), over http(s) |

```sh
drover fetch app --url https://gitlab.example.com/api/v4/projects/1/packages/generic/app/1.4.2/app \
  --header "PRIVATE-TOKEN: $TOKEN"
drover fetch app --url https://artifacts.example.com/app-1.4.2 --username ci --password "$TOKEN"

curl -X POST localhost:9100/apps/app/releases \
  -d '{"url": "https://example.com/app", "headers": {"Authorization": "Bearer '"$TOKEN"'"}}'
```

Credentials are used for that one request. They are not stored, not written to the version's record
(a login in the URL is cut out of it) and not logged. When a download is redirected to another host, or
from https to http, the headers and the login are not sent along, so a token given for one site does not
reach the storage host behind it. A header given together with `--username` wins if it is `Authorization`.

### Versions

- The name you give (`1.4.2`, `v2`, a git SHA, `2026-11-16-hotfix`) is the version's tag: letters, digits,
  `.`, `_`, `-`, up to 64 characters. Without one, drover uses a UTC timestamp like `20261116-154400`.
- `builtin` is the binary that came with the image. It is always in the list and can be switched back to.
- Any stored version can be made current at any time; the switch is a rollout like any other.
- A name belongs to one binary for good. To reuse it, delete the version first. There are no movable
  aliases such as `stable`.

## Folder apps: PHP, Node, Python

An app that is a tree of files instead of one binary is a **folder app**: a version is a whole folder,
`{dir}` is the folder of the version an instance runs, and it is the instance's working directory. An app
is a folder app when its command uses `{dir}` or the config has `folder:`.

```yaml
apps:
  - name: site
    command: php -S 127.0.0.1:{port} -t {dir}/public
    listen: ":8080"
    folder: /srv/site        # the built-in version; it may not exist yet (then upload a first one)
```

```sh
drover run --listen :8080 --folder /srv/site -- php -S '127.0.0.1:{port}' -t '{dir}/public'
```

| | Command | What must be in the folder |
|---|---|---|
| PHP | `php -S 127.0.0.1:{port} -t {dir}/public` (or FrankenPHP, RoadRunner) | the source and `vendor/` |
| Node | `node {dir}/dist/server.js` | the built output and production `node_modules/` |
| Python | `python {dir}/app.py`, with `env: { PYTHONPATH: "{dir}/deps" }` | the source and `pip install --target deps` |

`{dir}` also works in `dir:` and in `env:`, env file and override values, and every instance gets
`DROVER_DIR`. The interpreter comes from the container image, not from the folder.

A new version comes from one of four places. In every case `subdir` keeps only one folder of what was
fetched, for a build that sits in `dist/` or under the single top folder of a downloaded archive.

```sh
# 1. a local folder, or an archive (tar, tar.gz, tar.bz2, zip)
drover upload site ./dist --version 1.4.2
drover upload site site.zip --subdir site-main/dist
curl -T site.tar.gz 'localhost:9100/apps/site/releases/1.4.2?subdir=dist'

# 2. an archive by URL
drover fetch site --url https://example.com/builds/site-1.4.2.tar.gz --subdir dist

# 3. a folder of a git repository, at a branch, tag or commit
drover fetch site --git https://github.com/me/site --ref v1.4.2 --subdir dist
curl -X POST localhost:9100/apps/site/releases \
  -d '{"git": "https://github.com/me/site", "ref": "v1.4.2", "subdir": "dist", "version": "1.4.2"}'

# 4. a folder inside a container image, through image-file-fetch
drover fetch site --image ghcr.io/me/site:1.4.2 --path /var/www/html
```

Everything else is the same as for a binary: the rollout goes instance by instance, a folder whose app does
not start is not activated, every version stays for `activate` and `rollback`.

- **The folder has to be complete.** drover does not build: run `composer install`, `npm ci`, `pip install`
  in CI and ship the result. Packages with compiled parts must be built for the container's OS, CPU and
  interpreter version; the easy way is to build inside the image you deploy.
- **Its path changes with every version** (`<cache>/<app>/<version>-<id>`). The app must not store
  its own absolute path, and what it writes (uploads, SQLite, sessions) belongs outside the folder, or it
  is gone with the next version.
- **git** needs the `git` command in the container. Only `http(s)` URLs; for a private repository give
  `--username` and `--password` (a token), which drover passes to git through the environment, not the
  command line. Only the one commit is fetched, `.git` is not kept, submodules are not fetched, and the
  release records the commit it came from.
- **Links are kept if they stay inside.** A link that points outside the folder (an absolute path, or `..`
  past the top) makes drover refuse the archive, as does a device or socket. Names that try to leave the
  folder (`../x`, `/x`) are kept inside it.
- **Size.** `max_binary_bytes` (1 GiB) caps both the archive and what it unpacks to, and a folder may have
  at most 200000 entries. Every version is a full copy: delete old ones with `drover rm`.

## Adding apps through the API

With the switch on, apps can be added and removed while drover runs, without editing the config or
restarting. It is off by default, because whoever can call the admin API can then run any command and open
new ports.

```sh
drover run --manage-apps                       # or admin.manage_apps: true, or DROVER_MANAGE_APPS=1
                                               # (with no config file, drover starts with no app at all)

drover app add api --listen :8081 --instances 2 --health /health -- '{bin}' --port '{port}'
drover app add site --listen :8082 --folder /srv/site -- php -S '127.0.0.1:{port}' -t '{dir}/public'
drover app list
drover app show api                            # the definition it was added with (env names only)
drover app rm api                              # stop it, close its port, keep its versions
drover app rm api --purge                      # ... and delete its versions
```

```sh
curl -X POST localhost:9100/apps -d '{"name": "api", "command": "{bin} --port {port}", "listen": ":8081",
                                      "instances": 2, "health": {"path": "/health", "interval": "2s"}}'
curl -X DELETE 'localhost:9100/apps/api?purge=true'
```

- **The same keys as in the config file**, with durations as strings (`"30s"`). Unknown keys are refused.
- **It starts empty** when there is nothing to run yet, and runs a first `drover upload` or `fetch`.
- **Ports are checked before anything starts.** Without `port_base`, the first free range from 9000 is
  picked. A name in use, overlapping instance ports or a taken `listen` port is refused with 409.
- **Persistent.** The definitions are kept in `<data>/apps.json`, and drover starts those apps again when it
  restarts, on the version each was running. That only holds if the data directory is on a volume; without
  one, a container restart loses them, their versions included.
- **`env` values given here are written to `apps.json`** (mode 0600, readable by drover only). Secrets
  belong in an `env_file`; the API only ever shows the names.
- **Apps from the config file cannot be changed or removed through the API.** If the config file later
  defines an app with the name of an API app, the config wins: the API app is left out with a warning,
  stays recorded, and can be removed with `drover app rm`.
- **An API app whose port is taken at boot** is left out with a warning instead of keeping drover from
  starting. A config app whose port is taken still stops the start.
- Its status shows `"origin": "api"` (`"config"` for the others).
- Removing an app waits for nothing: open requests get up to 10 seconds, then the instances stop. A
  rollout of it that is running makes the removal fail with 409.
- Changing an existing app (instances, command) is not possible yet: remove it and add it again under the
  same name, which keeps its versions.

## Commands

Every operation is an HTTP call on the admin API and a subcommand of the binary, for images without curl.
The subcommands read `DROVER_ADMIN` (default `http://127.0.0.1:9100`) and `DROVER_TOKEN`.

| Goal | Command | HTTP |
|---|---|---|
| Status of all apps / one app | `drover status [app]` | `GET /apps`, `GET /apps/<app>` |
| Add an app (switch on) | `drover app add <name> --listen <addr> [flags] -- <command>` | `POST /apps` |
| Remove an app added through the API | `drover app rm <name> [--purge]` | `DELETE /apps/<name>[?purge=true]` |
| Definition of an API app | `drover app show <name>` | `GET /apps/<name>/spec` |
| List versions | `drover releases <app>` | `GET /apps/<app>/releases` |
| Add a version from a file, folder or archive | `drover upload <app> <path> --version <v>` | `PUT /apps/<app>/releases/<v>` |
| Add a version from a URL | `drover fetch <app> --url <url> --version <v>` | `POST /apps/<app>/releases` |
| Add a version from an image | `drover fetch <app> --image <ref> --path </file or folder> --version <v>` | `POST /apps/<app>/releases` |
| Add a version from git (folder apps) | `drover fetch <app> --git <url> [--ref r] [--subdir d]` | `POST /apps/<app>/releases` |
| Switch to a version | `drover activate <app> <v>` | `POST /apps/<app>/releases/<v>/activate` |
| Back to the version from before | `drover rollback <app>` | `POST /apps/<app>/rollback` |
| Rolling restart, same version | `drover restart <app>` | `POST /apps/<app>/restart` |
| Delete a version | `drover rm <app> <v>` | `DELETE /apps/<app>/releases/<v>` |
| Recent output | `drover logs <app> [-f] [--tail n] [--instance n]` | `GET /apps/<app>/logs` |
| Variable names, by source | `drover env <app>` | `GET /apps/<app>/env` |
| Set / remove an override | `drover env set <app> K=V...`, `drover env unset <app> K...` | `PATCH /apps/<app>/env` |
| Drop every override | `drover env clear <app>` | `DELETE /apps/<app>/env` |
| Edit the env file | `drover env set <app> --file K=V...`, `drover env unset <app> --file K...` | `PATCH /apps/<app>/env/file` |
| Replace the env file | `drover env put <app> <file>` | `PUT /apps/<app>/env/file` |
| Prometheus metrics | `drover metrics` | `GET /metrics` |
| Reference for AI agents | `drover llms` | |
| Is drover up | | `GET /healthz` |

`fetch` and `upload` also take `--sha256 <hex>`, `--no-activate` and, for folder apps, `--subdir <d>`; `fetch` takes `--username`,
`--password` and `--header` for [sources that need a login](#sources-that-need-a-login). Subcommands print the JSON answer and exit 0, or print the error and
exit 1. Response codes and JSON shapes are in [`llms.txt`](llms.txt).

## Configuration

`drover run` takes either flags and a command (one app), or `-c drover.yaml`
([every key](drover.example.yaml)). Flags, for the one-app form: `--name` `--listen` `--instances`
`--port-base` `--health` `--env-file` `--folder`. For both forms: `--admin` `--metrics` `--data`
`--cache` `--manage-apps`.

| Variable | Read by | Meaning |
|---|---|---|
| `DROVER_CONFIG` | server | Config file, when `-c` is not given. Then `./drover.yaml`, then `/etc/drover.yaml`. |
| `DROVER_ADMIN_LISTEN` | server | Admin API address. Order: `--admin`, this, `admin.listen`, `127.0.0.1:9100`. |
| `DROVER_TOKEN` | server, client | Admin API token. On the server it wins over `admin.token`. |
| `DROVER_DATA` | server | Data directory, when `data:` and `--data` are not set. Default `~/.local/share/drover`. |
| `DROVER_CACHE` | server | Where folder versions are unpacked, when `cache:` and `--cache` are not set. Default `~/.cache/drover`. |
| `DROVER_IMAGE_FETCH_URL` | server | image-file-fetch server for image sources. Wins over `image_fetch_url`. |
| `DROVER_MANAGE_APPS` | server | `1` or `true`: allow adding and removing apps through the API. |
| `DROVER_ADMIN` | client | URL of the admin API, default `http://127.0.0.1:9100`. |
| `PORT`, `DROVER_APP`, `DROVER_INDEX`, `DROVER_VERSION`, `DROVER_DIR` | set for each instance | Its port, app name, instance number, version and, for a folder app, the folder of that version. |

### Changing the admin address

The server and the clients are told separately, because one is an address to listen on and the other a URL
to connect to:

```dockerfile
ENV DROVER_ADMIN_LISTEN=127.0.0.1:9500 \
    DROVER_ADMIN=http://127.0.0.1:9500
```

An address that is not loopback (`:9500`, `0.0.0.0:9500`) needs `DROVER_TOKEN`, or drover does not start.

### Ports

| Port | Default | Set with |
|---|---|---|
| Public port of an app | `:8080` (flag form) | `--listen`, `listen:` |
| Instances | `9000` upward; an app uses `port_base` to `port_base + 2 × instances − 1` | `--port-base`, `port_base:` |
| Admin API | `127.0.0.1:9100` | `--admin`, `DROVER_ADMIN_LISTEN`, `admin.listen` |
| Metrics only | off | `--metrics`, `metrics.listen` |

An app needs twice its instance count in ports because a rollout runs old and new side by side. Keep
`port_base` below 32768: from there up Linux gives ports to outgoing connections, and an instance cannot
bind a port one of those happens to hold.

## Environment variables of the app

An instance's environment is built in layers, later ones winning:

| | Source | Changed by | Survives a restart of drover |
|---|---|---|---|
| 1 | drover's own environment (`ENV` in the Dockerfile, `docker run -e`), without `DROVER_TOKEN` | restarting drover | yes |
| 2 | `env:` in the config | restarting drover | yes |
| 3 | the app's `env_file` (`--env-file`), read each time an instance is started | editing the file, or the API | yes, it is your file |
| 4 | overrides set through the API, kept in memory | the API | **no** |
| 5 | `PORT`, `DROVER_APP`, `DROVER_INDEX`, `DROVER_VERSION`, `DROVER_DIR` | nobody: drover sets them | |

To change variables without downtime, put them in an `env_file` and restart the instances one by one:

```yaml
apps:
  - name: web
    command: /usr/local/bin/web --port {port}
    listen: ":8080"
    env_file: /etc/web.env
```

```sh
vi /etc/web.env && drover restart web          # edit by hand, then a rolling restart

drover env set web --file LOG_LEVEL=debug      # or let drover edit the file and restart
drover env unset web --file OLD_FLAG
drover env put web ./web.env                   # replace the whole file

drover env set web FEATURE_X=on                # an override: not written anywhere, gone when drover restarts
drover env clear web
drover env web                                 # which variables come from where (names only)
```

```sh
curl -X PATCH localhost:9100/apps/web/env/file -d '{"set": {"LOG_LEVEL": "debug"}, "unset": ["OLD_FLAG"]}'
curl -X PUT   localhost:9100/apps/web/env/file --data-binary @web.env
curl -X PATCH localhost:9100/apps/web/env      -d '{"set": {"FEATURE_X": "on"}}'
curl -X DELETE localhost:9100/apps/web/env
```

- **Every change is a rolling restart**, and it is undone if the instances do not come up with it: the file
  or the overrides go back to what they were, the old instances keep serving, and the error says what the
  new ones printed.
- **`--no-restart` (`?restart=false`) only stores the change.** Nothing tries it, so nothing is undone:
  the running instances keep their values, and the next rollout of any kind (a restart, a new version, a
  rollback) is the first to use it. If the values are bad, that rollout fails and keeps failing until they
  are fixed; its error notes that the environment had changed, so a bad variable is not mistaken for a bad
  binary. Use it to batch several changes before one `drover restart`.
- **Pending changes are visible.** `drover env` shows `"pending": true` and `drover status` shows
  `"env_pending": true` while the stored environment differs from what the instances run with, which
  includes a file edited by hand. `drover_env_pending` is the same as a metric.
- **A variable changes only through a rollout.** An instance restarted after a crash gets the environment
  it was created with, so a half-edited file never reaches a running app by accident.
- **Values are never returned.** `drover env` and `drover status` show names only. drover stores nothing:
  the file is yours, the overrides live in memory. Do not treat overrides as configuration; put what must
  last in the file.
- **The file format** is `KEY=VALUE` per line, `#` comments, an optional `export`, values optionally in
  single quotes (literal) or double quotes (`\n`, `\"`, `\\`). No `$VAR` expansion and no comments after a
  value. Editing through the API keeps comments and other lines. A file that does not parse stops a rollout
  rather than starting instances on half of it; a file that does not exist yet counts as empty.
- **The file must be writable by drover** for the API to edit it (a read-only mounted secret cannot be;
  edit it at its source and `drover restart`). A file drover creates is mode 0600.
- **Rolling back the binary does not roll back variables.** Versions and environment are independent.

## Adding it to an image

```dockerfile
FROM your-image
COPY --from=ghcr.io/mrofi/drover:latest /drover /usr/local/bin/drover
USER 1000
CMD ["drover", "run", "--listen", ":8080", "--instances", "3", "--", "app", "--port", "{port}"]
```

The image holds only `/drover` and is published for amd64 and arm64 when a version tag is pushed
(`:1.2.3`, `:1.2`, `:latest`; see [`publish.yml`](.github/workflows/publish.yml)). To pin a version, use its
tag in the `COPY --from`. Without a registry, take the binary from the GitHub release instead:

```dockerfile
ADD --chmod=755 https://github.com/mrofi/drover/releases/latest/download/drover-linux-amd64 /usr/local/bin/drover
```

A complete example is in [`examples/inject`](examples/inject).

- **State.** Releases, the current version and apps added through the API live in the data directory. Put
  it on a volume, or back it up, and a restarted container comes back as it was. Without either it starts
  on `builtin` again.
- **Backing up the data directory.** It holds plain files only: a binary per file version, one
  `tree.tar.gz` per folder version, and small JSON records. So any copy brings everything back, even one
  that keeps neither file modes nor links (rclone to S3, for one): drover sets the executable bit of a
  binary again, and a folder's modes, links and lock files are inside its archive. Folder versions are
  unpacked into the **cache directory** (`cache:`, `--cache`, `DROVER_CACHE`; default `~/.cache/drover`),
  which needs no backup: a missing folder is unpacked again from its archive when it is first run.
- **Read-only root filesystem.** Point the data and cache directories at a writable mount that allows
  execution (`--tmpfs /tmp:exec` with `DROVER_DATA=/tmp/drover DROVER_CACHE=/tmp/drover-cache`).
- **Beside another main process.** Start `drover run ... &` from the image's own entrypoint or hook.
  [`examples/opencode-host`](examples/opencode-host) does this for
  [opencode-host](https://github.com/mrofi/opencode-host): opencode keeps port 8080, the app is on 8090, and
  the agent is told how to deploy what it builds.

## Logs

Every line an instance prints goes to drover's stdout with the instance in front (`[web#0] ...`), so
`docker logs` and whatever collects your container logs (Loki, Fluent Bit, your platform) already have
them. That is the place for history.

drover also keeps the last 1000 lines of each app in memory (`log_lines`), together with its own events
about that app (rollouts, exits, restarts), and serves them as plain text:

```sh
curl localhost:9100/apps/web/logs                 # the last 200 lines
curl 'localhost:9100/apps/web/logs?tail=50'
curl 'localhost:9100/apps/web/logs?instance=0'    # one instance
curl -N 'localhost:9100/apps/web/logs?follow=true'  # keep the connection and stream new lines

drover logs web
drover logs web -f --instance 0 --tail 50
```

```
2026-11-16T16:26:11Z [web] rollout v1 -> v2
2026-11-16T16:26:11Z [web#0] listening on 9002
2026-11-16T16:26:12Z [web] now on v2
```

stdout and stderr are merged, and the timestamp is when drover received the line. The buffer is lost when
drover stops. When a rollout fails because the new binary exits, the error also carries the last lines that
binary printed, so a failed `drover upload` says why.

## Metrics

`GET /metrics` on the admin API returns Prometheus metrics (with the token, when one is set). To let
Prometheus scrape from outside the container, open a second listener that serves only `/metrics` and needs
no token:

```sh
drover run --metrics :9101 ...        # or in the config:  metrics: { listen: ":9101" }
curl localhost:9101/metrics
```

| Metric | Type | Meaning |
|---|---|---|
| `drover_requests_total{app,code}` | counter | Requests answered, by status code. `499`: the client went away, `101`: upgraded connection. |
| `drover_request_duration_seconds{app}` | histogram | Time from receiving a request to finishing its response (upgraded connections excluded). |
| `drover_retries_total{app}` | counter | Requests sent to another instance because the first could not be connected to. |
| `drover_instance_exits_total{app}` | counter | Times an instance exited by itself and was restarted. |
| `drover_rollouts_total{app,result}` | counter | Finished rollouts and rolling restarts, `success` or `failed`. |
| `drover_instances{app}` | gauge | Instances the app is configured to run. |
| `drover_healthy_instances{app}` | gauge | Instances passing their health check. |
| `drover_inflight_requests{app,instance}` | gauge | Requests open on each instance right now. |
| `drover_rollout_in_progress{app}` | gauge | 1 while a rollout runs. |
| `drover_env_pending{app}` | gauge | 1 while a stored environment change has not been applied by a rollout. |
| `drover_version_info{app,version}` | gauge | The version the app is on. |
| `drover_build_info{version}` | gauge | The drover version. |

Alerts worth having: `drover_healthy_instances < drover_instances` for a few minutes,
`increase(drover_instance_exits_total[10m]) > 3`, and `increase(drover_rollouts_total{result="failed"}[1h]) > 0`.
Counters start at zero when drover starts. These describe the load balancer and the processes; drover does
not collect metrics from inside your app.

## WebSockets and other long-lived connections

They are proxied like any request and stay on the instance that accepted them. During a rollout an old
instance waits `drain_timeout` (default 30s) for its open connections, then it is stopped and they are
closed; clients must reconnect, and land on the new version. Raise `drain_timeout` to give them longer.
Nothing ties a reconnecting client to a particular instance (no sticky sessions), so state a connection
needs must not live in one instance's memory.

## For AI agents

[`llms.txt`](llms.txt) is the whole reference in one file: the model, every command with its HTTP
equivalent, response codes, the config keys, recipes, and a symptom-to-cause table. It is also built into
the binary, so an agent working inside a container that only has drover can read it:

```sh
drover llms
```

## Security

- The admin API can replace the binaries drover runs, which is code execution. It listens on
  `127.0.0.1:9100`, and drover refuses to start with it on any other address unless a token is set
  (`admin.token` or `DROVER_TOKEN`). Clients then send the token in either header:
  `X-Drover-Token: <token>` or `Authorization: Bearer <token>`.
- Behind a proxy that has its own login, use `X-Drover-Token`, so `Authorization` stays the proxy's. The
  subcommands always send `X-Drover-Token`; a proxy's basic auth can go in the URL:
  `DROVER_ADMIN=https://user:pass@admin.example.com`. With curl:
  `curl -u user:pass -H "X-Drover-Token: $DROVER_TOKEN" https://admin.example.com/apps`.
- `admin.manage_apps` widens what the admin API can do: from replacing what configured apps run to running
  any command as a new app and opening new public ports. Keep it off unless you need it.
- Whoever can call the admin API can set an app's variables and replace its env file, but cannot read
  values back through it. (They could still deploy a binary that prints them: the admin API is full control.)
- The logs endpoint is part of the admin API, so app output has the same protection. The separate metrics
  listener has none: it serves numbers, app names and version names to anyone who can reach it.
- The apps do not get `DROVER_TOKEN` in their environment.
- A `url` or `git` source makes drover request that URL from inside the container (git only over http or
  https, and the ref cannot be an option). Only hand the admin API to people
  who may do that.
- `sha256` is checked before a version is stored. drover does not verify signatures.

## What it does not do

- TLS. Put it behind your platform's HTTPS or a proxy.
- Raw TCP. The load balancer speaks HTTP/1.1 to the instances.
- Sticky sessions.
- Building. A folder version is used as it arrives; build it in CI.
- Changing the number of instances of a running app. For a config app, change the config and restart
  drover; for an API app, remove it and add it again.
- Keeping logs. The buffer is for looking at what just happened; history belongs to your log collection.
- Reaping orphaned processes when it is PID 1. If your app leaves grandchildren behind, run it under an init
  (`docker run --init`).

## Develop

```sh
go test -race ./...   # builds a small test app and runs real processes (see Tested below)
go vet ./...
```

CI ([`ci.yml`](.github/workflows/ci.yml)) runs these on every push and pull request, then builds the image
and runs `examples/inject` as uid 1000 on a read-only root filesystem. Pushing a tag `v1.2.3` publishes the
image to `ghcr.io/mrofi/drover` and attaches `drover-linux-amd64`, `drover-linux-arm64` and `SHA256SUMS` to
a GitHub release.

| File | What is in it |
|---|---|
| `main.go`, `client.go` | The command line: `run`, and the subcommands that call the admin API. |
| `config.go` | The config file, its defaults and checks. |
| `drover.go` | Listeners: one per app, the admin API, the optional metrics port. |
| `app.go` | One app: its instances, the rollout, and the load balancer. |
| `proc.go` | One instance: start, restart, health check, stop. |
| `releases.go` | Versions on disk. |
| `archive.go`, `git.go` | Folder versions: unpacking archives safely, checking a tree, fetching a commit. |
| `admin.go` | The admin API. |
| `env.go` | The app's environment: env file, overrides, and changing them with a rolling restart. |
| `managed.go` | Apps added and removed through the API, and `apps.json`. |
| `logs.go`, `metrics.go` | The log buffer and the Prometheus output. |
| `llms.txt` | The agent reference, embedded in the binary. |

## Not tested

- **arm64.** The image and the release binary are built for it, but drover has only been run on amd64.
- **A real WebSocket library.** The test uses a bare protocol upgrade with an echo, not framed WebSocket
  traffic from a client library.
- **A single file through a real image-file-fetch server.** That path is tested against a stand-in that
  answers `POST /download` the same way; a folder was fetched through the real server by hand.
- **Real PHP, Node or Python apps as folder versions.** Folder versions are tested with a small server and
  were run by hand with `python3 -m http.server`; the commands in the table above are not.
- **Real services that need a token.** Headers, basic auth and the redirect rule are tested against local
  servers, not against GitHub, GitLab or an artifact store.
- **A private git repository.** Credentials are passed to git but only public and local repositories were
  fetched. Also not tried: a commit SHA as `ref` against a hosting service, and git over a proxy.
- **Large folders.** Nothing was measured for a release with tens of thousands of files.
- **A real Prometheus scraping `/metrics`.** The output is checked line by line in a test, not parsed by
  Prometheus or shown in Grafana.
- **More than one app in a config.** The port-overlap check is tested; two apps running side by side is not.
- **Two of the command forms above.** An app that reads only `$PORT`, and `command: node {bin}` with
  `binary:`. The `--port '{port}'` form is what the tests and the manual runs use.
- **The client commands with a token, and `drover fetch`.** The token and both fetch sources are tested
  through the HTTP API; the other subcommands were run by hand without a token.
- **The subcommands through a proxy with its own login.** The server accepting `X-Drover-Token` beside
  another `Authorization` is tested; `DROVER_ADMIN=https://user:pass@host` was not run against a real proxy.
- **A mounted, read-only env file.** The "not writable" answer is not tested, nor is an env file that is a
  single bind-mounted file or a Kubernetes secret.
- **API apps with a volume, in a container.** Keeping them across a restart was tested as a process
  restart on the same data directory.
- **Two clients adding apps at the same moment.** Adds and removes are serialized by a lock, but no test
  runs them concurrently.
- **`DROVER_ADMIN_LISTEN` in the test suite.** Its order against the flag and the config was checked by
  hand only.
- **Large folder versions after a restore.** Each is unpacked again on first run, which takes time for
  tens of thousands of files; nothing was measured.
- **A volume on the data directory in a container.** Keeping the version across a restart of drover is
  tested as a process, not with a mounted volume.
- **`examples/opencode-host` with the current binary.** It was built and run before logs and metrics were
  added.
- **`llms.txt` against the code.** It was written from the code; nothing checks that it stays in step.

## Tested

- By `go test -race ./...`, with real processes: load balancing across instances, restart after a crash,
  three rollouts under load with no failed request, rollback, a binary that exits (rollout undone, old
  version still serving, its output in the error), URL and image sources with checksum, the version
  surviving a restart of drover, starting with no binary, the admin token, a protocol upgrade through a
  rollout, the logs endpoint (tail, one instance, follow), the metrics on both listeners, and the app
  environment (layers, file edits through the API, overrides, a failing change being undone, a change
  stored without a restart showing as pending and failing the next rollout with a note), and folder
  versions (tar.gz, zip, a subfolder, a folder that does not start, archives that are refused, a git
  repository at HEAD and at a tag, an image folder, an archive by URL), and URL sources that need a login
  (a header, a bearer token, basic auth, a login in the URL, credentials not following a redirect to
  another host, nothing kept in the version record or the logs), and apps added through the API (the
  switch, adding, refusals, `apps.json`, coming back after a restart on the same version, removing with
  and without purge, the config winning over a recorded name).
- On GitHub: CI and the publish workflow ran for `v0.1.0`; the amd64 release binary and the image were
  downloaded and run.
- By hand: uploads with `curl -T` and `wget --method=PUT`; `curl` on the logs and metrics endpoints;
  `drover status`, `releases`, `upload`, `rollback`, `restart`, `logs`, `env` and `app`; folder versions
  from a local folder, from `nginx:alpine` through a real image-file-fetch server, and from a tag of a
  public GitHub repository; `--admin` and
  `DROVER_ADMIN_LISTEN`; the `examples/inject` image as uid 1000 on a read-only root filesystem with all
  capabilities dropped; the `examples/opencode-host` image (boot hook, first release, rollout under load,
  rollback, crash restart, shutdown hook), and its data directory going through opencode-host's encrypted
  backup into a new container, where a binary and a folder version both ran again.

## License

[MIT with the Commons Clause](LICENSE). You can use, modify and self-host it freely, including inside your
own company and to run your own products, paid ones included. You may **not** sell it, which includes
offering it (or a service substantially made of it) to third parties as a paid SaaS or PaaS. For that, see
[LICENSE-COMMERCIAL.md](LICENSE-COMMERCIAL.md) to get a commercial license.
