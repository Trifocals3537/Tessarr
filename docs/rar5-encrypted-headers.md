# Encrypted RAR5 header validation

[Issue #21](https://github.com/Trifocals3537/tessarr/issues/21) reproduced three
reader defects: a missing main-header volume byte, a valid file header under-read
at an AES boundary, and acceptance of an incorrect encrypted-header CRC.

The [RAR5 format specification](https://www.rarlab.com/technote.htm#genblock)
defines the size value as the header body length. If that value is `H` and its
stored encoding occupies `L` bytes, the complete plaintext is `4 + L + H`
bytes. The encrypted read rounds that total up to an AES block boundary. CRC32
covers the stored size encoding and body; it excludes the CRC field and padding.

The encrypted reader uses one CBC decrypter across all header blocks, then
passes exactly the unpadded plaintext through the same CRC-checked reader used
for plaintext headers. Type, flags, optional sizes, and payload are interpreted
after the checksum passes. Padding cannot supply missing fields, and the caller
still receives the padded ciphertext count for stream positioning.

The existing 64 KiB body allocation cap is retained. The size encoding is
limited to the format's three-byte maximum, optional extra data must fit within
the body, data-area lengths must fit the signed offset representation, and
overflowing 64-bit vints are rejected. These checks apply to both encrypted and
plaintext headers.

## Regression coverage

All three original failures were reproduced in the actual parser package before
the change. The suite now covers:

- AES boundaries, one/two/three-byte sizes, reserved size-vint space, and the
  maximum supported header body.
- Non-zero padding, multibyte common fields, optional extra/data sizes, exact
  consumed counts, and preservation of following bytes.
- Every ciphertext truncation point in a multiblock fixture, invalid key/IV
  lengths, CRC damage, malformed fields, and integer bounds.
- A complete encrypted-header archive with a stored member, password checks,
  distinct per-header IVs, correct data offsets, and rejection of a damaged
  final header without returning partial success.
- Synthetic NNTP/yEnc article processing with headers split across 13-byte
  fragments, followed by reading back the exact stored member and checking EOF.

Fixtures contain only public synthetic passwords, keys, salts, names, and data.
The header fix provides validated bytes needed for subsequent volume-ordering
work in [#20](https://github.com/Trifocals3537/tessarr/issues/20).
