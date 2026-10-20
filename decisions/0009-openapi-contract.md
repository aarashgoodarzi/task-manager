# 0009. A checked-in OpenAPI document as the HTTP contract

* Status: Accepted
* Date: 2026-10-20
* Scope: `api/`, `internal/infra/swagger/`, `internal/infra/server/`,
  `internal/app/`, `go.mod`
* Related: [0005](0005-task-crud-api.md) — the task HTTP contract this document
  formalizes; [0001](0001-clean-architecture.md) — cross-cutting concerns stay
  in adapters or bootstrap code.

## Context

[ADR 0005](0005-task-crud-api.md) defines the task HTTP contract precisely, but
only in prose and Go tests. A client author has to read the handler, the tests,
or the ADR to discover request fields, status codes, defaults, and error shapes.
That is enough for this repository's own tests, but it is not a document a
consumer can import into an HTTP client, a mock server, or an API catalog.

The contract also has behavior that is easy to miss in a code review: strict
JSON decoding, the `400` versus `404` split for malformed versus missing IDs,
the `Location` header on creation, full-replacement `PUT` defaults, the stable
list order, and the error envelope. A machine-readable description makes those
details explicit and gives reviewers something concrete to compare against.

The constraints are the existing architecture: no business package may depend
on a transport, no runtime service should be required for documentation, and
the repository should not gain a code-generation toolchain for a single small
API.

## Decision

### Canonical document

`api/openapi.yaml` is the canonical, hand-written OpenAPI 3.0.3 description of
the HTTP API. It documents:

- every task and health path and method, including the `404`/`405` behavior
  shared by the router;
- success and failure status codes for each operation;
- request and response schemas, required fields, defaults, the closed status
  set, and the UUID identifier pattern;
- the shared error envelope, its stable codes, and the optional `fields` map;
- the `Location` header on `201`, and the bodyless `204` on delete;
- examples for representative success and failure responses.

`servers` is a relative `/`, so the document does not hard-code a host or port;
`security: []` states explicitly that the API requires no authentication. The
operation IDs are stable and unique so generated clients have predictable
names.

### Embedding

`api/openapi.go` embeds `openapi.yaml` with `go:embed`. The document therefore
travels with the binary and cannot be missing at runtime because of a working
directory or packaging mistake. It adds no runtime dependency and no file I/O.

### Serving

`internal/infra/swagger` is the documentation adapter. It is registered by
`internal/app` as a `server.RouteRegistrar`, like a feature handler, and mounts:

| Path | Content |
| --- | --- |
| `/swagger/openapi.yaml` | The embedded OpenAPI document, as `application/yaml` |
| `/swagger/` | The Swagger UI index |
| `/swagger/index.html` | The same index, without a redirect |
| `/swagger/<asset>` | Swagger UI CSS, JavaScript, and favicon assets |

The UI comes from `github.com/swaggest/swgui/v5emb`, which embeds Swagger UI
and serves it through `statigz`. No CDN, external origin, or runtime file is
involved; the documentation works offline and in a network-restricted
deployment. The UI is configured with the embedded spec path, so it loads the
same bytes this service exposes.

The adapter registers only `GET`. Unsupported methods fall through to the
router's existing `405` handler, matching the rest of the API. `/swagger` without
a trailing slash is redirected to `/swagger/` by Gin's trailing-slash handling.
Unknown assets return `404` from the embedded file server.

### Validation and maintenance

The document is validated with Redocly's OpenAPI linter:

```sh
npx --yes @redocly/cli@2.66.0 lint api/openapi.yaml
```

This is a development-only tool; it is not a Go dependency and is not needed to
build or run the service. Any change to a route, payload, status code, or header
updates `api/openapi.yaml` in the same change. The handler tests remain the
behavioral contract tests: the document describes the contract, it does not
generate handlers and it is not generated from them.

There is deliberately no annotation or code-generation step. `swag`-style
annotations would spread documentation strings through handlers and produce a
second source of truth, and generating handlers from the document would make a
small handwritten API depend on a generator for little gain.

## Alternatives considered

1. **Swagger annotations plus generated output (`swag`).** Rejected: it adds a
   generator and generated files to the repository, requires annotations in the
   HTTP adapter, and still needs a check that the generated document is
   current. The API is small enough that a reviewed document is simpler.
2. **Hosted or CDN-loaded documentation.** Rejected: it introduces a runtime
   network dependency and a third-party origin for an internal service, and it
   fails in offline or restricted environments. Embedding keeps the artifact
   local and reproducible.
3. **Shipping a `swagger-ui/` directory beside the binary.** Rejected: the
   documentation would break when the binary is copied without that directory,
   and the container would need an extra asset layer. `go:embed` keeps the UI
   and spec in the same artifact.
4. **Keep the contract in README prose and handler tests.** Rejected: prose is
   not machine-readable, examples drift silently, and clients cannot generate
   code or mocks from it.
5. **OpenAPI 3.1.** Rejected for now: 3.0.3 is the broadest-supported target
   for validators, UI tools, and generated clients, and this API uses no 3.1
   feature. Moving later is possible without changing the HTTP behavior.
6. **Generate the specification from Go types.** Rejected: the handler's DTOs
   are intentionally small and do not carry descriptions, examples, or response
   documentation, so generation would still require annotations or a parallel
   metadata layer.

## Trade-offs

- **Gain:** the contract is explicit, reviewable, and usable by client
  generators and mock tools; examples document behavior that tests alone make
  tedious to discover.
- **Gain:** the document is embedded, so serving it later requires no runtime
  file access and no deployment artifact beyond the binary.
- **Cost:** the document is hand-written and can drift from the handlers. This
  is mitigated by updating it in the same change as the contract, by the
  linter, and by the existing handler tests.
- **Cost:** schemas duplicate the handler DTOs. For five routes this is less
  expensive than introducing code generation; if the API grows substantially,
  the decision should be revisited.
- **Cost:** the linter is a Node.js tool. It is confined to documentation
  validation and never enters the build or runtime path.
- **Cost:** embedding Swagger UI assets increases the binary size. This is
  preferable to a runtime asset directory or CDN for an internal service.

## Consequences

- A contract change is not complete until `api/openapi.yaml` reflects it.
- The document and the handler tests must agree; a disagreement is a bug in one
  of them, not a reason to weaken either.
- The specification and UI are available at `/swagger/openapi.yaml` and
  `/swagger/` whenever the service is running, including in the container.
- The docs adapter is composed in `internal/app` and uses only `GET`; adding a
  second documentation surface would be a new adapter, not a reason to couple
  feature handlers to Swagger UI.
- The linter command and the contract's maintenance rule belong in the README
  so a contributor does not need to infer them from this record.
