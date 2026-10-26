# ÖVERGÅNG

An IKEA TRÅDFRI gateway emulator on top of Home Assistant, in the spirit of
[Bifrost](https://github.com/chrivers/bifrost) for Hue. The old TRÅDFRI app talks to
it as if it were the real gateway, while the lights actually live in Zigbee2MQTT (or
anywhere else Home Assistant can reach).

Status: prototype. The TRÅDFRI app (firmware 1.21.57 behaviour) pairs with it and
controls lights, plugs, blinds, rooms and scenes against the fake backend; the Home
Assistant connector has not yet run against a real Home Assistant.

## What it does

- Serves the gateway's CoAP API over DTLS with pre-shared keys on UDP 5684, pairing
  apps with a security code just like the real gateway.
- Announces itself over mDNS as `_coap._udp` `gw-<mac>` on host
  `TRADFRI-Gateway-<mac>.local`. The app does not browse for gateways: it asks for the
  serial number, which is the MAC (`mac` in the config) written with dashes, and the
  security code.
- Shows every configured Home Assistant entity as a TRÅDFRI device:

  | Home Assistant              | App                    | Writable                                      |
  | --------------------------- | ---------------------- | --------------------------------------------- |
  | `light.*`                   | bulb (W, WS or CWS)    | on/off, brightness, colour temp, colour, fade |
  | `switch.*`                  | control outlet         | on/off                                        |
  | `cover.*`                   | FYRTUR blind           | position, stop                                |
  | battery `sensor.*` (remote) | remote with battery    | no                                            |
  | config room                 | group                  | on/off, dim                                   |
  | `scene.*` from `[[scenes]]` | scene                  | activate                                      |

  Rooms appear in the order of the config. A room with both lights and blinds is shown
  as two groups, since the app only offers room controls for one or the other. Scenes
  live where the real gateway keeps them: as moods of a hidden SuperGroup, next to the
  built-in "All off".

  Names, group membership, scene contents and smart tasks are read-only; the app gets
  4.03 if it tries to change them.

- Non-IKEA lights work too: the app is told an IKEA model string matching the light's
  features. The app's colour-temperature range is 2200–4000 K, so warmer or cooler
  settings are out of reach from the app.

## Trying it

```sh
nix develop          # Go toolchain
go run ./cmd/overgang serve -config examples/fake.toml -v
```

The fake backend makes up a device for every listed entity and logs the service calls
the app's actions turn into. Talk to it with the bundled client:

```sh
go run ./cmd/overgang client -addr 127.0.0.1:5684 -code overgang-test-code dump
go run ./cmd/overgang client -addr 127.0.0.1:5684 put 15001/65536 '{"3311":[{"5850":1,"5851":127,"5712":10}]}'
go run ./cmd/overgang client -addr 127.0.0.1:5684 observe 15004/131073
```

The client keeps paired keys in `overgang-client.json`, so `-code` is needed only once
per gateway.

For the real thing, start from `examples/homeassistant.toml`. It needs a long-lived Home
Assistant token (`OVERGANG_HA_TOKEN` or `token_file`) and a security code for the app
(`OVERGANG_SECURITY_CODE` or `security_code_file`).

## Finding out what the app needs

Two tools for comparing the emulator against the real gateway:

- `overgang client -addr <real gateway>:5684 -code <code on its label> dump` reads every
  resource the app would. Diff it against a dump of the emulator.
- `overgang proxy -gateway <real gateway>:5684 -code <code> -capture app.jsonl` sits
  between the app and the real gateway and logs every CoAP message in plain text. It
  learns each identity's key by watching the app pair through it, so the app has to be
  paired (or re-paired) while pointed at the proxy. `-mdns-id` announces the proxy so the
  app can find it.

Requests ÖVERGÅNG doesn't know are logged as `UNHANDLED`. `[gateway.resources]` in the
config serves fixed JSON at any path, for trying out what the app expects there.

The proxy logs every message in plain text, including the keys handed out at pairing,
and keeps those keys in its state file: treat its logs and captures as secrets.

## Deployment

The image is built by GitHub Actions and published to
`ghcr.io/stanmart/overgang`. On the Home Assistant host it runs like the rest of the
stack: Docker with host networking (for mDNS and UDP 5684), config and state in
`/var/lib/overgang` mounted at `/data`, and the security code and token from an env
file.

## Security

Anyone on the LAN can reach UDP 5684. Without the security code they get no further
than a DTLS handshake; with it, they can pair and do what the app can: switch, dim and
colour the configured lights, move the configured blinds, switch the configured plugs
and activate the configured scenes. Nothing else in Home Assistant is reachable, and
values are clamped before they get there.

- Use a security code of 16 random letters and digits, like the real gateway's. Shorter
  than 12 is refused: a recorded pairing handshake allows guessing it offline.
- The gateway holds at most 64 connections and 64 paired identities, gives a handshake
  5 seconds and an idle connection 15 minutes, and accepts messages up to 16 KiB.
- Each connection's observe notifications are delivered on their own, with a 10 second
  limit; a peer that stops answering is disconnected rather than holding up others.
- `state.json` holds every paired identity's key; it is written with mode 0600.

## Layout

- `cmd/overgang`: the `serve`, `client` and `proxy` commands
- `internal/gateway`: CoAP/DTLS server, pairing, observe
- `internal/bridge`: TRÅDFRI resources ↔ Home Assistant entities and services
- `internal/backend`: the backend interface; `homeassistant` (websocket) and `fake`
- `internal/tradfri`: resource paths, attribute keys and colour helpers
- `internal/proxy`, `internal/client`, `internal/mdns`, `internal/state`

The protocol details come from [pytradfri](https://github.com/home-assistant-libs/pytradfri)
and [node-tradfri-client](https://github.com/AlCalzone/node-tradfri-client).
