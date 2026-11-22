# opencode-host with drover beside it: opencode keeps port 8080, and the app you (or the agent) build is
# served on 8090 as supervised, load-balanced instances that can be updated without downtime.
FROM ghcr.io/mrofi/opencode-host:latest

COPY --from=ghcr.io/mrofi/drover:latest /drover /usr/local/bin/drover
COPY --chown=node:node drover.yaml   /home/node/drover.yaml
# opencode-host runs only hooks that are executable, and skips the others without a word. --chmod makes them
# executable whatever the source lost on the way (e.g. files uploaded through a web page, like a Hugging Face
# Space's, have no executable bit).
COPY --chown=node:node --chmod=755 entrypoint.d/ /home/node/entrypoint.d/
COPY --chown=node:node --chmod=755 shutdown.d/   /home/node/shutdown.d/
COPY --chown=node:node AGENTS.d/     /home/node/config/AGENTS.d/

# drover's data directory (versions, apps added through the API) is backed up to S3 with opencode's own
# state and restored before the boot hooks run. It holds plain files only, so the S3 copy is complete.
ENV BACKUP_EXTRA_DIRS="drover=/home/node/.local/share/drover"

USER node
EXPOSE 8090
