# Windows exception investigation

This developer-only classifier runs the independent, synthetic reproducer from
[golang/go#81238](https://github.com/golang/go/issues/81238) in two child
processes: ordinary panic/recover as a control and a recovered Windows access
violation as the trigger. The diagnostic workflow checks out the [exact
upstream source revision](https://github.com/denisvmedia/ptah-2365-repro/tree/72cb4a8a27ef8c86347f8eee237c23e7b6681e85),
then builds its single Go source file with Tessarr's pinned Go version. No
upstream source is copied into Tessarr.

The classifier requires a live adjacent heap span, matching before/after
snapshots, and the expected GC abort stack before it reports
`exception_gc_abort`. A passing control and clean trigger report
`not_reproduced` for that runner. Timeouts, missing layout evidence, truncated
output, and unrelated crashes fail the diagnostic job. Each child has a
45-second deadline and a 128 KiB output limit.

The workflow samples sixteen separate GitHub-hosted Windows runners because the
upstream report found that the failure depended on the runner's processor and
enabled context state. It records each runner's processor, enabled XSTATE mask,
and required Windows exception context size using documented Windows APIs. A
clean host cannot exclude the problem elsewhere. An
independent reproducer matching the GC error also cannot establish that this
was Tessarr's original failure. Follow attribution in issue #65.

On hosts with a context larger than the current 4096-byte Windows stack
reserve, the workflow also runs three complete manager-suite repetitions on
the **same runner**, using normal GC settings. Sample 1 runs those repetitions
as a control even if its context is small. This tests whether the application
suite fails on the hosts where the independent reproducer fails; a passing
suite does not rule out an intermittent crash.

The ordinary Windows CI job also records the hosted runner's exception context
with its existing test-failure artifact. This lets a future application crash
be compared with the independent probe without guessing which processor or
XSTATE configuration ran the suite.
