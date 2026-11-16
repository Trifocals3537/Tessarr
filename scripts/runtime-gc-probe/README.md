# Runtime GC investigation

This developer tool runs a small standard-library-only lifetime probe for
[golang/go#82394](https://github.com/golang/go/issues/82394) in isolated child
processes. It uses synthetic allocations and does not start Tessarr or contact
any services.

```sh
go run ./scripts/runtime-gc-probe
```

The JSON report distinguishes a passing control, an observed runtime GC abort,
a trigger that did not reproduce, and unexpected failures/timeouts. Each child
has a 30-second deadline and a 64 KiB output capture. An expected GC abort is
reported as evidence; it does not make the collection command fail. Unexpected
failures and a failing control do fail the command.

A reproduced upstream pattern establishes a toolchain problem independently of
Tessarr's code. It does **not** establish the cause of Tessarr's earlier Windows
CI abort. Likewise, a passing manager stress run does not prove the intermittent
failure is fixed. Track that investigation in issue #65.

The Windows runtime diagnostic workflow runs this probe and then the manager
suite with JSON test events, garbage-collector validation, and pointer checks.
It saves the GitHub-generated evidence for seven days. Normal product builds
and runtime settings do not use this diagnostic configuration.
