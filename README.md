# ÖVERGÅNG

An IKEA TRÅDFRI gateway emulator on top of Home Assistant, in the spirit of
[Bifrost](https://github.com/chrivers/bifrost) for Hue. The old TRÅDFRI app talks to
it as if it were the real gateway, while the lights actually live in Zigbee2MQTT (or
anywhere else Home Assistant can reach).

Status: prototype. The emulator, Home Assistant connector, logging proxy and client
are working and tested against each other, but **not yet against the real app**.

## What it does

- Serves the gateway's CoAP API over DTLS with pre-shared keys on UDP 5684, pairing
  apps with a security code just like the real gateway.
- Announces itself over mDNS as `_coap._udp` `gw-<mac>`.
- Shows every configured Home Assistant entity as a TRÅDFRI device:

  | Home Assistant              | App                    | Writable                                      |
  | --------------------------- | ---------------------- | --------------------------------------------- |
  | `light.*`                   | bulb (W, WS or CWS)    | on/off, brightness, colour temp, colour, fade |
  | `switch.*`                  | control outlet         | on/off                                        |
  | `cover.*`                   | FYRTUR blind           | position, stop                                |
  | battery `sensor.*` (remote) | remote with battery    | no                                            |
  | config room                 | group                  | on/off, dim, mood                             |

  Names, group membership, moods and smart tasks are read-only; the app gets 4.03 if it
  tries to change them. Each room has the three predefined moods (FOCUS, EVERYDAY,
  RELAX), which set colour temperature and brightness on its lights.

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

Open questions only the app can answer:

- **The QR code.** The app pairs by scanning the QR code on the gateway's label.
  Decoding the real one (any phone camera will do) shows the format to reproduce with
  our own security code.
- **How the app finds the gateway:** mDNS only, or does it remember an IP?
- **Undocumented calls:** firmware or update checks, and writes during onboarding.
  Unknown requests are logged as `UNHANDLED`.

## Deployment

The image is built by GitHub Actions and published to
`ghcr.io/stanmart/overgang`. On the Home Assistant host it runs like the rest of the
stack: Docker with host networking (for mDNS and UDP 5684), config and state in
`/var/lib/overgang` mounted at `/data`, and the token from an env file.

## Layout

- `cmd/overgang`: the `serve`, `client` and `proxy` commands
- `internal/gateway`: CoAP/DTLS server, pairing, observe
- `internal/bridge`: TRÅDFRI resources ↔ Home Assistant entities and services
- `internal/backend`: the backend interface; `homeassistant` (websocket) and `fake`
- `internal/tradfri`: resource paths, attribute keys and colour helpers
- `internal/proxy`, `internal/client`, `internal/mdns`, `internal/state`

The protocol details come from [pytradfri](https://github.com/home-assistant-libs/pytradfri)
and [node-tradfri-client](https://github.com/AlCalzone/node-tradfri-client).
