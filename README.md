# spotify-tokener

A simple GoLang service which returns spotify anonymous & account tokens without having to deal with their annoying TOTP shit.

## Installation

For god's sake, just use docker. Otherwise, have fun compiling yourself:

### Manual

Requirements:

* [go 1.24+](https://go.dev/doc/install)
* [chrome](https://www.google.com/chrome/)

```bash
go build -o spotify-tokener .
```

Now run it via systemd or something.

### Docker

Build this checkout, not the upstream `:master` binary. From the workspace root on the Docker host:

```bash
docker compose -f LavalinkServer/tokener/compose.yml up -d --build spotify-tokener
```

The Compose build context is `../../spotify-tokener`; deploy both directories with that relative layout.
For a standalone checkout, build the same image and use it in your existing container setup:

```bash
docker build -t spotify-tokener:local .
```

Rebuilding/recreating the container is required after source changes. Restarting an old image does not apply patches.
Chrome path defaults to `/headless-shell/headless-shell`; keep `SPOTIFY_TOKENER_ADDR=0.0.0.0:8080` for container networking.
Keep `init: true` and adequate shared memory (`shm_size: '1gb'`).


## Usage

`GET /api/token` returns the original Spotify JSON after checking the token and expiry.
Cookies (including `sp_dc`) are relayed in an isolated browser context for each fetch, never shared with another request.

- Anonymous requests share one in-memory token and one in-flight refresh; refresh starts on demand within 60 seconds of expiry.
- Cookie-bearing requests bypass the shared cache. The service does not persist tokens or cookies to disk or application logs.
- Failed early refresh may use a cached token only if more than 5 seconds remain; expired tokens are never returned.
- At most two browser jobs run per process. Waiting and fetching share a 25-second request deadline.
- When clients disconnect, their work is cancelled. A shared refresh continues only while another client is waiting.
- No internal retry loop; LavaSrc already retries one HTTP 5xx. Responses use `Cache-Control: no-store`.
- HTTP 502 indicates upstream/invalid-payload failure, 504 a deadline, 503 browser cancellation/unavailability,
  400 malformed cookies, and 405 unsupported methods. HTTP error bodies never contain upstream token data.
- `GET /health` checks the local browser connection only. HTTP 200 does not prove Spotify is reachable or playlists are available.
- Startup failure or browser loss exits the process; the existing container restart policy handles recovery. SIGTERM shuts down cleanly.

### Separate Lavalink host

This deployment runs Docker at `192.168.31.20`, not on the Lavalink host:

```yaml
plugins:
  lavasrc:
    spotify:
      customTokenEndpoint: "${SPOTIFY_TOKEN_ENDPOINT:http://192.168.31.20:8080/api/token}"
```

Keep the existing port 8080 mapping. Do not replace the endpoint with `127.0.0.1` on the Lavalink host.
Restrict access to the Lavalink host with your firewall/private network/VPN; this service has no built-in authentication.
Do not expose it directly to the public internet or send account cookies over an untrusted plaintext connection.

From the Lavalink host, check status/latency without printing credentials:

```bash
curl --fail --silent --show-error --max-time 3 http://192.168.31.20:8080/health
curl --silent --show-error --max-time 30 --output /dev/null --write-out 'HTTP %{http_code}; %{time_total}s\n' http://192.168.31.20:8080/api/token
```

A usable token does not grant access to private playlists, bypass Spotify restrictions, or fix mirrored YouTube/SoundCloud audio.
For playlist failures, correlate LavaSrc's `/v4/loadtracks` error with tokener status logs, rather than assuming a Premium policy problem.

### Tests

```bash
go test -race -timeout 60s ./...
go vet ./...
```

Set `SPOTIFY_TOKENER_TEST_CHROME` to the Chrome executable to include browser fixture tests
(delayed response body, upstream rejection, cookie isolation, cancellation). No Spotify account is required.

## License

This project is licensed under the [Apache License 2.0](LICENSE).

## Contributing

Contributions are welcome, but for bigger changes, please open an issue first to discuss what you would like to change.

## Contact

- [Discord](https://discord.gg/sD3ABd5)
- [Matrix](https://matrix.to/#/@topi:topi.wtf)
- [Twitter](https://twitter.com/topi314)
- [Email](mailto:hi@topi.wtf)
