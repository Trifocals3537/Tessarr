# RAR5 physical data-area alignment

[Issue #32](https://github.com/Trifocals3537/tessarr/issues/32) reproduced a valid
65-byte unencrypted stored member failing after an encrypted header. The parser
rounded its data size to 80 bytes, consuming 15 bytes of the following header IV.

The encrypted-header path now skips the declared physical data-area size exactly,
as the plaintext-header path does. Header encryption does not imply file-data
encryption. For encrypted members, the packed size already includes ciphertext
padding; that padding remains part of the physical range. This also applies to
service headers with data areas. No unchecked size-rounding arithmetic remains
in this path, and the existing bounded skip still rejects unavailable data.

The [RAR5 format](https://www.rarlab.com/technote.htm) defines the data-area size
as the packed file size and separately defines block alignment of encrypted
headers and file-encryption extra records.

Regression coverage uses public synthetic archives with correct header and
payload CRCs, password checks, independent header/file IVs, and small KDF counts:

- Plain and encrypted members at 13 lengths from zero through 129 bytes,
  including both sides of AES and size-vint boundaries. Aligned encrypted
  controls retain their physical padding and logical size.
- Mixed plain/encrypted members, an empty member and an unaligned service data
  area, fragmented into 13-byte, 17-byte and single-article inputs through the
  real synthetic NNTP/parser/streaming-reader path.
- Exact payload/decryption readback, physical member EOF and bounded cross-member reads,
  archive-end positioning and an untouched following trailer.
- Every truncated prefix of the mixed archive returns an error and no usable
  partial parse result. Existing wrong-password and bad-header-CRC tests remain.
