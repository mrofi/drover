## Serving what you build (drover)

`drover` runs the app `web` on port 8090 as two supervised instances behind a load balancer. The binary must
take `--port <n>` and serve HTTP on it.

- Deploy a build: `drover upload web ./path/to/binary --version <name>`. It rolls out instance by instance
  and is called off, leaving the old version serving, if the new one does not start.
- See what is running: `drover status web`. Read what the app printed: `drover logs web`. A failed deploy
  reports the new binary's last output in its error.
- Change a variable of the app for now (lost when the container restarts): `drover env set web KEY=value`.
  It restarts the instances one by one and undoes the change if they do not come up.
- Go back to the version from before: `drover rollback web`. To a specific one: `drover activate web <name>`.

Run `drover llms` for the complete reference (every command, response code and failure cause).

Never stop or restart the `drover` process itself.
