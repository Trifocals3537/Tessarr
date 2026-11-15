# Go dependency advisory follow-up

## November 14 security follow-up

[Issue #28](https://github.com/Trifocals3537/tessarr/issues/28) records four
reachable findings that stopped CI before its race and native-build steps. The
minimum Go version is now **1.26.11**, retaining the 1.26 series. The dependency
updates also remove two imported-package findings and the unused legacy
OpenPGP module finding reported by the current database.

| Component | Previous | Selected | Advisory |
| --- | --- | --- | --- |
| Go | `1.26.10` | `1.26.11` | [GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859) |
| `github.com/anacrolix/torrent` | `v1.55.0` | `v1.62.0` | [GO-2026-6894](https://pkg.go.dev/vuln/GO-2026-6894) |
| `github.com/andybalholm/brotli` | `v1.2.0` | `v1.2.7` | [GO-2026-6888](https://pkg.go.dev/vuln/GO-2026-6888) |
| `github.com/klauspost/compress` | `v1.18.7` | `v1.20.2` | [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865) |
| `golang.org/x/net` | `v0.60.0` | `v0.62.0` | [GO-2026-6866](https://pkg.go.dev/vuln/GO-2026-6866), [GO-2026-6860](https://pkg.go.dev/vuln/GO-2026-6860) |
| `golang.org/x/crypto` | `v0.57.0` | `v0.59.0` | [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) |

The stable torrent release contains upstream fix
[`76452a2`](https://github.com/anacrolix/torrent/commit/76452a2c8a2fd9c59d4d4f10fc3495723d538c7b).
There is no `v1.61.1` tag; `v1.62.0` is the first available stable tag after the
fixed pseudo-version in the advisory. Its module requires Go 1.26.0, which the
selected toolchain satisfies. The newer `x/crypto` root module removes the
legacy OpenPGP packages; their separate deprecated module remains unsafe and
must not be introduced into the application's import graph.

`go mod tidy` records the required companion updates, including `x/sys`,
`x/term`, `x/text`, `x/sync`, protobuf, and the torrent metadata dependencies.
No scanner suppression, module replacement, or CI gate change is included.

The Windows/amd64 source scan with CGO disabled, Go 1.26.11, and the pinned
govulncheck 1.7.0 reports **no vulnerabilities** against the current database.
The CI native Linux job repeats the scan with CGO enabled before the full race
suite, native build, and command-line checks. GitHub records the results for
each published revision.

Regression coverage rejects deeply nested torrent extension fields in both the
outer document and raw info dictionary, while accepting ordinary extensions.
All four excessive-nesting cases reproduced on the previous dependency.
Frozen NZB frames produced with `compress v1.18.7` verify full metadata,
header-only, and sampled message-ID reads after the upgrade, with and without
article geometry. The deprecated torrent magnet constructor is replaced with
the equivalent v1 magnet fields from already validated metadata. Regression
tests retain the infohash, UTF-8 display name, tracker policy, and web seeds.

Reproduce the current checks from the repository root:

```sh
go version
govulncheck -show=verbose ./...
go test -count=1 ./...
go vet ./...
go mod verify
```

Use Linux/amd64 with CGO enabled for the CI race and native compatibility checks.
The dated sections below retain historical results and toolchain pins.

## October 21 toolchain follow-up

The durable-workflow branch's unchanged vulnerability gate found the newly
published [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809) in `crypto/x509`,
reachable through the NNTP TLS handshake. The minimum Go version was raised to
**1.26.10**, the patched release in the retained 1.26 series. The October 19
results and explicit toolchain commands below are historical evidence; current
validation must use the updated `go.mod` pin and the current advisory database.

The docs audit also required Vite **8.1.6**. That patch is pinned through the docs
override and lockfile, retaining the existing Astro version and unrelated locked
dependencies. Its fixes include
[GHSA-rq7h-c2jc-7f22](https://github.com/vitejs/vite/security/advisories/GHSA-rq7h-c2jc-7f22).
Neither security gate is suppressed or relaxed.

## October 19 dependency follow-up

The release-readiness scan of beta `993ea2f7df5ea02840fb0401a6b66df0de1b50e9`
reported ten advisories at module level, with none in imported packages or called
symbols. This update removes all nine findings that have patched versions. The
remaining OpenPGP finding has no fixed version and affects packages that the
application does not import.

## Dependency changes

| Module | Previous | Selected |
| --- | --- | --- |
| `golang.org/x/net` | `v0.56.0` | `v0.60.0` |
| `golang.org/x/crypto` | `v0.53.0` | `v0.57.0` |
| `golang.org/x/text` | `v0.39.0` | `v0.42.0` |
| `golang.org/x/sys` | `v0.46.0` | `v0.48.0` |
| `golang.org/x/term` | `v0.44.0` | `v0.46.0` |
| `golang.org/x/sync` | `v0.21.0` | `v0.23.0` |

`x/net v0.60.0` is the first release fixing all five networking findings. Its
module requirements select `x/crypto v0.57.0`, `x/text v0.42.0`, `x/sys v0.48.0`,
and `x/term v0.46.0`; `x/text` also requires `x/sync v0.23.0`. These requirements
explain the companion updates. They all support the existing **Go 1.26.9** pin.
`go mod tidy` records `x/text` as a direct dependency because application path
normalization imports it directly.

## Findings removed by the updates

| Advisory | Affected code | First fixed module version |
| --- | --- | --- |
| [GO-2026-6355](https://pkg.go.dev/vuln/GO-2026-6355) | SSH established-channel deadlock | `x/crypto v0.56.0` |
| [GO-2026-6354](https://pkg.go.dev/vuln/GO-2026-6354) | SSH undecided-channel deadlock | `x/crypto v0.56.0` |
| [GO-2026-6303](https://pkg.go.dev/vuln/GO-2026-6303) | SSH source-address option enforcement | `x/crypto v0.55.0` |
| [GO-2026-6617](https://pkg.go.dev/vuln/GO-2026-6617) | HTTP/2 HPACK encoder race | `x/net v0.60.0` |
| [GO-2026-6612](https://pkg.go.dev/vuln/GO-2026-6612) | HTTP/2 flow-control refunds | `x/net v0.60.0` |
| [GO-2026-6611](https://pkg.go.dev/vuln/GO-2026-6611) | HTTP/2 window-change CPU exhaustion | `x/net v0.60.0` |
| [GO-2026-6610](https://pkg.go.dev/vuln/GO-2026-6610) | HTTP/2 framing-header validation | `x/net v0.60.0` |
| [GO-2026-6603](https://pkg.go.dev/vuln/GO-2026-6603) | HTTP/2 trailer memory exhaustion | `x/net v0.60.0` |
| [GO-2026-6629](https://pkg.go.dev/vuln/GO-2026-6629) | PRECIS crafted-input panic | `x/text v0.41.0` |

Some HTTP/2 records also cover the Go standard library. The retained Go 1.26.9
toolchain already contains the applicable standard-library fixes; the updates
above address the separately versioned `x/net` module.

## October 19 unused-code finding (historical)

[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) covers
`golang.org/x/crypto/openpgp` and its subpackages. Upstream marks this OpenPGP
implementation unmaintained and unsafe by design. At that time, the advisory
covered all versions with no known fix. The November follow-up above records
the later removal of those packages from the `x/crypto` root module; the
deprecated separate OpenPGP module remains unsafe.

The application retains `x/crypto` for bcrypt authentication. Neither production
code nor test dependencies import `openpgp` or any of its subpackages in the
Linux/amd64 (CGO enabled) or Windows/amd64 (CGO disabled) dependency graphs. The
vulnerable packages are outside the compiled dependency closure for those tested
targets. This is a documented unused-package finding, not a claim that the entire
`x/crypto` module is free of advisories.

No advisory suppression, vendored patch, package replacement, or scanner bypass
is added. Keep the existing vulnerability check enabled. Any future OpenPGP
feature must select a maintained implementation and repeat the import and
vulnerability checks; this record does not authorize using the affected package.

## Verification and reproduction

The post-update source scans used **Go 1.26.9**, **govulncheck 1.7.0**, and the
official database updated **2026-10-19 21:08:39 UTC**. Both Linux/amd64 with CGO
enabled and Windows/amd64 with CGO disabled report:

- Zero vulnerabilities in called symbols.
- Zero vulnerabilities in imported packages.
- One module-level finding: `GO-2026-5932` only.

Run these from the repository root with the pinned scanner installed. Repeat for
each supported build target, including the platform-specific CGO setting:

```sh
GOTOOLCHAIN=go1.26.9 GOOS=linux GOARCH=amd64 CGO_ENABLED=1 govulncheck -show=verbose ./...
GOTOOLCHAIN=go1.26.9 GOOS=windows GOARCH=amd64 CGO_ENABLED=0 govulncheck -show=verbose ./...
GOTOOLCHAIN=go1.26.9 GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go list -mod=readonly -deps -test ./...
GOTOOLCHAIN=go1.26.9 GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go list -mod=readonly -deps -test ./...
```

The dependency listings must contain no package equal to
`golang.org/x/crypto/openpgp` or beginning with `golang.org/x/crypto/openpgp/`.
Recheck against the current database whenever dependencies, imports, build tags,
or supported targets change. CI results and the integration PR record the test,
race, static-analysis, and native compatibility checks for the final revision.

This change updates source dependencies and this record. Live-service validation
and deployment are separate actions.
