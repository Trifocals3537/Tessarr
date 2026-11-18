# RAR5 volume ordering

RAR5 archive admission uses the volume indices in CRC-validated main headers,
including decrypted main headers, before validating a member's split chain.
Filename suffixes and NZB file order no longer decide the order of a RAR5 set.
RAR4 retains its existing filename-based order.

The [RAR5 main-header specification](https://www.rarlab.com/technote.htm#mainhead)
defines an omitted number for the first volume and positive numbers starting at
1 for continuations. The parser requires exactly one occurrence of every index
from 0 through the admitted volume count minus one. Multiple volumes must carry
the multivolume flag, and their header encryption mode must agree. Duplicate,
missing, contradictory or out-of-range metadata is rejected without falling back
to potentially misleading filenames. An explicit zero continuation number or a
number without the multivolume flag is invalid.

The [end-header continuation flag](https://www.rarlab.com/technote.htm#endarchead)
must identify every nonterminal volume and be clear for the last. A missing or
duplicate main header, truncated number, inconsistent end marker or invalid
header CRC rejects the set. Main-header extra records are kept outside the
required field reader, so they cannot supply a missing number. Password checks,
KDF limits and the common CRC/decryption path remain active.

Member volume indices and part numbers are remapped before aggregation. The flat
article map is then assembled in that same order from the original validated
volume descriptors. It does not recompute geometry from the reordered filename
positions, which would attach legacy final-volume sizing to the wrong file.
Original article provenance and offsets remain attached to their posted files;
only the member's output coordinates change. Combined size arithmetic and
descriptor coverage are checked before building the output map.

Existing split-chain, declared-size, member-range, article-geometry and import
availability checks still apply. Ordering does not prove that every later article
is available or validate a complete media payload during admission. Streaming
continues to validate fetched article bodies. This change does not merge separate
file groups, infer archive identity across basenames, migrate stored NZB metadata,
or reconcile the cross-group naming case in issue #24. The separate
[filename-group diagnostic](rar-group-diagnostics.md) can now explain some
complementary failures without combining those groups.

Synthetic tests reproduce the 13-volume filename/header permutation from #20
with plain and encrypted headers. They cover complete NZB admission, fragmented
articles, full and random readback after metadata serialization, encrypted data
split at non-AES-aligned volume boundaries, legacy geometry, invalid sequences,
CRC/password failures, extra-area boundaries and size overflows. The older valid
two-volume fixture now includes the required continuation number and end flag.
No live release, provider content, deployment or publication is part of these
checks.
