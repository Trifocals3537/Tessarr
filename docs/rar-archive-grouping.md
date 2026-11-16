# Independent RAR archives and multipart assembly

NZB parsing preserves its filename-derived archive groups. Having several groups
with one entry no longer causes them to be merged before their contents are
validated. Complete independent RAR4/RAR5 archives can therefore coexist without
being interpreted as conflicting parts of one archive.

Archive processing first handles every group independently. A successful group
is preserved. Only a failed one-entry RAR group carrying the parser's private
incomplete-volume error can enter a multipart fallback. RAR5 supplies this
evidence after CRC-validated (and, if needed, decrypted) multivolume metadata
requires earlier or later volumes. RAR4 supplies it after CRC-validated headers
reach an orphan continuation or missing-final-member error. Error text is never
used to identify candidates.

Candidates are separated by format. At most one assembly attempt per format is
made, after the first processing pass has finished, using the existing connection
budget and processing context. The retry owns its file/segment slices, reads its
own geometry, and passes through the ordinary archive parser. Header CRC,
RAR5 indices/end markers, split chains, declared sizes, segment mapping and the
later import-availability checks remain active. Original failures are consumed
only if that attempt produces valid files. A failed attempt retains both its
cause and the original group errors, including cancellation.

Provider, password, CRC, geometry and unsupported-format failures do not request
this fallback. Complete archives never enter it. RAR4 candidates with identical
effective filename-order/file-number positions are ambiguous and rejected rather
than ordered by map iteration. RAR5 continues to use its validated embedded
indices. The separate RAR4 mixed-name ordering issue #49 is unchanged.

This is structural archive validation, not a new archive-identity identifier or
a complete payload/availability audit. The parser does not search combinations
to partition several incomplete archive sets; conflicting candidates may still
fail that single assembly attempt. Groups with multiple entries retain their
existing behavior, including the bounded split-group diagnostic from #24.

Logical output names must still pass the separate portable-name collision gate.
The same internal name can be represented in separate parser groups, but final
processing rejects files resolving to the same output path. This fix does not
silently overwrite or rename those files. Independent archives with distinct
logical member names keep those names and their article provenance.

Synthetic TLS loopback tests cover standalone archives and multipart mixtures,
opaque subject/yEnc names, plain and encrypted RAR5 headers, format separation,
fragmented articles, additional complete members, missing/duplicate/corrupt or
conflicting volumes, ambiguous legacy order, password/error exclusions,
cancellation, and import-availability failure after successful assembly. Complete
controls exercise full and random readback after metadata serialization. No live
providers, releases, queues or media are involved.
