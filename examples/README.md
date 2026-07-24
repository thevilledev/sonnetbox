# sonnetbox examples

These runnable programs progress from one isolated evaluation to a small HTTP
service. Run every command from the repository root.

## Hello

[`hello`](hello) creates an engine, evaluates inline Jsonnet with a two-second
deadline, supplies an external variable, and closes the engine:

```sh
go run ./examples/hello
```

```json
{
   "message": "Hello, sonnetbox!",
   "sandboxed": true
}
```

An application that evaluates repeatedly should create one engine at startup
and reuse it. Each call still receives a fresh WASM guest.

## Config renderer

[`config-renderer`](config-renderer) exposes a read-only Jsonnet workspace. The
root application imports `service.libsonnet` through the declared `lib` search
path; no ambient filesystem access is available inside the guest.

```sh
go run ./examples/config-renderer \
  -workspace ./examples/config-renderer/jsonnet \
  -environment production
```

```json
{
   "environment": "production",
   "service": {
      "logLevel": "info",
      "name": "catalog",
      "replicas": 3
   }
}
```

The `-environment` flag accepts `development` or `production`. The host passes
it as a request-scoped external variable, while `EvaluateFile` loads the root
file and all imports through `WorkspaceImporter`.

## HTTP service

[`http-service`](http-service) reuses one engine across requests and listens on
`127.0.0.1:8080` by default:

```sh
go run ./examples/http-service
```

In another terminal, submit Jsonnet to `POST /render`:

```sh
curl --fail-with-body \
  --header 'Content-Type: application/json' \
  --data-binary @- \
  http://127.0.0.1:8080/render <<'JSON'
{
  "customer": "acme",
  "source": "local catalog = import \"../lib/catalog.libsonnet\"; std.trace(\"rendering \" + std.extVar(\"customer\"), { customer: std.extVar(\"customer\"), product: catalog(\"starter\") })"
}
JSON
```

The response wraps the rendered JSON together with bounded trace output and
host-observed statistics:

```json
{
  "output": {
    "customer": "acme",
    "product": {
      "monthly_price": 9,
      "name": "Starter"
    }
  },
  "trace": "TRACE: requests/input.jsonnet:1 rendering acme\n",
  "stats": {
    "queue_duration": "...",
    "execution_duration": "...",
    "import_resolutions": 1,
    "import_bytes": 47,
    "capability_calls": 1,
    "trace_bytes": 47,
    "trace_truncated": false
  }
}
```

Durations and trace byte counts vary. The service demonstrates a typical
untrusted-code boundary:

- the client controls source and one external string, but never a host path;
- imports come only from a fixed `MapImporter` bundle;
- `lookup_product` is a narrow, deterministic, read-only capability;
- a context deadline provides the CPU and wall-clock budget;
- per-request limits lower the engine ceilings for source, output, imports,
  capability calls, and traces; and
- typed sonnetbox errors become stable HTTP status codes without exposing
  internal failures.

Capabilities must remain pure: Jsonnet laziness means a capability may run
zero, one, or multiple times. Do not replace the catalog lookup with generic
filesystem, network, shell, or secret access.

## Test the examples

```sh
go test ./examples/...
```

The repository's normal `go test ./...` and `make ci` commands also compile and
test these programs.
