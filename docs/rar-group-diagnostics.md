# RAR filename-group diagnostics

Separately rejected RAR groups can now produce an additional warning such as:

```text
possible RAR chain split across filename groups: group 1 (14 volumes, .rNN) and group 2 (1 volume, .partNN.rar); basenames differ by whitespace and member boundaries are complementary; archive identity and completeness unverified; groups kept separate
```

The group numbers refer to `group_index` in the existing `Failed to process file
group` warnings for that NZB. Groups are numbered in basename order. The hint is
also included in the returned processing error when no group produces a valid
file. If another group succeeds, the existing partial-success policy remains in
effect and the hint is available in the log.

The hint requires two RAR groups of the same format with matching stored
(uncompressed) member names, declared sizes and encryption flags, but opposite
incomplete-chain boundaries. Compressed members are excluded: the parser's
`IsStored=false` flag does not retain their actual compression method, so it
cannot verify that those methods agree. Basenames must differ only by whitespace or the
separators dot, hyphen and underscore. This comparison is used only for the
diagnostic; it never changes the grouping key. Counts refer to NZB volume entries,
not proof that every article or byte is present.

RAR4 contributes typed evidence from its existing first-continuation and
missing-final-member errors. RAR5 validates volume order before aggregating member
parts. For an already rejected RAR5 group, the diagnostic can recognize a
contiguous prefix or suffix from the CRC-validated/decrypted volume metadata,
consistent continuation markers, and a corresponding member chain. Duplicate or
gapped indices, conflicting member metadata and ambiguous boundary members do
not supply this evidence. The original error text and wrapped causes are
preserved; error text is never used to infer relatedness.

Per NZB, at most 64 failed RAR groups are considered and three hints are emitted.
A group must have at most 256 volume entries, and RAR5 boundary inspection also
limits the total parsed member entries to 256. Basenames, volume filenames and
member names over 512 bytes are skipped rather than truncated into potential
matches. Only counts, group numbers and fixed descriptions appear in the new
message. Existing per-group warnings retain their original errors and names.

This diagnoses the two naming patterns recorded in issue #24, including
`Demo-Release.part01.rar` beside `Demo - Release.r00` and `Archive.Name.part01.rar`
beside `Archive Name.r00`. It does not establish common archive identity or
recoverability, fetch more provider articles, retry, or reconcile groups. The
pre-existing obfuscated-single-volume grouping heuristic is unchanged. Existing
header CRC, split-chain, declared-size, segment-geometry and import-availability
checks still decide admission. A genuinely incomplete set continues to reject.

Synthetic loopback NNTP tests exercise both reported patterns with the first
volume last in NZB order, a one-volume/fourteen-volume partition, RAR4 and plain
or encrypted RAR5 headers. Complete controls include full and random byte
readback. Separate valid archives, mixed success/failure, missing/duplicate/corrupt
volumes, conflicting metadata, bounded output and original error propagation are
covered without live providers, releases or media. Synthetic CRC-valid headers
using incompatible compressed methods verify that neither RAR4 nor RAR5 emits
the hint for unverified compressed members.
