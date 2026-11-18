# Zoe-compatible voice gateway deployment

This deployment runs the Zoe-compatible fork
[`ronigooja/chatgpt-web-voice`](https://github.com/ronigooja/chatgpt-web-voice).
Set `VOICE_GATEWAY_REVISION` to an immutable fork commit for production; the
default `main` is intended only for operator development. It is a separate Go
gateway container and does not authorize or complete Zoe business work.

## Host deployment

The installed operator CLI configures the remote gateway endpoint used by Zoe.
It writes the endpoint and a mode-0600 API-key file into the Zoe deployment;
it never handles ChatGPT cookies or the gateway management password:

```bash
sudo zoe voice configure \
  --url https://voice.example.internal \
  --token-file /private/voice-api-key
sudo zoe voice status
sudo zoe voice test
```

The gateway is external to this host. `zoe voice configure` updates the
existing Zoe Compose deployment and reloads the server. `zoe voice test` calls
the gateway's authenticated `/v1/health` endpoint.

Run `sudo deploy/voice/deploy.sh` from a Zoe checkout. The script checks out
the configured fork revision under `/opt/zoe/voice/upstream`, creates the
gateway's management password and AES encryption key if this is a first
install, builds the upstream image, and starts the container with Docker's
`unless-stopped` restart policy. It never imports a ChatGPT account. The
database persists under `/var/lib/zoe/voice`; deployment configuration and
generated credentials are root-readable only under `/etc/zoe`.
An initial downstream key named `zoe-control-plane` is created once and
stored in `/etc/zoe/voice-api-key.env`; later deployments reuse this file.

By default, the management page is bound only to host loopback:

```text
http://127.0.0.1:18090/login
```

Open this URL from a browser that can reach the Linux host's loopback address
(for example, through an operator-managed port forward). Read the generated
management login from `/etc/zoe/voice-admin.txt` on the Linux host. Keep that
file there; do not paste it into chat, tickets, or source files. On the
management UI, add only a personally owned ChatGPT Web account that the user
is authorized to use and that has Web Voice access. This account's
`access_token` must be entered manually; it is not an OpenAI API key, is
encrypted in the gateway database, and does not refresh automatically.

The gateway uses plain HTTP on loopback and on the private Docker network named
`zoe`. A LAN bind is an explicit opt-in for an address assigned to the host:

```bash
sudo env VOICE_LAN_BIND=192.168.1.4 ./deploy/voice/deploy.sh
```

The script persists this bind address at `/etc/zoe/voice.lan.env` and adds a
second host mapping while retaining the loopback mapping. It accepts only a
private IPv4 host address; it does not bind all interfaces. Without this
setting, port 18090 stays loopback-only. The LAN page uses HTTP, so only use it
on a trusted private network. To return to loopback-only, redeploy with
`sudo env VOICE_LAN_BIND= ./deploy/voice/deploy.sh`. The container has a
384 MiB memory cap, 1 CPU cap, 128 PID cap, a read-only root filesystem, and
bounded local Docker logs. Persistent gateway data is stored in
`/var/lib/zoe/voice`.

## Zoe integration configuration

Inside the shared Docker network, the gateway URL is
`http://voice-gateway:8090`; the downstream API base routes include
`GET /v1/voice/config`, `POST /v1/voice/sessions`,
`POST /v1/voice/sessions/{id}/context`, and
`DELETE /v1/voice/sessions/{id}`. Downstream access uses a separately created
Bearer API key (`vgw_live_…`) and cannot access the management pages or account
pool. The key is shown only once when created and must be stored in a
root-readable secret file.

The container listener is plain HTTP; expose it to Zoe only through an
approved private HTTPS reverse proxy or equivalent trusted transport. The
voice topology, termination boundary, and unsupported account-media mode are
defined in [media](../../docs/design/flows/media.md).
Deployment alone does not prove voice calls or task dispatch work; see the
[evidence and acceptance guidance](../../docs/development.md#evidence-and-acceptance).
