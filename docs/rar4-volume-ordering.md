# RAR4 mixed volume naming

Recognized labels now share zero-based positions across both naming conventions:

| Position | Old naming | Part naming |
| --- | --- | --- |
| 0 | `Demo.rar` | `Demo.part01.rar` |
| 1 | `Demo.r00` | `Demo.part02.rar` |
| 2 | `Demo.r01` | `Demo.part03.rar` |

Consequently, `Demo.part01.rar` sorts before `Demo.r00` even when the first volume
appears last in NZB order or counters repeat. Mixed continuations follow the
same positions. Matching is case insensitive and accepts positive part numbers
with or without leading zeros.

Numeric `.partNN.rar` suffixes must be positive and fit the platform's integer
range; legacy `.rNN` suffixes contain exactly two ASCII digits. Zero/overflowing
part numbers and malformed legacy suffixes receive no recognized position. As
with other opaque labels, they sort after recognized positions and retain the
existing NZB-number fallback. Ordering never allocates by an untrusted number.

RAR4 cannot independently validate embedded volume indices in this parser.
Distinct recognized aliases for the same basename and position, such as
`Demo.r00` and `Demo.part02.rar`, are therefore rejected as ambiguous instead of
being ordered by NZB counters. Literal repeated content-detected names and
different opaque basenames retain their existing upload-order behavior. Original
header CRC, split-chain, declared-size, geometry and import checks still apply.

RAR5 uses its CRC-validated/decrypted embedded indices and end markers. The
RAR4-only alias rejection never overrides that order, including when its labels
conflict. Independent groups and the incomplete-volume fallback from #48 retain
their existing behavior; the label change does not merge groups or bypass final
output-name collision checks.

Synthetic loopback tests reproduce #49, exercise all 24 permutations of a mixed
four-label set, and check complete admission plus full/random byte readback after
metadata serialization. Coverage includes both naming styles, mixed first and
continuation names, uppercase/zero padding, reversed documents and repeated
counters, RAR5 plain/encrypted headers, conflicting aliases, opaque names,
missing/duplicate/corrupt volumes and conflicting sizes. No live providers,
media, queues, deployment or publication are involved.
