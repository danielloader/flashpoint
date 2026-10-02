# flashpoint

Hot reload for a **Go HTTP server with a Vite/React front end**: one command runs both, rebuilds the server when you save, and stops everything on one Ctrl-C.

![flashpoint's TUI: tabs for API, Web and All logs, and a status bar with a failed build](docs/demo.png)

## Quick start

1. **Install it.**

   ```
   brew install danielloader/tap/flashpoint
   ```

   Or use `go install github.com/danielloader/flashpoint/cmd/flashpoint@latest`. Binaries for macOS and Linux are on the [releases page](https://github.com/danielloader/flashpoint/releases).

2. **Have your server read its port from `PORT`, and point Vite's proxy at `FLASHPOINT_API_URL`.**

   ```go
   port := cmp.Or(os.Getenv("PORT"), "8080")
   log.Fatal(http.ListenAndServe(":"+port, mux))
   ```

   ```ts
   server: { proxy: { "/api": process.env.FLASHPOINT_API_URL ?? "http://127.0.0.1:8080" } },
   ```

3. **Run `flashpoint`** in your project.

### What happens on save

flashpoint builds the new server in the background while the old one keeps answering. If the build fails, the old server stays up and the error appears in the status bar. If it succeeds, flashpoint stops the old server, waits for the port to be released, and starts the new one on the same port. The gap is the new server's start-up: a few tens of milliseconds, plus macOS's scan of each new binary unless you turn it off (see below). Vite keeps doing its own hot reload for the front end.

**macOS tip:** macOS scans the first launch of each new binary, which takes about 0.3–1s and lands in that gap. To remove it, enable your terminal app under System Settings → Privacy & Security → Developer Tools (and your editor too, if you run flashpoint from its terminal). If Terminal isn't in that list, `sudo spctl developer-mode enable-terminal` adds it.

[`examples/basic`](examples/basic) is a complete minimal project: a Go JSON API and a Vite React page. To try it, run `cd examples/basic/web && npm install && cd .. && flashpoint`.

## What it detects

| | how |
|---|---|
| project root | the nearest directory at or above the current one with `flashpoint.toml` or `go.mod` |
| main package | the root package if it is `main`, otherwise the only `main` under `./cmd/...`, otherwise the only one in the module. If there are several, flashpoint lists them and asks for `--main`. |
| Vite app | the first of `.`, `web/`, `frontend/`, `ui/` and `client/` with a `vite.config.*` |
| package manager | `packageManager` in package.json, otherwise the nearest lockfile (`pnpm-lock.yaml`, `yarn.lock`, `bun.lock(b)`, `package-lock.json`), otherwise npm |
| dev command | `<pm> run dev`. When the script is plain `vite`, flashpoint adds `--port N --strictPort`. Otherwise it exports the port as `FLASHPOINT_WEB_PORT` and leaves the script alone. |

## Ports, and running checkouts side by side

The API defaults to port 8080 and the web app to 5173. Each can be set with a flag, an environment variable or the config file, in that order of precedence:

```
flashpoint --api-port 8091 --web-port 5191
FLASHPOINT_API_PORT=8091 FLASHPOINT_WEB_PORT=5191 flashpoint
```

Port `0` picks a free port. Two worktrees can run at once, each with its own pair.

If a port is taken, flashpoint says who holds it (via `lsof`, when it is installed) and exits with code 3. **It never kills the holder**, which might be your other checkout.

## The TUI

On a terminal, flashpoint shows tabs for the **API** logs, the **Web** logs and **All** of them interleaved and labelled. Below the logs is a status bar:

- **API**: ● building (amber, showing elapsed time), ● ready (green, with "rebuilt in 1.4s"), or ● build failed / crashed / offline (red). A failed build also shows the first compiler error, and `e` jumps to it.
- **Web**: ● starting, ● ready or ● down.
- **The URLs** of the API and the web app, as OSC 8 hyperlinks. Click one, or press `o`.

| key | |
|---|---|
| `1` `2` `3`, `tab` | API, Web and All (or click a tab) |
| `↑` `↓` `pgup` `pgdn` `g` `G` | scroll; `G` returns to follow mode (the mouse wheel also scrolls) |
| `/` | filter lines (`esc` clears the filter) |
| `c` | clear the current tab |
| `e` | jump to the last build error |
| `r` | rebuild and restart the API now |
| `w` | restart the web dev server |
| `o` / `O` | open the web app / open the API |
| `m` | turn mouse capture on or off (off lets the terminal select text) |
| `q`, `ctrl+c` | stop everything and quit; pressing again leaves immediately |
| `?` | help |

## Plain mode

`--no-tui` prints labelled lines. It is also the default when stdout or stdin is not a terminal, when `CI` is set, or when `TERM=dumb`. This suits agents and CI logs:

```
flashpoint ▸ api http://localhost:8080 · web http://localhost:5173
api ▸ watching 2 packages of . in 3 directories
api │ 2026/10/02 15:01:06 listening on [::]:8080
api ▸ ready in 412ms (build 343ms, restart 22ms)
web │   VITE v8.3.2  ready in 733 ms
web ▸ ready on http://localhost:5173
api ▸ main.go changed; building
api ▸ build failed in 103ms; still serving the last good build
api │ ./main.go:20:6: syntax error: unexpected name main, expected (
api ▸ main.go changed; building
api ▸ ready in 801ms (build 752ms, restart 19ms)
```

Colour is used only on a terminal, and `NO_COLOR` turns it off.

## Configuration

Everything is optional. Put a `flashpoint.toml` at the project root, and unknown keys are rejected so a typo is caught.

```toml
[api]
main = "./cmd/server"        # main package to build
port = 8080                  # 0 picks a free port
host = ""                    # address to bind; empty means all interfaces
build_flags = ["-tags=dev"]  # added after flashpoint's own
args = ["-v"]                # passed to the server (or: flashpoint -- -v)
env = { LOG_LEVEL = "debug" }
health = "/healthz"          # polled until it answers below 500; default "/"
port_env = "PORT"            # variable that carries the port to the server
stop_signal = "SIGINT"       # sent to stop the old server
stop_timeout = "10s"         # then SIGKILL

[web]
enabled = true
dir = "frontend"
command = "pnpm dev --port {port}"   # replaces the detected command; runs under sh -c
port = 5173
env = { VITE_FLAG = "1" }

[watch]
enabled = true                                      # false: rebuild only on request
include = ["config/**/*.yaml", "migrations/*.sql"]  # extra files that rebuild the API
exclude = ["internal/gen/**"]                       # never rebuild for these
debounce = "150ms"

[logs]                       # paths are relative to the project root
dir = ".flashpoint/logs"     # api.log, web.log, flashpoint.log and all.log
api = ""                     # or name any stream's file on its own
web = ""
flashpoint = ""
all = ""
timestamps = true            # RFC 3339 prefix on each line
truncate = false             # append, with a session header
max_size = "10MB"            # then rotate to .1
```

`{port}` and `{api_url}` are replaced in `web.command`.

### Environment flashpoint sets

| variable | set for | value |
|---|---|---|
| `PORT` (or `api.port_env`) | API | the API port |
| `FLASHPOINT=1` | both | |
| `FLASHPOINT_API_URL` | both | `http://127.0.0.1:<api port>`, for the Vite proxy |
| `FLASHPOINT_WEB_URL` | API | `http://localhost:<web port>` |
| `FLASHPOINT_WEB_PORT` | web | the web port |

### Flags

```
flashpoint [flags] [-- server args]
  -C dir                 run in dir
  --main pkg             main package to build
  --api-port N           API port
  --web-port N           web port
  --web-dir dir          the Vite app's directory
  --no-web               run the API only
  --no-tui               plain output
  --watch=false          rebuild only on SIGUSR1 or `flashpoint reload`
  --log-dir dir          api.log, web.log, flashpoint.log, all.log
  --log-api|--log-web|--log-flashpoint|--log-all path
  --log-timestamps=false
  --log-truncate
  --log-max-size 10MB
  --version

flashpoint reload [--wait] [--timeout 60s] [--web] [-C dir]
flashpoint logs api|web|flashpoint|all [-n 100] [--since-build] [-C dir]
```

### Exit codes

| code | meaning |
|---|---|
| 0 | stopped cleanly (`q`, Ctrl-C, SIGTERM or SIGHUP) |
| 1 | could not start: no `go.mod`, no main package, a bad `package.json`, and similar |
| 2 | bad flags or a bad `flashpoint.toml` |
| 3 | a port is already in use |

`flashpoint reload` and `flashpoint logs` exit with 0 on success, 1 when the build failed or the API did not come up, 4 when no flashpoint is running for the project, and 124 when `--timeout` runs out.

## Driving flashpoint from an agent or script

By default flashpoint rebuilds on every save. An agent that edits several files in a row may prefer to say when: run with `--watch=false` (or `watch.enabled = false`) and trigger rebuilds yourself. The triggers also work with watching on.

| | |
|---|---|
| `kill -USR1 $(cat .flashpoint/pid)` | fire and forget; no dependencies |
| `flashpoint reload` | the same, from the CLI |
| `flashpoint reload --wait` | blocks until the build is done: exit 0 once the new API answers, 1 with the compiler errors on stderr if it failed |
| `curl --unix-socket .flashpoint/ctl -X POST 'http://flashpoint/reload?wait=1'` | the same over HTTP: `200 {"ok":true,"buildMs":812,…}`, or `422` with `"errors": [...]` |

A rebuild on request takes the same path as a save: the old server keeps serving while the new one builds. The log says why each build ran: `reload requested (signal)` or `(cli)`. `SIGUSR2` (or `flashpoint reload --web`) restarts the web dev server. `SIGHUP` still means quit, because closing a terminal sends it.

flashpoint keeps its per-project state in `.flashpoint/` at the project root. That directory holds `pid`, the control socket `ctl`, and log files if you ask for them, plus a `.gitignore` of its own so you never commit it. The watcher ignores it. The control socket is a unix socket (mode 0600) that speaks plain HTTP:

| | |
|---|---|
| `POST /reload[?wait=1]` | rebuild; with `wait`, 200 when ready, 422 on a build failure |
| `POST /restart-web` | restart the dev server |
| `GET /status` | the API and web states, the URLs, the last build and ready times, the log paths |
| `GET /logs/{api,web,flashpoint,all}?n=100&since_build=1` | the tail of a stream (also `flashpoint logs`) |

There is no TCP control port for now. An opt-in `--control-addr` with a token, for containers, could come later.

### One log file per stream

`--log-dir .flashpoint/logs` is the recommended setup for agents. It writes `api.log` (the server's output), `web.log` (Vite's), `flashpoint.log` (builds, restarts, reloads and compiler errors) and `all.log` (all of them, labelled). Each line gets a timestamp and has its colours stripped (the terminal keeps them). Each file is flushed per line, so `tail -f` is live. Every build is bracketed in `api.log` and `flashpoint.log`:

```
2026-10-02T15:04:02.113+01:00 --- build #12 started (cli)
2026-10-02T15:04:02.508+01:00 ./internal/api/hello.go:21:9: undefined: greeting
2026-10-02T15:04:02.508+01:00 --- build #12 failed in 395ms
```

So `flashpoint logs api --since-build` (or a `grep` from the last marker) shows only what the last build did. The files are written in TUI and plain mode alike. A write error is reported once, and it never blocks a child. If the disk falls behind, lines are dropped and counted.

```
flashpoint --watch=false --log-dir .flashpoint/logs
# … edit …
flashpoint reload --wait && tail -n 50 .flashpoint/logs/api.log
```

### With Claude Code

This hook in `.claude/settings.json` rebuilds after every edit. When the build fails, the compiler errors go back to Claude: exit code 2 feeds a PostToolUse hook's stderr to the model. When flashpoint isn't running, the hook stays quiet.

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Edit|MultiEdit|Write",
        "hooks": [
          {
            "type": "command",
            "command": "flashpoint reload --wait --timeout 90s >/dev/null; rc=$?; [ $rc -eq 1 ] && exit 2; exit 0"
          }
        ]
      }
    ]
  }
}
```

If you'd rather not use a hook, end the agent's edit step with `flashpoint reload --wait`, and read `flashpoint logs api -n 50` when it fails.

## How it works

The design comes from replacing air in a large Go + React project, with about 600 packages in the build graph and a 61 MB binary. There, every save cost 6–8 seconds and seconds of failed requests.

- **Build first, then restart.** The old server keeps serving during `go build`. A failed build changes nothing. The restart itself is stop (SIGINT, then SIGKILL after `stop_timeout`), a bounded wait of up to 2s for the port to be free, and start. While no server is running at all, for example after a failed first build or a crash, flashpoint answers on the port with a `503` that carries the compiler output, and it releases the port before the next start.
- **Watch exactly what the build reads.** The watch set is `go list -deps` for the main package: the Go, cgo and `go:embed` files of your module's packages (and of workspace modules and local `replace`s), plus `go.mod`, `go.sum` and `go.work`. Tests, the module cache, `node_modules` and the front end are left out, with no globs to maintain. The list is refreshed after each build, and a trailing 150 ms debounce turns a burst of saves into one build.
- **Skip restarts that change nothing.** If the new binary is byte-identical to the running one, nothing restarts. That covers same-content rewrites and comment edits outside the main package. An edit to the main package changes the binary's build ID, so it does restart.
- **Fast dev build flags.** Builds use `-buildvcs=false -ldflags=-w`. A handler edit took 1.30s to produce a binary this way, against 1.79s with the defaults (median of 10 interleaved rounds). `-trimpath` is left out, so stack traces keep real paths.
- **Probing is cheap and bounded.** The readiness check uses one keep-alive HTTP client, polling from 20 ms up to 250 ms with a 60 s deadline. The web check backs off to 1 s and stops once Vite answers. Nothing polls while idle. `GET /status` reports `probeDials`, and the tests assert that it stays at 0 over 10 idle seconds.
- **Nothing is left behind.** Each child runs under a small shim in its own process group. When flashpoint exits, even by `kill -9`, the shim stops the whole group: npm, its shell and node. No orphan is left holding a port.

In that project, a handler edit went from 6.6–8.0s to 2.4–2.7s until the API answered again. The earlier figure was air v1.62.0 with `delay = 2000` and `kill_delay = "2s"`.

## Compared with other tools

Each of these is a good tool. flashpoint is narrower: it covers only the Go API + Vite pair. Its restart is the same idea as theirs: stop the old process and start the new one, with a short gap. It is short because the build happens before the stop, which current air also does on macOS and Linux. The table reflects each project's README and source on 2026-10-02.

| | flashpoint | [air](https://github.com/air-verse/air) | [wgo](https://github.com/bokwoon95/wgo) | [gow](https://github.com/mitranim/gow) | [watchexec](https://github.com/watchexec/watchexec) |
|---|---|---|---|---|---|
| language | Go | Go | Go | Go | Rust |
| watch set | derived from `go list -deps` | extensions, dirs and regexes in `.air.toml` | `-file`/`-dir` regexes (`.go` by default for `wgo run`) | `-w` dirs and `-e` extensions (default `go,mod`) | paths, extensions and filters; honours `.gitignore` |
| change batching | trailing debounce, 150 ms | fixed `delay` after the first event, then a flush ([engine.go]) | trailing debounce, 300 ms | none found | `--debounce`, 50 ms |
| old server during the build | keeps serving | keeps serving on macOS and Linux; stopped first on Windows ([engine.go]) | stopped, then rebuilt and rerun ([wgo_cmd.go]) | signalled, then `go run` again ([gow_cmd.go]) | not a build tool; `--restart` stops the command and runs it again |
| skips a byte-identical binary | yes | no | no | no | n/a |
| runs Vite beside the API | yes, labelled, one Ctrl-C | no (`[[build.rules]]` run commands on change) | yes, `:: wgo …` runs parallel watchers | run several instances | one command per instance |
| browser reload | no (Vite reloads the front end) | `[proxy]` injects a script into HTML responses | no | no | no |
| children's process group | own group via a shim; cleaned up even if flashpoint is SIGKILLed | own group, signalled on stop ([util_linux.go]) | own group, SIGTERM on stop ([util_unix.go]) | finds descendants with `ps` instead of groups | group (or session on macOS) by default; `--stdin-quit` exits when stdin closes |
| UI | TUI (tabs, filter, status, links) or plain | log output | log output | hotkeys in raw mode | `--interactive` keys |
| licence | MIT | GPL-3.0 | MIT | Unlicense | Apache-2.0 |

What happens to each tool's children when the tool itself is SIGKILLed is not documented for air, wgo, gow or watchexec, so the table does not claim it.

Choose air if you want one mature tool for any Go project with proxy-based live reload. Choose wgo or gow for something minimal. Choose watchexec for a general-purpose watcher in any language. It can also hold sockets across restarts with `--socket`, if you need zero refused connections.

[engine.go]: https://github.com/air-verse/air/blob/master/runner/engine.go
[util_linux.go]: https://github.com/air-verse/air/blob/master/runner/util_linux.go
[wgo_cmd.go]: https://github.com/bokwoon95/wgo/blob/main/wgo_cmd.go
[util_unix.go]: https://github.com/bokwoon95/wgo/blob/main/util_unix.go
[gow_cmd.go]: https://github.com/mitranim/gow/blob/master/gow_cmd.go

## Caveats

- **macOS and Linux only.**
- **Drain before exiting.** flashpoint stops the old server with SIGINT and waits for it. `http.Server.Serve` returns as soon as `Shutdown` begins, so a `main` that returns at that point cuts off requests still in flight. Wait for `Shutdown` to finish, as [`examples/basic`](examples/basic/main.go) does.
- **State held in memory is lost on each restart**, like any restart-based reloader.
- **There is a brief gap on each restart.** Requests during it are refused, and a browser or Vite's proxy may show an error. It's a dev server, so this is accepted.
- **flashpoint needs `go` on `PATH`**, plus your package manager when there is a web app.

## Releasing

Push a `v*` tag. The Release workflow runs goreleaser, which publishes the archives and updates `Formula/flashpoint.rb` in [danielloader/homebrew-tap](https://github.com/danielloader/homebrew-tap). The tap step needs the `HOMEBREW_TAP_TOKEN` secret, and goreleaser skips that step when the secret is unset. Published assets are never replaced: every release is a new tag.

## Licence

[MIT](LICENSE)
